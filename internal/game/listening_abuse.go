package game

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var listeningAbuseMu sync.Mutex

const listeningAbuseRetryInterval = 5 * time.Minute

func RunListeningAbuseMonitorIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	if !listeningAbuseMu.TryLock() {
		return
	}
	defer listeningAbuseMu.Unlock()

	releaseDueListeningAbuseFreezes(bot, now)

	targetDay := listeningAbuseScanDayKey(now)
	if targetDay == "" {
		return
	}
	lastScanDay, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
	if err != nil {
		recordListeningAbuseStateReadFailure(bot, storage.ListeningAbuseLastScanDayKey, err)
		return
	}
	if lastScanDay == targetDay {
		return
	}
	lastAttempt, err := storage.GetSystemConfigTimeChecked(storage.ListeningAbuseLastAttemptAtKey)
	if err != nil {
		recordListeningAbuseStateReadFailure(bot, storage.ListeningAbuseLastAttemptAtKey, err)
		return
	}
	if age := now.Sub(lastAttempt); !lastAttempt.IsZero() && age >= 0 && age < listeningAbuseRetryInterval {
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastAttemptAtKey, now.Format(time.RFC3339)); err != nil {
		recordListeningAbuseStateWriteFailure(bot, storage.ListeningAbuseLastAttemptAtKey, err)
		return
	}

	warnings, freezes, skipped, err := scanListeningAbuseForDay(bot, targetDay, now)
	if err != nil {
		storage.SetSystemConfigError(storage.ListeningAbuseLastErrorKey, err)
		log.Printf("listening abuse scan failed: day=%s err=%s", textfmt.FormatPlainValue(targetDay), textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("播放异常巡检失败\n\n日期: %s\n错误: %s", textfmt.FormatPlainValue(targetDay), textfmt.FormatPlainError(err)))
		return
	}

	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey, targetDay); err != nil {
		recordListeningAbuseStateWriteFailure(bot, storage.ListeningAbuseLastScanDayKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastScanAtKey, time.Now().Format(time.RFC3339)); err != nil {
		recordListeningAbuseStateWriteFailure(bot, storage.ListeningAbuseLastScanAtKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastErrorKey, ""); err != nil {
		recordListeningAbuseStateWriteFailure(bot, storage.ListeningAbuseLastErrorKey, err)
		return
	}
	log.Printf("listening abuse scan finished: day=%s warnings=%d freezes=%d skipped=%d", textfmt.FormatPlainValue(targetDay), warnings, freezes, skipped)
}

func listeningAbuseScanDayKey(now time.Time) string {
	localNow := storage.DailyOperationsLocalTime(now)
	if localNow.Hour() == 0 && localNow.Minute() < 10 {
		return ""
	}
	return localNow.AddDate(0, 0, -1).Format("2006-01-02")
}

func recordListeningAbuseStateReadFailure(bot *tgbotapi.BotAPI, key string, err error) {
	storage.SetSystemConfigError(storage.ListeningAbuseLastErrorKey, err)
	log.Printf("播放异常风控状态读取失败，已跳过本轮巡检: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("播放异常风控状态读取失败，已跳过本轮巡检。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func recordListeningAbuseStateWriteFailure(bot *tgbotapi.BotAPI, key string, err error) {
	if key != storage.ListeningAbuseLastErrorKey {
		storage.SetSystemConfigError(storage.ListeningAbuseLastErrorKey, err)
	}
	log.Printf("播放异常风控状态写入失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("播放异常风控状态写入失败，请人工核查；相关巡检已中止或完成状态未确认。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func previousDayKey(dayKey string) (string, bool) {
	day, err := time.ParseInLocation("2006-01-02", dayKey, storage.DailyOperationsLocation)
	if err != nil {
		return "", false
	}
	return day.AddDate(0, 0, -1).Format("2006-01-02"), true
}

func listeningAbuseShouldWarn(rawHours float64) bool {
	return rawHours > storage.ListeningAbuseDailyRawHoursThreshold
}

func listeningAbuseShouldFreeze(rawHours float64, previousRawHours float64) bool {
	return rawHours > storage.ListeningAbuseDailyRawHoursThreshold && previousRawHours > storage.ListeningAbuseDailyRawHoursThreshold
}

func scanListeningAbuseForDay(bot *tgbotapi.BotAPI, dayKey string, now time.Time) (int, int, int, error) {
	previousKey, ok := previousDayKey(dayKey)
	if !ok {
		return 0, 0, 0, fmt.Errorf("invalid_day_key")
	}

	var users []model.User
	if err := storage.DB.Select("telegram_id", "username", "abs_user_id", "expire_at", "is_suspended", "is_whitelist", "role", "status").
		Where("abs_user_id <> '' AND is_whitelist = ? AND role <> ?", false, "super_admin").
		Where("status = ?", "active").
		Find(&users).Error; err != nil {
		return 0, 0, 0, err
	}

	warnings := 0
	freezes := 0
	skipped := 0
	pending := 0
	for _, u := range users {
		if u.IsSuspended || storage.IsUserExpiredAt(u, now) {
			skipped++
			continue
		}

		// A retry must not repeat successful warnings/freezes. These records
		// survive restarts and are written atomically with their audits.
		var processed int64
		if err := storage.DB.Model(&model.ListeningAbuseRecord{}).
			Where("user_id = ? AND day_key = ? AND action IN ?", u.TelegramID, dayKey,
				[]string{storage.ListeningAbuseActionWarning, storage.ListeningAbuseActionFreeze}).
			Count(&processed).Error; err != nil {
			log.Printf("listening abuse processed record read failed: user=%d day=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(err))
			pending++
			skipped++
			continue
		}
		if processed > 0 {
			continue
		}

		// The normal five-minute cache contains only today's value. A scan
		// needs fresh complete days for yesterday and the day before.
		days, refreshed := refreshDailyListeningStatsFromABS(u.TelegramID, u.AbsUserID, true, dayKey, previousKey)
		time.Sleep(storage.DailyListeningRefreshRequestDelay)
		if !refreshed {
			pending++
			skipped++
			continue
		}
		rawHours := abs.PositiveListeningSeconds(days[dayKey]) / 3600
		if !listeningAbuseShouldWarn(rawHours) {
			continue
		}
		previousRawHours := abs.PositiveListeningSeconds(days[previousKey]) / 3600
		if listeningAbuseShouldFreeze(rawHours, previousRawHours) {
			started, err := startListeningAbuseFreeze(bot, u, dayKey, previousKey, rawHours, previousRawHours, now)
			if err != nil {
				pending++
				log.Printf("listening abuse freeze failed: user=%d day=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(err))
				writeListeningAbuseFailureAudit(bot, "LISTENING_ABUSE_FREEZE_FAILED", u.TelegramID, "播放异常自动冻结失败",
					fmt.Sprintf("failed to freeze abnormal listening user; day=%s raw_hours=%.2f previous_day=%s previous_raw_hours=%.2f error=%s",
						textfmt.FormatPlainValue(dayKey), rawHours, textfmt.FormatPlainValue(previousKey), previousRawHours, textfmt.FormatPlainError(err)))
				continue
			}
			if started {
				freezes++
			}
			continue
		}

		warned, err := createListeningAbuseWarning(bot, u, dayKey, rawHours, now)
		if err != nil {
			pending++
			log.Printf("listening abuse warning failed: user=%d day=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(err))
			continue
		}
		if warned {
			warnings++
		}
	}

	if pending > 0 {
		return warnings, freezes, skipped, fmt.Errorf("LISTENING_ABUSE_SCAN_PENDING: %d users require retry", pending)
	}
	return warnings, freezes, skipped, nil
}

func createListeningAbuseRecordInTx(tx *gorm.DB, record *model.ListeningAbuseRecord) (bool, error) {
	if tx == nil || record == nil {
		return false, fmt.Errorf("LISTENING_ABUSE_RECORD_INVALID")
	}
	entry := *record
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	*record = entry
	return true, nil
}

func createListeningAbuseWarning(bot *tgbotapi.BotAPI, u model.User, dayKey string, rawHours float64, now time.Time) (bool, error) {
	record := model.ListeningAbuseRecord{
		UserID:    u.TelegramID,
		AbsUserID: u.AbsUserID,
		DayKey:    dayKey,
		RawHours:  rawHours,
		Action:    storage.ListeningAbuseActionWarning,
		Status:    storage.ListeningAbuseStatusWarningSent,
	}

	if err := storage.DB.Transaction(func(tx *gorm.DB) error {
		created, err := createListeningAbuseRecordInTx(tx, &record)
		if err != nil {
			return err
		}
		if !created {
			return nil
		}
		return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_WARNING", strconv.FormatInt(u.TelegramID, 10), 0,
			fmt.Sprintf("abnormal listening warning; record_id=%d day=%s raw_hours=%.2f threshold=%.2f",
				record.ID, dayKey, rawHours, storage.ListeningAbuseDailyRawHoursThreshold))
	}); err != nil {
		return false, err
	}
	if record.ID == 0 {
		return false, nil
	}

	text := fmt.Sprintf("⚠️ **播放时长异常提醒**\n\n系统检测到您在北京时间 `%s` 的 可证明的 ABS 播放时长至少为 `%.2f` 小时，已超过 `%g` 小时。\n\n若连续两个自然日超过该阈值，系统将自动暂停听书账号 `24` 小时。请确认播放器、自动播放或多设备播放状态是否正常。",
		dayKey, rawHours, storage.ListeningAbuseDailyRawHoursThreshold)
	noticeErr := sendListeningAbusePrivateNotice(bot, u.TelegramID, text)
	if noticeErr != nil {
		recordListeningAbuseNoticeError(record.ID, u.TelegramID, textfmt.FormatPlainError(noticeErr))
	}
	return true, nil
}

func startListeningAbuseFreeze(bot *tgbotapi.BotAPI, u model.User, dayKey string, previousDayKey string, rawHours float64, previousRawHours float64, now time.Time) (bool, error) {
	if err := abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, false); err != nil {
		return false, err
	}

	freezeEnd := now.Add(time.Duration(storage.ListeningAbuseFreezeHours) * time.Hour)
	record := model.ListeningAbuseRecord{
		UserID:           u.TelegramID,
		AbsUserID:        u.AbsUserID,
		DayKey:           dayKey,
		PreviousDayKey:   previousDayKey,
		RawHours:         rawHours,
		PreviousRawHours: previousRawHours,
		Action:           storage.ListeningAbuseActionFreeze,
		Status:           storage.ListeningAbuseStatusActive,
		FreezeStartAt:    &now,
		FreezeEndAt:      &freezeEnd,
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var activeCount int64
		if err := tx.Model(&model.ListeningAbuseRecord{}).
			Where("user_id = ? AND action = ? AND status = ? AND deleted_at IS NULL", u.TelegramID, storage.ListeningAbuseActionFreeze, storage.ListeningAbuseStatusActive).
			Count(&activeCount).Error; err != nil {
			return err
		}
		if activeCount > 0 {
			return nil
		}
		created, err := createListeningAbuseRecordInTx(tx, &record)
		if err != nil {
			return err
		}
		if !created {
			return nil
		}
		return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_FREEZE", strconv.FormatInt(u.TelegramID, 10), 0,
			fmt.Sprintf("auto freeze abnormal listening user; day=%s raw_hours=%.2f previous_day=%s previous_raw_hours=%.2f freeze_until=%s",
				dayKey, rawHours, previousDayKey, previousRawHours, freezeEnd.Format(time.RFC3339)))
	})
	if err != nil {
		if reactivateErr := abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, true); reactivateErr != nil {
			log.Printf("listening abuse freeze rollback failed: user=%d abs=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(reactivateErr))
		}
		return false, err
	}
	if record.ID == 0 {
		return false, nil
	}

	privateText := fmt.Sprintf("⛔ **账号已临时暂停**\n\n系统检测到您连续两个北京时间自然日可证明的播放时长均超过 `%g` 小时：\n`%s`：`%.2f` 小时\n`%s`：`%.2f` 小时\n\n您的听书账号已暂停 `24` 小时，预计恢复时间：`%s`。\n\n本次仅暂停收听权限，不扣除积分、贡献或修为。请检查播放器自动播放、多设备播放或未关闭播放的问题。",
		storage.ListeningAbuseDailyRawHoursThreshold, previousDayKey, previousRawHours, dayKey, rawHours, storage.DailyOperationsLocalTime(freezeEnd).Format("2006-01-02 15:04"))
	noticeErr := sendListeningAbusePrivateNotice(bot, u.TelegramID, privateText)
	groupErr := sendListeningAbuseGroupFreezeNotice(bot, u, dayKey, rawHours, previousDayKey, previousRawHours, freezeEnd)
	if noticeErr != nil || groupErr != nil {
		recordListeningAbuseNoticeError(record.ID, u.TelegramID, fmt.Sprintf("private=%s group=%s", textfmt.FormatPlainError(noticeErr), textfmt.FormatPlainError(groupErr)))
	}
	return true, nil
}

func releaseDueListeningAbuseFreezes(bot *tgbotapi.BotAPI, now time.Time) {
	var records []model.ListeningAbuseRecord
	if err := storage.DB.Where("action = ? AND status = ? AND freeze_end_at IS NOT NULL AND freeze_end_at <= ?",
		storage.ListeningAbuseActionFreeze, storage.ListeningAbuseStatusActive, now).
		Find(&records).Error; err != nil {
		log.Printf("query due listening abuse freezes failed: err=%s", textfmt.FormatPlainError(err))
		return
	}

	for _, record := range records {
		if err := releaseListeningAbuseFreeze(bot, record, now); err != nil {
			log.Printf("release listening abuse freeze failed: record=%d user=%d err=%s", record.ID, record.UserID, textfmt.FormatPlainError(err))
		}
	}
}

func releaseListeningAbuseFreeze(bot *tgbotapi.BotAPI, record model.ListeningAbuseRecord, now time.Time) error {
	var u model.User
	if err := storage.DB.Where("telegram_id = ?", record.UserID).First(&u).Error; err != nil {
		return err
	}

	if u.AbsUserID != record.AbsUserID || u.IsSuspended || storage.IsUserExpiredAt(u, now) {
		reason := fmt.Sprintf("release blocked; abs_changed=%t suspended=%t expired=%t",
			u.AbsUserID != record.AbsUserID, u.IsSuspended, storage.IsUserExpiredAt(u, now))
		return markListeningAbuseFreezeReleaseBlocked(record.ID, record.UserID, now, reason)
	}

	if err := abs.DefaultClient.SetUserActiveStatus(record.AbsUserID, true); err != nil {
		recordListeningAbuseReleaseError(record.ID, record.UserID, err)
		writeListeningAbuseFailureAudit(bot, "LISTENING_ABUSE_RELEASE_FAILED", record.UserID, "播放异常临停到期恢复失败",
			fmt.Sprintf("failed to release abnormal listening freeze; record_id=%d error=%s", record.ID, textfmt.FormatPlainError(err)))
		return err
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.ListeningAbuseRecord{}).
			Where("id = ? AND status = ?", record.ID, storage.ListeningAbuseStatusActive).
			Updates(map[string]interface{}{
				"status":        storage.ListeningAbuseStatusReleased,
				"released_at":   now,
				"release_error": "",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_RELEASE", strconv.FormatInt(record.UserID, 10), 0,
			fmt.Sprintf("released abnormal listening freeze; record_id=%d freeze_end=%s", record.ID, now.Format(time.RFC3339)))
	})
	if err != nil {
		return err
	}

	_ = sendListeningAbusePrivateNotice(bot, record.UserID, "✅ **账号暂停已解除**\n\n本次播放异常暂停已满 `24` 小时，系统已恢复您的 ABS 听书权限。请保持正常收听，避免连续长时间自动播放再次触发风控。")
	return nil
}

func recordListeningAbuseReleaseError(recordID uint, userID int64, releaseErr error) {
	if releaseErr == nil {
		return
	}
	res := storage.DB.Model(&model.ListeningAbuseRecord{}).
		Where("id = ?", recordID).
		Update("release_error", textfmt.FormatPlainError(releaseErr))
	if res.Error != nil {
		log.Printf("listening abuse release error persistence failed: record=%d user=%d err=%s", recordID, userID, textfmt.FormatPlainError(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("listening abuse release error persistence missed record: record=%d user=%d", recordID, userID)
	}
}

func writeListeningAbuseFailureAudit(bot *tgbotapi.BotAPI, action string, userID int64, label string, detail string) {
	if err := storage.WriteAuditLogInTx(storage.DB, 0, action, strconv.FormatInt(userID, 10), 0, detail); err != nil {
		log.Printf("listening abuse failure audit write failed: action=%s user=%d err=%s", textfmt.FormatPlainValue(action), userID, textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ %s，且失败审计写入失败。\n用户：%d\n审计动作：%s\n审计错误：%s", textfmt.FormatPlainValue(label), userID, textfmt.FormatPlainValue(action), textfmt.FormatPlainError(err)))
	}
}

func recordListeningAbuseNoticeError(recordID uint, userID int64, noticeText string) {
	noticeText = strings.TrimSpace(textfmt.FormatPlainValue(noticeText))
	if noticeText == "" {
		return
	}
	res := storage.DB.Model(&model.ListeningAbuseRecord{}).
		Where("id = ?", recordID).
		Update("notice_error", noticeText)
	if res.Error != nil {
		log.Printf("listening abuse notice error persistence failed: record=%d user=%d err=%s", recordID, userID, textfmt.FormatPlainError(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("listening abuse notice error persistence missed record: record=%d user=%d", recordID, userID)
	}
}

func markListeningAbuseFreezeReleaseBlocked(recordID uint, userID int64, now time.Time, reason string) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.ListeningAbuseRecord{}).
			Where("id = ? AND status = ?", recordID, storage.ListeningAbuseStatusActive).
			Updates(map[string]interface{}{
				"status":        storage.ListeningAbuseStatusReleaseBlocked,
				"released_at":   now,
				"release_error": reason,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_RELEASE_BLOCKED", strconv.FormatInt(userID, 10), 0,
			fmt.Sprintf("abnormal listening freeze release blocked; record_id=%d reason=%s", recordID, textfmt.FormatPlainValue(reason)))
	})
}

func sendListeningAbusePrivateNotice(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	if bot == nil {
		return fmt.Errorf("bot_empty")
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, err := telegram.SendNoAutoDelete(bot, msg)
	return err
}

func sendListeningAbuseGroupFreezeNotice(bot *tgbotapi.BotAPI, u model.User, dayKey string, rawHours float64, previousDayKey string, previousRawHours float64, freezeEnd time.Time) error {
	if bot == nil || config.AppConfig == nil || config.AppConfig.NoticeGroupID == 0 {
		return nil
	}

	name := textfmt.EscapeMarkdown(u.Username)
	if name == "" {
		name = strconv.FormatInt(u.TelegramID, 10)
	}
	text := fmt.Sprintf("⚠️ **播放异常风控**\n\n道友 `%s` 因连续两天可证明的播放时长超限，听书账号已临时暂停 `24` 小时。\n\n`%s`：`%.2f` 小时\n`%s`：`%.2f` 小时\n预计恢复：`%s`",
		name, previousDayKey, previousRawHours, dayKey, rawHours, storage.DailyOperationsLocalTime(freezeEnd).Format("2006-01-02 15:04"))
	msg := tgbotapi.NewMessage(config.AppConfig.NoticeGroupID, text)
	msg.ParseMode = "Markdown"
	_, err := telegram.SendNoAutoDelete(bot, msg)
	return err
}

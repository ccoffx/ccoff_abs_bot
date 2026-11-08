package game

import (
	"encoding/json"
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
	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var listeningAbuseMu sync.Mutex

const listeningAbuseRetryInterval = 5 * time.Minute
const listeningAbuseFailureNoticeInterval = time.Hour

type listeningAbuseFailureNotice struct {
	Day        string              `json:"day"`
	Error      string              `json:"error"`
	At         time.Time           `json:"at,omitzero"` // Legacy all-recipient receipt.
	Recipients map[int64]time.Time `json:"recipients,omitempty"`
}

type listeningAbuseFailureNotices struct {
	Notices []listeningAbuseFailureNotice `json:"notices"`
}

func RunListeningAbuseMonitorIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	started := time.Now()
	runListeningAbuseMonitorIfNeeded(bot, func() time.Time {
		return now.Add(time.Since(started))
	}, func(adminID int64, text string) bool {
		return telegram.NotifySuperAdminPlain(bot, adminID, text)
	})
}

func runListeningAbuseMonitorIfNeeded(bot *tgbotapi.BotAPI, currentTime func() time.Time, enqueue func(int64, string) bool) {
	if !listeningAbuseMu.TryLock() {
		return
	}
	defer listeningAbuseMu.Unlock()
	now := currentTime()

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
	scanDay, err := nextListeningAbuseScanDay(lastScanDay, targetDay)
	if err != nil {
		recordListeningAbuseStateReadFailure(bot, storage.ListeningAbuseLastScanDayKey, err)
		return
	}
	if scanDay == "" {
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

	warnings, freezes, skipped, err := scanListeningAbuseForDay(bot, scanDay, now)
	if err != nil {
		storage.SetSystemConfigError(storage.ListeningAbuseLastErrorKey, err)
		log.Printf("listening abuse scan failed: day=%s err=%s", textfmt.FormatPlainValue(scanDay), textfmt.FormatPlainError(err))
		// Scanning all accounts may take a long time. The hourly notice
		// interval starts when the failure is queued, not when scanning began.
		notifyListeningAbuseScanFailure(scanDay, err, currentTime(), enqueue)
		return
	}

	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey, scanDay); err != nil {
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
	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey, ""); err != nil {
		log.Printf("播放异常巡检告警去重状态清理失败: err=%s", textfmt.FormatPlainError(err))
	}
	log.Printf("listening abuse scan finished: day=%s warnings=%d freezes=%d skipped=%d", textfmt.FormatPlainValue(scanDay), warnings, freezes, skipped)
}

// Called under listeningAbuseMu. Persist receipts per recipient so a partially
// full queue does not turn retries into duplicate messages for other admins.
func notifyListeningAbuseScanFailure(day string, scanErr error, now time.Time, enqueue func(int64, string) bool) {
	if config.AppConfig == nil || enqueue == nil {
		return
	}
	failure := textfmt.FormatPlainError(scanErr)
	raw, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey)
	if err != nil {
		log.Printf("播放异常巡检告警去重状态读取失败: err=%s", textfmt.FormatPlainError(err))
	}
	var state listeningAbuseFailureNotices
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			state = listeningAbuseFailureNotices{}
		} else if state.Notices == nil {
			// Both earlier formats stored only the latest error, with either an
			// all-recipient timestamp or individual recipient receipts.
			var legacy listeningAbuseFailureNotice
			if json.Unmarshal([]byte(raw), &legacy) == nil && legacy.Day != "" {
				state.Notices = append(state.Notices, legacy)
			}
		}
	}
	previous := listeningAbuseFailureNotice{Day: day, Error: failure, Recipients: make(map[int64]time.Time)}
	retained := make([]listeningAbuseFailureNotice, 0, len(state.Notices)+1)
	for _, notice := range state.Notices {
		if notice.Recipients == nil {
			notice.Recipients = make(map[int64]time.Time)
			// Earlier versions wrote At only after every admin was queued.
			if !notice.At.IsZero() {
				for adminID := range config.AppConfig.AdminIDs {
					notice.Recipients[adminID] = notice.At
				}
			}
		}
		notice.At = time.Time{}
		for adminID, at := range notice.Recipients {
			if at.IsZero() || now.Sub(at) >= listeningAbuseFailureNoticeInterval {
				delete(notice.Recipients, adminID)
			}
		}
		if notice.Day == day && notice.Error == failure {
			previous = notice
		} else if len(notice.Recipients) > 0 {
			retained = append(retained, notice)
		}
	}
	text := fmt.Sprintf("播放异常巡检失败\n\n日期: %s\n错误: %s\n\n系统每 5 分钟自动重试；同一天的相同错误每小时最多提醒一次。", textfmt.FormatPlainValue(day), failure)
	changed := false
	for adminID := range config.AppConfig.AdminIDs {
		if at := previous.Recipients[adminID]; !at.IsZero() {
			if age := now.Sub(at); age >= 0 && age < listeningAbuseFailureNoticeInterval {
				continue
			}
		}
		if enqueue(adminID, text) {
			previous.Recipients[adminID] = now
			changed = true
		}
	}
	if !changed {
		return
	}
	// Keep all unexpired errors, rather than only the last one. Changing
	// pending-user counts can otherwise bypass the throttle on every retry.
	state.Notices = append(retained, previous)
	encoded, err := json.Marshal(state)
	if err == nil {
		err = storage.SetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey, string(encoded))
	}
	if err != nil {
		log.Printf("播放异常巡检告警去重状态写入失败: err=%s", textfmt.FormatPlainError(err))
	}
}

func nextListeningAbuseScanDay(lastDay, targetDay string) (string, error) {
	if lastDay == "" {
		return targetDay, nil
	}
	last, err := time.ParseInLocation("2006-01-02", lastDay, storage.DailyOperationsLocation)
	if err != nil || last.Format("2006-01-02") != lastDay {
		return "", fmt.Errorf("invalid last listening abuse scan day %q", lastDay)
	}
	target, err := time.ParseInLocation("2006-01-02", targetDay, storage.DailyOperationsLocation)
	if err != nil || target.Format("2006-01-02") != targetDay {
		return "", fmt.Errorf("invalid target listening abuse scan day %q", targetDay)
	}
	if !last.Before(target) {
		return "", nil
	}
	return last.AddDate(0, 0, 1).Format("2006-01-02"), nil
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
		if storage.IsSuperAdminUser(u) || u.IsSuspended || storage.IsUserExpiredAt(u, now) {
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
		// Recheck immunity under the writer lock in case the role or whitelist
		// changed while ABS statistics were being fetched.
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", u.TelegramID).Error; err != nil {
			return err
		}
		var current model.User
		if err := tx.First(&current, "telegram_id = ?", u.TelegramID).Error; err != nil {
			return err
		}
		if current.IsWhitelist || storage.IsSuperAdminUser(current) {
			return nil
		}
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
		// Serialize eligibility, the freeze record and ABS suspension with every
		// account writer. Never compensate a failed local write by enabling ABS.
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", u.TelegramID).Error; err != nil {
			return err
		}
		var current model.User
		if err := tx.First(&current, "telegram_id = ?", u.TelegramID).Error; err != nil {
			return err
		}
		if current.AbsUserID != u.AbsUserID || current.Status != "active" || current.IsSuspended || current.AdminSuspended || current.IsWhitelist || storage.IsSuperAdminUser(current) || storage.IsUserExpiredAt(current, now) {
			return nil
		}
		var claims int64
		if err := tx.Model(&model.AccountLifecycleClaim{}).Where("user_row_id = ?", current.ID).Count(&claims).Error; err != nil {
			return err
		}
		if claims > 0 {
			return fmt.Errorf("账号生命周期操作处理中，请稍后重试")
		}

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
		if err := storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_FREEZE", strconv.FormatInt(u.TelegramID, 10), 0,
			fmt.Sprintf("auto freeze abnormal listening user; day=%s raw_hours=%.2f previous_day=%s previous_raw_hours=%.2f freeze_until=%s",
				dayKey, rawHours, previousDayKey, previousRawHours, freezeEnd.Format(time.RFC3339))); err != nil {
			return err
		}
		if abs.DefaultClient == nil {
			return fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
		}
		return abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, false)
	})
	if err != nil {
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
	released, err := lifecycle.ReleaseListeningAbuseFreeze(record.ID, now)
	if err != nil {
		recordListeningAbuseReleaseError(record.ID, record.UserID, err)
		writeListeningAbuseFailureAudit(bot, "LISTENING_ABUSE_RELEASE_FAILED", record.UserID, "播放异常临停到期恢复失败",
			fmt.Sprintf("failed to release abnormal listening freeze; record_id=%d error=%s", record.ID, textfmt.FormatPlainError(err)))
		return err
	}
	if !released {
		return nil
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

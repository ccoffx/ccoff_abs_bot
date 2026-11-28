package game

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

const (
	activityEntryBoss         = "boss"
	activityEntryRealm        = "realm"
	activityEntryPendingLimit = 512
	activityEntryEventLimit   = 128
	activityEntryTimeout      = 2 * time.Minute
	activityEntryMaxAttempts  = 3
)

var (
	activityEntryWorkers  = background.New(4, 128)
	activityEntryDispatch = background.New(1, 1)
	activityEntryReceipts = background.New(2, 128)
	activityEntryMessages = background.New(2, 256)
	activityEntryTurns    = struct {
		sync.Mutex
		ids    map[uint]bool
		groups map[string]int
	}{ids: make(map[uint]bool), groups: make(map[string]int)}
)

func activityEntryGroup(r model.ActivityEntryRequest) string {
	if r.Kind == activityEntryBoss {
		return "world_boss_events:" + r.EventID
	}
	return "sect_secret_realm_events:" + r.EventID
}

func startActivityEntryTurn(r model.ActivityEntryRequest) (func(), bool) {
	activityEntryTurns.Lock()
	defer activityEntryTurns.Unlock()
	group := activityEntryGroup(r)
	if activityEntryTurns.ids[r.ID] || len(activityEntryTurns.ids) >= 128 || activityEntryTurns.groups[group] >= 3 {
		return nil, false
	}
	activityEntryTurns.ids[r.ID], activityEntryTurns.groups[group] = true, activityEntryTurns.groups[group]+1
	return func() {
		activityEntryTurns.Lock()
		delete(activityEntryTurns.ids, r.ID)
		activityEntryTurns.groups[group]--
		if activityEntryTurns.groups[group] == 0 {
			delete(activityEntryTurns.groups, group)
		}
		activityEntryTurns.Unlock()
	}, true
}

func StartActivityEntryScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			kickActivityEntries(bot)
			<-ticker.C
		}
	}()
}

func kickActivityEntries(bot *tgbotapi.BotAPI) {
	activityEntryDispatch.Submit("dispatch", func() { dispatchActivityEntries(bot, time.Now()) }, true)
}

func dispatchActivityEntries(bot *tgbotapi.BotAPI, now time.Time) {
	if storage.DB == nil {
		return
	}
	var due []model.ActivityEntryRequest
	err := storage.DB.Where("status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ? OR deadline_at <= ?)", "pending", now, now).
		Order("created_at ASC, id ASC").Limit(activityEntryPendingLimit).Find(&due).Error
	if err != nil {
		log.Printf("活动入场队列读取失败: %s", textfmt.FormatPlainError(err))
		return
	}
	// Interleave events before dispatch, so a burst from one Boss cannot fill
	// every worker ahead of a realm. Per-event turns include queued jobs, too.
	groups := make(map[string][]model.ActivityEntryRequest)
	var order []string
	for _, r := range due {
		key := activityEntryGroup(r)
		if len(groups[key]) == 0 {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	for round := 0; round < 3; round++ {
		for _, key := range order {
			if len(groups[key]) <= round {
				continue
			}
			r := groups[key][round]
			release, ok := startActivityEntryTurn(r)
			if !ok {
				continue
			}
			result := activityEntryWorkers.Submit(fmt.Sprintf("entry:%d", r.ID), func() {
				defer func() { release(); kickActivityEntries(bot) }()
				processActivityEntry(bot, r.ID)
			}, false)
			if result != background.Accepted {
				release()
			}
		}
	}
	var notices []model.ActivityEntryRequest
	if err := storage.DB.Where("status IN ? AND notified_at IS NULL AND (next_notification_at IS NULL OR next_notification_at <= ?)", []string{"joined", "failed"}, now).
		Order("next_notification_at ASC, completed_at ASC, id ASC").Limit(64).Find(&notices).Error; err != nil {
		log.Printf("活动入场通知读取失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, r := range notices {
		enqueueActivityEntryResult(bot, r)
	}
}

// All network replies are off the message worker, including busy/duplicate
// receipts. A full notification queue never blocks or discards the entry row.
func replyActivityEntry(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, message string) {
	if msg == nil || msg.Chat == nil {
		return
	}
	r := model.ActivityEntryRequest{ChatID: msg.Chat.ID, MessageID: msg.MessageID}
	key := fmt.Sprintf("entry-reply:%d:%d:%s", r.ChatID, msg.From.ID, message)
	if activityEntryReceipts.Submit(key, func() {
		if err := sendActivityEntryMessage(bot, r, message); err != nil {
			log.Printf("活动入场回执发送失败: %s", textfmt.FormatTelegramSendError(err))
		}
	}, false) == background.Full {
		log.Printf("活动入场回执队列已满: chat=%d", r.ChatID)
	}
}

func sendActivityEntryMessage(bot *tgbotapi.BotAPI, r model.ActivityEntryRequest, message string) error {
	if bot == nil {
		return errors.New("Telegram 尚未初始化")
	}
	m := tgbotapi.NewMessage(r.ChatID, message)
	m.ParseMode = "Markdown"
	if r.ChatID < 0 && r.MessageID > 0 {
		m.ReplyToMessageID = r.MessageID
		m.AllowSendingWithoutReply = true
	}
	_, err := telegram.SendAutoDelete(bot, m)
	return err
}

func enqueueActivityEntryResult(bot *tgbotapi.BotAPI, r model.ActivityEntryRequest) {
	activityEntryMessages.Submit(fmt.Sprintf("entry-result:%d:%d", r.ID, r.Generation), func() {
		now := time.Now()
		// Unlike baseline sampling, result delivery must survive any number of
		// failures/restarts. Persist a bounded backoff before attempting the send.
		delay := min(30*time.Second*time.Duration(1<<min(max(r.NotificationAttempts, 0), 6)), 30*time.Minute)
		claim := storage.DB.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status IN ? AND notified_at IS NULL AND (next_notification_at IS NULL OR next_notification_at <= ?)",
			r.ID, r.Generation, []string{"joined", "failed"}, now).
			Updates(map[string]any{"notification_attempts": gorm.Expr("notification_attempts + 1"), "next_notification_at": now.Add(delay)})
		if claim.Error != nil || claim.RowsAffected != 1 {
			return
		}
		if err := sendActivityEntryMessage(bot, r, r.ResultText); err != nil {
			log.Printf("活动入场结果发送失败: request=%d err=%s", r.ID, textfmt.FormatTelegramSendError(err))
			return
		}
		if err := storage.DB.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ?", r.ID, r.Generation).Update("notified_at", time.Now()).Error; err != nil {
			log.Printf("活动入场通知确认失败: request=%d err=%s", r.ID, textfmt.FormatPlainError(err))
		}
	}, false)
}

func submitActivityEntry(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, request model.ActivityEntryRequest) {
	request.ChatID, request.MessageID = msg.Chat.ID, msg.MessageID
	request.UserName = textfmt.GetTelegramDisplayName(msg.From)
	duplicate := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Acquire SQLite's writer before capacity checks and the unique-key read.
		if err := tx.Exec("UPDATE users SET telegram_id = telegram_id WHERE telegram_id = ?", request.UserID).Error; err != nil {
			return err
		}
		if err := validateActivityEntryTx(tx, request, time.Now()); err != nil {
			return err
		}
		var saved model.ActivityEntryRequest
		err := tx.Where("kind = ? AND event_id = ? AND user_id = ?", request.Kind, request.EventID, request.UserID).First(&saved).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && saved.Status != "failed" {
			request, duplicate = saved, true
			return nil
		}
		var total, eventTotal int64
		if err := tx.Model(&model.ActivityEntryRequest{}).Where("status = ?", "pending").Count(&total).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ActivityEntryRequest{}).Where("status = ? AND kind = ? AND event_id = ?", "pending", request.Kind, request.EventID).Count(&eventTotal).Error; err != nil {
			return err
		}
		if total >= activityEntryPendingLimit || eventTotal >= activityEntryEventLimit {
			return entryIneligible("报名队列繁忙，请稍后重试")
		}
		request.Status, request.Generation = "pending", 1
		request.DeadlineAt = minTime(request.DeadlineAt, time.Now().Add(activityEntryTimeout))
		if err == nil {
			request.ID, request.Generation = saved.ID, saved.Generation+1
			request.CreatedAt = time.Now()
			return tx.Select("*").Save(&request).Error
		}
		return tx.Create(&request).Error
	})
	if err != nil {
		log.Printf("入场申请登记失败: kind=%s event=%s user=%d err=%s", request.Kind, textfmt.FormatPlainValue(request.EventID), request.UserID, textfmt.FormatPlainError(err))
		reason := "暂时无法入场，请稍后重试"
		var invalid *activityEntryIneligible
		if errors.As(err, &invalid) {
			reason = invalid.reason
		}
		replyActivityEntry(bot, msg, "❌ 入场失败："+textfmt.EscapeMarkdown(reason))
		return
	}
	if duplicate && request.Status == "joined" {
		replyActivityEntry(bot, msg, activityEntryAlreadyJoinedText(request.Kind))
		return
	}
	if duplicate {
		replyActivityEntry(bot, msg, "⏳ 你的入场申请已在处理中，请勿重复提交。")
		kickActivityEntries(bot)
		return
	}
	kickActivityEntries(bot)
}

func processActivityEntry(bot *tgbotapi.BotAPI, id uint) {
	var r model.ActivityEntryRequest
	if err := storage.DB.First(&r, id).Error; err != nil || r.Status != "pending" {
		return
	}
	if err := validateActivityEntryTx(storage.DB, r, time.Now()); err != nil {
		var invalid *activityEntryIneligible
		if errors.As(err, &invalid) {
			failActivityEntry(r, err)
		} else {
			retryActivityEntry(r, err)
		}
		return
	}
	if !time.Now().Before(r.DeadlineAt) {
		failActivityEntry(r, errors.New("未能在入场期限内完成，请等待下次活动或重新报名"))
		return
	}
	if r.NextAttemptAt != nil && time.Now().Before(*r.NextAttemptAt) {
		return
	}
	if r.Attempts >= activityEntryMaxAttempts {
		failActivityEntry(r, errors.New("多次读取听书基线失败，请稍后重新报名"))
		return
	}
	// Realm entry updates the ordinary cultivation snapshot for eligibility, but
	// it must not wait behind a full-server publication barrier.
	if r.Kind == activityEntryRealm {
		if !listeningRefreshMu.TryRLock() {
			retryActivityEntry(r, errors.New("听书统计正在更新，等待入场重试"))
			return
		}
		defer listeningRefreshMu.RUnlock()
	}
	group := activityEntryGroup(r)
	started := time.Now()
	if !listeningSyncSlots.acquire(listeningRequestEntry, group, min(5*time.Second, time.Until(r.DeadlineAt))) {
		retryActivityEntry(r, errors.New("同步额度繁忙，等待入场重试"))
		return
	}
	var stats officialListeningStats
	var requestedAt, sampledAt time.Time
	var err error
	func() {
		defer listeningSyncSlots.release(group)
		if !time.Now().Before(r.DeadlineAt) {
			err = errors.New("入场准备已超时")
			return
		}
		attemptAt := time.Now()
		res := storage.DB.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status = ?", r.ID, r.Generation, "pending").
			Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_attempt_at": attemptAt})
		if res.Error != nil {
			err = res.Error
			return
		}
		if res.RowsAffected != 1 {
			err = errors.New("入场申请状态已变化")
			return
		}
		r.Attempts++
		requestedAt = time.Now()
		stats, sampledAt, err = fetchActivityListeningStats(r.AbsUserID, requestedAt)
	}()
	log.Printf("活动入场采样: request=%d kind=%s event=%s attempts=%d duration_ms=%d err=%s", r.ID, r.Kind, textfmt.FormatPlainValue(r.EventID), r.Attempts, time.Since(started).Milliseconds(), textfmt.FormatPlainError(err))
	if err != nil {
		retryActivityEntry(r, err)
		return
	}
	if SectDayKey(requestedAt) != SectDayKey(sampledAt) {
		retryActivityEntry(r, errors.New("听书基线跨日，需要重新采样"))
		return
	}
	if !sampledAt.Before(r.DeadlineAt) {
		failActivityEntry(r, errors.New("基线采样完成时已超过入场期限"))
		return
	}
	if err := validateActivityEntryTx(storage.DB, r, time.Now()); err != nil {
		var invalid *activityEntryIneligible
		if errors.As(err, &invalid) {
			failActivityEntry(r, err)
		} else {
			retryActivityEntry(r, err)
		}
		return
	}
	baseline := newActivityListeningSnapshot(stats.Days, sampledAt)
	baseHours := 0.0
	if r.Kind == activityEntryRealm {
		credited, recordErr := recordCurrentListeningDay(r.UserID, r.AbsUserID, stats.Days[SectDayKey(requestedAt)], requestedAt)
		if recordErr != nil {
			retryActivityEntry(r, recordErr)
			return
		}
		baseline.BaselineSeconds = dailyListeningCreditSeconds(baseline.BaselineDay, credited[baseline.BaselineDay], sampledAt)
		var ok bool
		baseHours, ok = secretRealmCultivationTotal(r.UserID)
		if !ok {
			retryActivityEntry(r, errors.New("修为统计暂时无法校验"))
			return
		}
	}
	result, err := commitActivityEntry(r, baseline, baseHours, requestedAt)
	if err != nil {
		var invalid *activityEntryIneligible
		if errors.As(err, &invalid) {
			failActivityEntry(r, err)
		} else {
			retryActivityEntry(r, err)
		}
		return
	}
	r.Status, r.ResultText = "joined", result
	enqueueActivityEntryResult(bot, r)
}

func retryActivityEntry(r model.ActivityEntryRequest, cause error) {
	if r.Attempts >= activityEntryMaxAttempts || !time.Now().Before(r.DeadlineAt) {
		failActivityEntry(r, cause)
		return
	}
	next := minTime(time.Now().Add(time.Duration(max(1, r.Attempts))*5*time.Second), r.DeadlineAt)
	if err := storage.DB.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status = ?", r.ID, r.Generation, "pending").
		Updates(map[string]any{"last_error": textfmt.TruncateRunes(textfmt.FormatPlainError(cause), 240), "next_attempt_at": next}).Error; err != nil {
		log.Printf("活动入场重试保存失败: request=%d err=%s", r.ID, textfmt.FormatPlainError(err))
	}
}

func failActivityEntry(r model.ActivityEntryRequest, cause error) {
	reason := "读取听书基线暂未成功，请稍后重新报名"
	var invalid *activityEntryIneligible
	if errors.As(cause, &invalid) {
		reason = invalid.reason
	}
	if !time.Now().Before(r.DeadlineAt) {
		reason = "未能在入场期限内完成，请等待下一场或重新报名"
	}
	message := "❌ 道友：`" + textfmt.EscapeMarkdown(r.UserName) + "`\n入场未成功：" + textfmt.EscapeMarkdown(reason) + "。本次尚未参加活动。"
	if err := storage.DB.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status = ?", r.ID, r.Generation, "pending").
		Updates(map[string]any{"status": "failed", "last_error": textfmt.TruncateRunes(textfmt.FormatPlainError(cause), 240), "result_text": message, "completed_at": time.Now(), "next_attempt_at": nil}).Error; err != nil {
		log.Printf("活动入场失败状态保存失败: request=%d err=%s", r.ID, textfmt.FormatPlainError(err))
	}
}

func activityEntryStatus(kind, eventID string, userID int64) (string, bool, error) {
	var r model.ActivityEntryRequest
	err := storage.DB.Where("kind = ? AND event_id = ? AND user_id = ?", kind, eventID, userID).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if r.Status == "pending" {
		return "⏳ 道友：`" + textfmt.EscapeMarkdown(r.UserName) + "`\n入场正在处理中，尚未正式入场；完成后会通知成功或失败。", true, nil
	}
	return r.ResultText, true, nil
}

func activityEntryAlreadyJoinedText(kind string) string {
	if kind == activityEntryRealm {
		return "✅ 你当前已经加入过本次宗门秘境，请发送 `宗门秘境明细` 查看个人记录。"
	}
	return "✅ 你当前已经加入过本期世界Boss讨伐，请发送 `Boss明细` 查看个人战况。"
}

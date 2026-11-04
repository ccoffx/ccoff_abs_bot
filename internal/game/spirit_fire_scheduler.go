package game

import (
	"errors"
	"fmt"
	"log"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

const (
	spiritFireNoticePending   = "pending"
	spiritFireNoticeSending   = "sending"
	spiritFireNoticeSent      = "sent"
	spiritFireNoticeFailed    = "failed"
	spiritFireNoticeUncertain = "uncertain"
	spiritFireNoticeStaleAge  = 10 * time.Minute
)

func StartSpiritFireScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		notices := time.NewTicker(time.Minute)
		defer notices.Stop()
		lastSweep := time.Time{}
		for now := range notices.C {
			if err := dispatchSpiritFireNotices(bot); err != nil {
				log.Printf("异火通知待重试: %v", err)
			}
			if now.Sub(lastSweep) < time.Hour {
				continue
			}
			var owners []int64
			if err := storage.DB.Model(&model.SpiritFire{}).Where("owner_id <> 0").Distinct().Pluck("owner_id", &owners).Error; err != nil {
				log.Printf("异火巡检查询失败: %v", err)
				continue
			}
			lastSweep = now
			for _, owner := range owners {
				var u model.User
				err := storage.DB.Where("telegram_id = ?", owner).First(&u).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					log.Printf("异火巡检档案读取失败: %v", err)
					continue
				}
				refreshed := false
				if err == nil && u.AbsUserID != "" {
					_, refreshed = refreshDailyListeningStatsFromABS(owner, u.AbsUserID, true)
				}
				if err := spiritfire.SweepOwner(storage.DB, owner, refreshed, now); err != nil {
					log.Printf("异火巡检跳过: user=%d err=%v", owner, err)
				}
			}
		}
	}()
}
func dispatchSpiritFireNotices(bot *tgbotapi.BotAPI) error {
	now := time.Now()
	// A process can stop after Telegram accepts a send but before its receipt
	// commits. Expired claims must not return to the queue and send it again.
	if err := storage.DB.Model(&model.SpiritFireNotice{}).
		Where("status = ? AND sending_at <= ?", spiritFireNoticeSending, now.Add(-spiritFireNoticeStaleAge)).
		Update("status", spiritFireNoticeUncertain).Error; err != nil {
		return err
	}
	var notices []model.SpiritFireNotice
	if err := storage.DB.Where("status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", spiritFireNoticePending, now).Order("id").Limit(50).Find(&notices).Error; err != nil {
		return err
	}
	for _, n := range notices {
		started := time.Now()
		claim := storage.DB.Model(&model.SpiritFireNotice{}).
			Where("id = ? AND status = ? AND attempts = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", n.ID, spiritFireNoticePending, n.Attempts, started).
			Updates(map[string]any{"status": spiritFireNoticeSending, "sending_at": started, "next_attempt_at": nil, "attempts": gorm.Expr("attempts + 1")})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			continue
		}
		_, sendErr := telegram.SendNoAutoDelete(bot, tgbotapi.NewMessage(n.UserID, n.Text))
		status, next := spiritFireNoticeResult(sendErr, n.Attempts+1, time.Now())
		if sendErr != nil {
			log.Printf("异火通知发送未完成: notice=%d status=%s err=%s", n.ID, status, textfmt.FormatTelegramSendError(sendErr))
		}
		receipt := storage.DB.Model(&model.SpiritFireNotice{}).
			Where("id = ? AND status = ?", n.ID, spiritFireNoticeSending).
			Updates(map[string]any{"status": status, "next_attempt_at": next})
		if receipt.Error != nil {
			return receipt.Error
		}
		if receipt.RowsAffected != 1 {
			return fmt.Errorf("异火通知发送回执状态变化: notice=%d", n.ID)
		}
	}
	return nil
}

func spiritFireNoticeResult(err error, attempts int, now time.Time) (string, *time.Time) {
	if err == nil {
		return spiritFireNoticeSent, nil
	}
	var apiPointer *tgbotapi.Error
	var apiValue tgbotapi.Error
	retryAfter := 0
	rateLimited := false
	if errors.As(err, &apiPointer) {
		rateLimited, retryAfter = apiPointer.Code == 429, apiPointer.RetryAfter
	} else if errors.As(err, &apiValue) {
		rateLimited, retryAfter = apiValue.Code == 429, apiValue.RetryAfter
	}
	if rateLimited {
		backoff := time.Duration(1<<min(attempts, 10)) * time.Minute
		if delay := time.Duration(retryAfter) * time.Second; delay > backoff {
			backoff = delay
		}
		next := now.Add(backoff)
		return spiritFireNoticePending, &next
	}
	if telegram.IsDefiniteTelegramSendFailure(err) {
		return spiritFireNoticeFailed, nil
	}
	return spiritFireNoticeUncertain, nil
}

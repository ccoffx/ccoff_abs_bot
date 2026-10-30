package game

import (
	"errors"
	"log"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
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
			lastSweep = now
			var owners []int64
			if err := storage.DB.Model(&model.SpiritFire{}).Where("owner_id <> 0").Distinct().Pluck("owner_id", &owners).Error; err != nil {
				log.Printf("异火巡检查询失败: %v", err)
				continue
			}
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
	var notices []model.SpiritFireNotice
	if err := storage.DB.Where("sent = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", false, time.Now()).Order("id").Limit(50).Find(&notices).Error; err != nil {
		return err
	}
	for _, n := range notices {
		if _, err := telegram.SendNoAutoDelete(bot, tgbotapi.NewMessage(n.UserID, n.Text)); err != nil {
			log.Printf("异火通知发送失败: notice=%d err=%v", n.ID, err)
			attempts := n.Attempts + 1
			backoff := time.Duration(1<<min(attempts, 10)) * time.Minute
			if err := storage.DB.Model(&n).Updates(map[string]any{"attempts": attempts, "next_attempt_at": time.Now().Add(backoff)}).Error; err != nil {
				return err
			}
			continue
		}
		if err := storage.DB.Model(&n).Update("sent", true).Error; err != nil {
			return err
		}
	}
	return nil
}

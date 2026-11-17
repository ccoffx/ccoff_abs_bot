package game

import (
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

// Kept for existing callers; reading a report no longer drives synchronization
// or rewards. The background dispatcher consumes durable settlement events.
func FetchReportAndCheckUpgrade(_ *tgbotapi.BotAPI, _ int64, absUserID string) string {
	if absUserID == "" {
		return ""
	}
	return GetPersonalReport(abs.DefaultClient, absUserID)
}

var listeningUpgradeDeliveryMu sync.Mutex

func DeliverListeningUpgrades(bot *tgbotapi.BotAPI) {
	if bot == nil || storage.DB == nil || config.AppConfig == nil || !listeningUpgradeDeliveryMu.TryLock() {
		return
	}
	defer listeningUpgradeDeliveryMu.Unlock()
	var events []model.CultivationUpgradeEvent
	if err := storage.DB.Where("completed = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", false, time.Now()).Order("id").Limit(20).Find(&events).Error; err != nil {
		log.Printf("读取听书升级通知失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, event := range events {
		// Reserve a retry time before delivery; a crash leaves the event pending.
		retryAt := time.Now().Add(time.Minute * time.Duration(min(360, 1<<min(event.Attempts, 9))))
		if err := storage.DB.Model(&event).UpdateColumns(map[string]any{"next_attempt_at": retryAt, "attempts": gorm.Expr("attempts + 1")}).Error; err != nil {
			continue
		}
		if err := settleListeningUpgradeReward(&event); err != nil {
			log.Printf("听书升级奖池结算失败: event=%d err=%s", event.ID, textfmt.FormatPlainError(err))
			continue
		}
		var user model.User
		if err := storage.DB.Where("telegram_id = ?", event.UserID).First(&user).Error; err != nil {
			continue
		}
		name := textfmt.EscapeMarkdown(user.Username)
		if name == "" {
			name = "神秘道友"
		}
		oldRealm := GetRealmName(&model.Cultivation{MajorRealm: event.FromMajor, MinorRealm: event.FromMinor})
		newRealm := GetRealmName(&model.Cultivation{MajorRealm: event.ToMajor, MinorRealm: event.ToMinor})
		announce := fmt.Sprintf("🎉 **【斗气精进·修为突破】** 🎉\n\n恭喜道友 %s 闭关苦修，厚积薄发！\n\n✨ 成功从 **%s** 突破至全新的 **%s** 境界！", name, oldRealm, newRealm)
		if !event.PrivateSent {
			if _, err := telegram.SendAutoDelete(bot, tgbotapi.NewMessage(event.UserID, announce)); err != nil {
				log.Printf("听书升级私聊通知失败: event=%d err=%s", event.ID, textfmt.FormatTelegramSendError(err))
			} else {
				if err := storage.DB.Model(&event).UpdateColumn("private_sent", true).Error; err != nil {
					continue
				}
				event.PrivateSent = true
			}
		}
		if !event.GroupSent {
			groupID := config.AppConfig.NoticeGroupID
			if groupID != 0 {
				text := announce
				if event.PoolPoints > 0 {
					text += fmt.Sprintf("\n\n💧 为天道奖池注入 `%d` 积分，当前进度 `%d/300`。", event.PoolPoints, event.PoolCurrent)
				}
				if _, err := telegram.SendAutoDelete(bot, tgbotapi.NewMessage(groupID, text)); err != nil {
					log.Printf("听书升级群通知失败: event=%d err=%s", event.ID, textfmt.FormatTelegramSendError(err))
					continue
				}
				if event.PoolBurst {
					telegram.NotifyFusionPoolBurst(bot, groupID, "众道友接连突破引动天地异象")
				}
			}
			if err := storage.DB.Model(&event).UpdateColumn("group_sent", true).Error; err != nil {
				continue
			}
			event.GroupSent = true
		}
		if event.PrivateSent && event.GroupSent {
			storage.DB.Model(&event).UpdateColumn("completed", true)
		}
	}
}

// Payment and its idempotency flag share the pool's locked transaction. Retrying
// Telegram delivery cannot pay again, even after process restart.
func settleListeningUpgradeReward(event *model.CultivationUpgradeEvent) error {
	return storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		if err := tx.First(event, event.ID).Error; err != nil {
			return err
		}
		if event.RewardSettled {
			return nil
		}
		if config.AppConfig != nil && config.AppConfig.NoticeGroupID != 0 && event.FromMajor == event.ToMajor {
			points, err := security.RandomIntRange(2, 4)
			if err != nil {
				return err
			}
			current, burst, err := storage.AddPointsToFusionPoolInTx(tx, points)
			if err != nil {
				return err
			}
			event.PoolPoints, event.PoolCurrent, event.PoolBurst = points, current, burst
		}
		return tx.Model(event).Updates(map[string]any{"reward_settled": true, "pool_points": event.PoolPoints, "pool_current": event.PoolCurrent, "pool_burst": event.PoolBurst}).Error
	})
}

func HandleBreakthroughConfirmation(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, session *telegram.SessionState, text string) {
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return
	}
	if session == nil {
		session = telegram.GetSession(msg.From.ID)
	}

	if !msg.Chat.IsPrivate() {
		telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	}

	mode := session.GetTemp("bt_mode")
	if (mode == "USE_INVENTORY" && text == "确认渡劫") || (mode == "AUTO_BUY" && text == "确认代购并渡劫") {
		// 将控制权移交给底层渡劫处决引擎
		BeginBreakthroughWithGuards(bot, msg, mode)
	} else {
		telegram.ReplyTextToMessage(bot, msg, "🛑 您已压制体内翻涌的气血，取消了本次渡劫。")
	}
	telegram.ClearSession(msg.From.ID)
}

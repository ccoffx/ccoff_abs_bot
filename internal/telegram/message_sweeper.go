package telegram

import (
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const redPacketSettlementInterval = 5 * time.Minute

// 🛡️ 核心重构：后台巡警协程，集成了消息安全清理与内存垃圾回收
func StartMessageSweeper(bot *tgbotapi.BotAPI) {
	// 1. Cron 定时清理消息任务
	go func() {
		// A zero deadline settles historical packets on the first sweep after startup.
		var nextRedPacketSettlement time.Time
		for {
			time.Sleep(30 * time.Second)

			defaultChatID := int64(0)
			if config.AppConfig != nil {
				defaultChatID = config.AppConfig.NoticeGroupID
			}
			now := time.Now()
			if !now.Before(nextRedPacketSettlement) {
				if err := storage.SettleExpiredRedPackets(now); err != nil {
					log.Printf("红包超时结算失败，将在下次结算检查时重试: %s", textfmt.FormatPlainError(err))
				}
				nextRedPacketSettlement = time.Now().Add(redPacketSettlementInterval)
			}
			SyncRedPacketNotices(bot, defaultChatID)

			var msgs []model.AutoDeleteMsg
			if err := storage.DB.Where("delete_at <= ?", time.Now()).Find(&msgs).Error; err != nil {
				log.Printf("⚠️ 自动删消息队列读取失败: err=%s", textfmt.FormatPlainError(err))
				continue
			}
			for _, m := range msgs {
				if m.PacketID != "" {
					active, err := retainRedPacketNotice(m, time.Now())
					if err != nil || active {
						continue
					}
				}
				if err := deleteAutoDeleteMessage(bot, m); err != nil {
					log.Printf("自动删除 Telegram 消息失败，保留发布凭据: id=%d err=%s", m.ID, textfmt.FormatTelegramSendError(err))
				}

				time.Sleep(50 * time.Millisecond)
			}
		}
	}()

	// 2. 内存清道夫任务
	go func() {
		for {
			time.Sleep(1 * time.Hour)
			now := time.Now()

			// 清理过期的群成员鉴权缓存
			groupMemberCache.Range(func(key, value interface{}) bool {
				if entry, ok := value.(*groupMemberCacheEntry); ok {
					if now.After(entry.expireAt) {
						groupMemberCache.CompareAndDelete(key, value)
					}
				} else {
					// 清理类型异常的缓存数据
					groupMemberCache.CompareAndDelete(key, value)
				}
				return true
			})

			sweepIdleSessions(now)
			sweepIdleUserLocks(now)
		}
	}()
}

// Packet announcements may be replaced only after deletion is confirmed.
// Permission errors and expired deletion windows do not prove disappearance.
func deleteAutoDeleteMessage(bot *tgbotapi.BotAPI, record model.AutoDeleteMsg) error {
	_, err := bot.Request(tgbotapi.NewDeleteMessage(record.ChatID, record.MessageID))
	if err != nil && !isTelegramMessageAlreadyDeleted(err) {
		if record.PacketID != "" || !isTerminalTelegramDeleteError(err) {
			return err
		}
		var guarded int64
		if queryErr := storage.DB.Model(&model.RedPacket{}).Where("notice_chat_id = ? AND notice_legacy_cleanup_max_id >= ? AND is_finished = ? AND left_count > 0 AND left_points > 0", record.ChatID, record.ID, false).Count(&guarded).Error; queryErr != nil {
			return queryErr
		}
		if guarded > 0 {
			return err
		}
	}
	return completeRedPacketNoticeCleanup(record)
}

func SendGroupAutoDeleteMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if _, err := SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送 Telegram 自动删除消息失败: %s", textfmt.FormatTelegramSendError(err))
	}
}

func SendGroupAutoDeleteMessageAsync(bot *tgbotapi.BotAPI, chatID int64, text string, kind string, dedupeKey string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if !EnqueueAutoDelete(bot, msg, kind, TelegramAsyncPriorityNormal, dedupeKey) {
		log.Printf("⚠️ Telegram 群通知异步入队失败: chat=%d kind=%s", chatID, textfmt.FormatPlainValue(kind))
	}
}

func IsTelegramCommandText(text string, command string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}

	head := strings.ToLower(fields[0])
	command = strings.ToLower(command)
	return head == command || strings.HasPrefix(head, command+"@")
}

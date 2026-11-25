package telegram

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

var redPacketNoticeMu sync.Mutex

// Only migration-time unlinked records block publication. New group commands
// must not extend the transition indefinitely.
const redPacketLegacyCleanupCompleteSQL = "NOT EXISTS (SELECT 1 FROM auto_delete_msgs AS legacy WHERE legacy.chat_id = red_packets.notice_chat_id AND COALESCE(legacy.packet_id, '') = '' AND legacy.id <= COALESCE(red_packets.notice_legacy_cleanup_max_id, 0))"

const redPacketNoticeMaxAge = 46 * time.Hour

// Retain receipts when the database itself is unavailable. The next sync retries
// cleanup before considering another publication for the same packet.
var pendingRedPacketNoticeCleanup = map[string]model.AutoDeleteMsg{}

// Preserve the known rejection policy even if saving that result fails.
// Access is serialized by redPacketNoticeMu, just like compensation receipts.
type redPacketNoticeRejection struct {
	ChatID  int64
	State   string
	RetryAt *time.Time
}

var pendingRedPacketNoticeRejections = map[string]redPacketNoticeRejection{}

func noticeRejection(chatID int64, err error, now time.Time) redPacketNoticeRejection {
	result := redPacketNoticeRejection{ChatID: chatID, State: "failed"}
	var pointer *tgbotapi.Error
	var value tgbotapi.Error
	if errors.As(err, &pointer) {
		value = *pointer
	} else {
		errors.As(err, &value)
	}
	if value.Code == 429 || value.Code == 408 {
		delay := time.Minute
		if value.RetryAfter > 0 && (value.Code == 429 || time.Duration(value.RetryAfter)*time.Second > delay) {
			delay = time.Duration(value.RetryAfter) * time.Second
		}
		next := now.Add(delay)
		result.State, result.RetryAt = "rejected", &next
		if value.Code == 429 {
			result.State = "rate_limited"
		}
	}
	return result
}

func persistRedPacketNoticeRejection(packetID string, result redPacketNoticeRejection) {
	res := storage.DB.Model(&model.RedPacket{}).Where("id = ? AND notice_chat_id = ? AND notice_send_state = ? AND COALESCE(notice_message_id, 0) = 0", packetID, result.ChatID, "uncertain").Updates(map[string]any{"notice_send_state": result.State, "notice_retry_at": result.RetryAt})
	if res.Error != nil {
		pendingRedPacketNoticeRejections[packetID] = result
		log.Printf("红包公告明确拒绝后状态登记失败，将重试: packet=%s err=%s", packetID, textfmt.FormatPlainError(res.Error))
		return
	}
	delete(pendingRedPacketNoticeRejections, packetID)
}

func compensateRedPacketNotice(bot *tgbotapi.BotAPI, record model.AutoDeleteMsg) bool {
	_, err := bot.Request(tgbotapi.NewDeleteMessage(record.ChatID, record.MessageID))
	if err != nil && !isTelegramMessageAlreadyDeleted(err) {
		if record.ID == 0 {
			if saveErr := storage.DB.Create(&record).Error; saveErr != nil {
				log.Printf("红包公告补偿清理登记失败: packet=%s err=%s", record.PacketID, textfmt.FormatPlainError(saveErr))
			}
		}
		pendingRedPacketNoticeCleanup[record.PacketID] = record
		return false
	}
	if err := completeRedPacketNoticeCleanup(record); err != nil {
		pendingRedPacketNoticeCleanup[record.PacketID] = record
		return false
	}
	delete(pendingRedPacketNoticeCleanup, record.PacketID)
	return true
}

// A superseded or unsuccessfully registered announcement must be cleaned even
// while its packet is active. DeleteAt encodes publication time for old receipts.
func retainRedPacketNotice(record model.AutoDeleteMsg, now time.Time) (bool, error) {
	if !now.Before(record.DeleteAt.Add(redPacketNoticeMaxAge - botMessageAutoDeleteDelay)) {
		return false, nil
	}
	var count int64
	err := storage.DB.Model(&model.RedPacket{}).Where("id = ? AND notice_chat_id = ? AND notice_message_id = ? AND is_finished = ? AND left_count > 0 AND left_points > 0", record.PacketID, record.ChatID, record.MessageID, false).Where("created_at > ?", now.Add(-storage.RedPacketLifetime)).Count(&count).Error
	return count > 0, err
}

func completeRedPacketNoticeCleanup(record model.AutoDeleteMsg) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if record.PacketID != "" {
			if err := tx.Model(&model.RedPacket{}).Where("id = ? AND notice_chat_id = ? AND (notice_message_id = ? OR (COALESCE(notice_message_id, 0) = 0 AND notice_send_state = ?))", record.PacketID, record.ChatID, record.MessageID, "uncertain").Updates(map[string]any{"notice_message_id": 0, "notice_send_state": ""}).Error; err != nil {
				return err
			}
		}
		if record.ID == 0 {
			return nil
		}
		return tx.Delete(&record).Error
	})
}

func redPacketNoticeActive(packetID string) (bool, error) {
	var count int64
	err := storage.DB.Model(&model.RedPacket{}).Where("id = ? AND is_finished = ? AND left_count > 0 AND left_points > 0", packetID, false).Where("created_at > ?", time.Now().Add(-storage.RedPacketLifetime)).Count(&count).Error
	return count > 0, err
}

// SyncRedPacketNotices also catches pool bursts from transaction-only callers.
// Destinations must already be persisted at packet creation; chatID is retained
// for caller compatibility and never assigns an unaddressed packet.
// Persisted message IDs prevent ordinary sweeps and restarts from republishing.
func SyncRedPacketNotices(bot *tgbotapi.BotAPI, chatID int64) {
	if bot == nil || storage.DB == nil || chatID > 0 {
		return
	}
	redPacketNoticeMu.Lock()
	defer redPacketNoticeMu.Unlock()
	for packetID, result := range pendingRedPacketNoticeRejections {
		persistRedPacketNoticeRejection(packetID, result)
	}
	// Telegram flood control can apply to the bot, so event-triggered syncs and
	// other packets must also respect the persisted rate-limit deadline.
	for _, result := range pendingRedPacketNoticeRejections {
		if result.State == "rate_limited" && result.RetryAt != nil && time.Now().Before(*result.RetryAt) {
			return
		}
	}
	var limited int64
	if err := storage.DB.Model(&model.RedPacket{}).Where("notice_send_state = ? AND notice_retry_at > ?", "rate_limited", time.Now()).Count(&limited).Error; err != nil || limited > 0 {
		return
	}
	for _, record := range pendingRedPacketNoticeCleanup {
		compensateRedPacketNotice(bot, record)
	}
	var packets []model.RedPacket
	if err := storage.DB.Where("COALESCE(notice_message_id, 0) = 0 AND COALESCE(notice_send_state, '') IN ('', 'rejected', 'rate_limited') AND is_finished = ? AND left_count > 0 AND left_points > 0", false).Where("created_at > ?", time.Now().Add(-storage.RedPacketLifetime)).Where("NOT EXISTS (SELECT 1 FROM auto_delete_msgs WHERE auto_delete_msgs.packet_id = red_packets.id)").Where("notice_retry_at IS NULL OR notice_retry_at <= ?", time.Now()).Where(redPacketLegacyCleanupCompleteSQL).Order("created_at asc").Find(&packets).Error; err != nil {
		log.Printf("红包公告待发送队列读取失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, packet := range packets {
		if _, pending := pendingRedPacketNoticeCleanup[packet.ID]; pending {
			continue
		}
		targetChatID := packet.NoticeChatID
		if targetChatID >= 0 {
			continue
		}
		// Persist uncertainty BEFORE making the external call. A crash or a lost
		// response must never turn into an automatic duplicate publication.
		reservation := storage.DB.Model(&model.RedPacket{}).Where("id = ? AND notice_chat_id = ? AND COALESCE(notice_message_id, 0) = 0 AND COALESCE(notice_send_state, '') IN ('', 'rejected', 'rate_limited') AND is_finished = ? AND left_count > 0 AND left_points > 0", packet.ID, targetChatID, false).Where("created_at > ?", time.Now().Add(-storage.RedPacketLifetime)).Where("notice_retry_at IS NULL OR notice_retry_at <= ?", time.Now()).Where(redPacketLegacyCleanupCompleteSQL).Updates(map[string]any{"notice_chat_id": targetChatID, "notice_send_state": "uncertain"})
		if reservation.Error != nil {
			log.Printf("红包公告发送预登记失败: packet=%s err=%v", packet.ID, reservation.Error)
			continue
		}
		if reservation.RowsAffected != 1 {
			continue
		}
		command, title := "抢", fmt.Sprintf("🧧 %s 发放了一个积分红包！", textfmt.FormatPlainValue(packet.SenderName))
		if strings.HasPrefix(packet.ID, "FS-") {
			command, title = "沾仙气", "🌈 天道奖池已蓄满，天降甘霖·仙气化雨！"
		}
		scope := ""
		if packet.ClaimScope == storage.RedPacketClaimScopeWorldBossParticipant {
			scope = "\n仅限本期参加 Boss 的道友领取。"
		}
		text := fmt.Sprintf("%s\n\n💰 总额：%d 积分\n📦 总份数：%d 份\n📦 发布时剩余：%d 份%s\n\n请在群内发送「%s」领取。\n红包自发放起 24 小时有效，超时剩余积分回归天道奖池。\n红包编号：%s", title, packet.TotalPoints, packet.Count, packet.LeftCount, scope, command, packet.ID)
		sent, err := SendNoAutoDelete(bot, tgbotapi.NewMessage(targetChatID, text))
		if err == nil && sent.MessageID <= 0 {
			err = fmt.Errorf("Telegram returned no message ID")
		}
		if err != nil {
			if IsDefiniteTelegramSendFailure(err) {
				result := noticeRejection(targetChatID, err, time.Now())
				persistRedPacketNoticeRejection(packet.ID, result)
				log.Printf("红包公告被明确拒绝: packet=%s state=%s retry_at=%v err=%s", packet.ID, result.State, result.RetryAt, textfmt.FormatTelegramSendError(err))
				if result.State == "rate_limited" {
					return
				}
			} else {
				log.Printf("红包公告发送结果未知，已停止自动重发，需人工核查: packet=%s chat=%d", packet.ID, targetChatID)
			}
			log.Printf("红包公告发送失败: packet=%s err=%s", packet.ID, textfmt.FormatTelegramSendError(err))
			continue
		}
		receipt := model.AutoDeleteMsg{ChatID: targetChatID, MessageID: sent.MessageID, PacketID: packet.ID, DeleteAt: time.Now().Add(botMessageAutoDeleteDelay)}
		err = storage.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.RedPacket{}).Where("id = ?", packet.ID).Updates(map[string]any{"notice_chat_id": targetChatID, "notice_message_id": sent.MessageID, "notice_send_state": "sent", "notice_retry_at": nil}).Error; err != nil {
				return err
			}
			return tx.Create(&receipt).Error
		})
		if err == nil {
			active, checkErr := redPacketNoticeActive(packet.ID)
			if checkErr == nil && !active {
				compensateRedPacketNotice(bot, receipt)
			}
		}
		if err != nil {
			compensateRedPacketNotice(bot, model.AutoDeleteMsg{ChatID: targetChatID, MessageID: sent.MessageID, PacketID: packet.ID, DeleteAt: time.Now()})
			log.Printf("红包公告登记失败: packet=%s err=%s", packet.ID, textfmt.FormatPlainError(err))
		}
	}
}

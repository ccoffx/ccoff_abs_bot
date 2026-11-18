package storage

import (
	"errors"
	"strings"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

const legacyRedPacketNoticeTargetsKey = "migration_red_packet_notice_targets_v2"

// MigrateLegacyRedPacketNoticeTargets runs once after schema/index migration.
// Receipts and Boss events take precedence over the historical official-group
// destination. Unknown ownership stays unassigned; event fallbacks are never used.
func MigrateLegacyRedPacketNoticeTargets(db *gorm.DB, officialChatID int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var marker model.SystemConfig
		err := tx.Where("key = ?", legacyRedPacketNoticeTargetsKey).First(&marker).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var packets []model.RedPacket
		if err := tx.Where("is_finished = ? AND left_count > 0 AND left_points > 0 AND (COALESCE(notice_chat_id, 0) = 0 OR (COALESCE(notice_message_id, 0) = 0 AND COALESCE(notice_send_state, '') IN ('', 'rejected')))", false).Find(&packets).Error; err != nil {
			return err
		}
		for _, packet := range packets {
			target := packet.NoticeChatID
			updates := map[string]any{}
			var receipts []model.AutoDeleteMsg
			if err := tx.Where("packet_id = ?", packet.ID).Find(&receipts).Error; err != nil {
				return err
			}
			// Multiple receipts cannot safely identify one live publication.
			if len(receipts) > 1 {
				continue
			}
			if len(receipts) == 1 {
				receipt := receipts[0]
				if receipt.ChatID >= 0 || receipt.MessageID <= 0 || (packet.NoticeMessageID != 0 && packet.NoticeMessageID != receipt.MessageID) {
					continue
				}
				if target != 0 && target != receipt.ChatID {
					continue
				}
				target = receipt.ChatID
				updates["notice_message_id"], updates["notice_send_state"] = receipt.MessageID, "sent"
			} else if target == 0 && (packet.RefType == "world_boss" || packet.ClaimScope == RedPacketClaimScopeWorldBossParticipant) {
				var event model.WorldBossEvent
				err := tx.Where("boss_id = ?", packet.RefID).First(&event).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil && event.ChatID < 0 {
					target = event.ChatID
				} else if err == nil {
					target = officialChatID
				}
			} else if target == 0 && (strings.HasPrefix(packet.ID, "HB-") || strings.HasPrefix(packet.ID, "FS-")) {
				target = officialChatID
			}
			if target >= 0 {
				continue
			}
			updates["notice_chat_id"] = target
			// Old announcements have no PacketID. Snapshot the group's unlinked
			// cleanup records and wait for confirmed removal, rather than guessing
			// which one belongs to this packet or relying on a ten-minute timeout.
			if len(receipts) == 0 && packet.NoticeMessageID == 0 {
				var maxID uint
				if err := tx.Model(&model.AutoDeleteMsg{}).Select("COALESCE(MAX(id), 0)").Where("chat_id = ? AND COALESCE(packet_id, '') = ''", target).Scan(&maxID).Error; err != nil {
					return err
				}
				if maxID > packet.NoticeLegacyCleanupMaxID {
					updates["notice_legacy_cleanup_max_id"] = maxID
				}
			}
			if err := tx.Model(&model.RedPacket{}).Where("id = ?", packet.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.SystemConfig{Key: legacyRedPacketNoticeTargetsKey, Value: "1"}).Error
	})
}

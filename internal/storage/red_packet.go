package storage

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func CreateRedPacketInTx(tx *gorm.DB, packet *model.RedPacket) error {
	if tx == nil || packet == nil {
		return fmt.Errorf("RED_PACKET_INVALID")
	}
	entry := *packet
	// The destination is fixed when creating the packet, never by a later
	// unrelated event that happens to trigger the announcement sweep.
	if entry.NoticeChatID == 0 && config.AppConfig != nil {
		entry.NoticeChatID = config.AppConfig.NoticeGroupID
	}
	entry.ID = textfmt.FormatPlainValue(entry.ID)
	entry.SenderName = textfmt.FormatPlainValue(entry.SenderName)
	entry.RefType = textfmt.FormatPlainValue(entry.RefType)
	entry.RefID = textfmt.FormatPlainValue(entry.RefID)
	entry.ClaimScope = textfmt.FormatPlainValue(entry.ClaimScope)
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("RED_PACKET_CREATE_MISSED")
	}
	*packet = entry
	return nil
}

// RedPacketLifetime is measured from creation, never from notice publication or retry.
const RedPacketLifetime = 24 * time.Hour

// SettleExpiredRedPackets atomically closes packets and recycles their remainder.
// The CAS and pool write share a transaction, so claims, retries and restarts
// cannot spend or return the same points twice. Announcement receipts are kept
// until Telegram confirms deletion.
func SettleExpiredRedPackets(now time.Time) error {
	return RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		var packets []model.RedPacket
		if err := tx.Where("is_finished = ? AND created_at <= ?", false, now.Add(-RedPacketLifetime)).Order("created_at asc, id asc").Find(&packets).Error; err != nil {
			return err
		}
		for _, packet := range packets {
			if packet.LeftPoints < 0 {
				return fmt.Errorf("negative red packet balance: %s", packet.ID)
			}
			res := tx.Model(&model.RedPacket{}).Where("id = ? AND is_finished = ? AND left_count = ? AND left_points = ?", packet.ID, false, packet.LeftCount, packet.LeftPoints).Updates(map[string]any{
				"is_finished": true, "left_count": 0, "left_points": 0,
				"expired_at": now, "returned_points": packet.LeftPoints, "notice_retry_at": nil,
			})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrConcurrentRedPacketGrabRetry
			}
			if _, _, err := AddPointsToFusionPoolInTx(tx, packet.LeftPoints, packet.NoticeChatID); err != nil {
				return err
			}
		}
		return nil
	})
}

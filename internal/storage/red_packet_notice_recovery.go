package storage

import (
	"ccoff-abs-bot/internal/model"
	"fmt"
	"gorm.io/gorm"
)

// ReleaseLegacyRedPacketNoticeBarrier is called only after super-admin review.
// It leaves all message cleanup records and all delivery/retry state intact.
func ReleaseLegacyRedPacketNoticeBarrier(db *gorm.DB, packetID string, actorID int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.RedPacket{}).Where("id = ? AND notice_legacy_cleanup_max_id > 0 AND notice_chat_id < 0 AND COALESCE(notice_message_id,0) = 0 AND COALESCE(notice_send_state,'') IN ('','rejected','rate_limited') AND is_finished = ? AND left_count > 0 AND left_points > 0", packetID, false).Update("notice_legacy_cleanup_max_id", 0)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("红包不存在、已结束或当前不允许解除迁移过渡")
		}
		return tx.Create(&model.AuditLog{ActorID: actorID, ActorRole: "super_admin", Action: "RELEASE_RED_PACKET_NOTICE_BARRIER", Target: packetID, Detail: "管理员已核查旧红包公告不存在，解除历史消息清理屏障；不删除消息凭据或修改发送状态"}).Error
	})
}

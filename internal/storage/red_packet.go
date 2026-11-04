package storage

import (
	"fmt"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func CreateRedPacketInTx(tx *gorm.DB, packet *model.RedPacket) error {
	if tx == nil || packet == nil {
		return fmt.Errorf("RED_PACKET_INVALID")
	}
	entry := *packet
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

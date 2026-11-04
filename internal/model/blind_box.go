package model

import "time"

type BlindBoxReceipt struct {
	ID             string `gorm:"primaryKey"`
	UserID         int64  `gorm:"index;not null;uniqueIndex:idx_blind_box_pending_user,where:delivered_at IS NULL"`
	ReplyEncrypted string `gorm:"not null"`
	Broadcast      string
	CreatedAt      time.Time
	DeliveredAt    *time.Time `gorm:"index"`
}

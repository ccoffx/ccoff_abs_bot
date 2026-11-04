package model

import "time"

type ExchangeCodeReceipt struct {
	ID             string `gorm:"primaryKey"`
	UserID         int64  `gorm:"not null;uniqueIndex:idx_exchange_code_pending_user,where:delivered_at IS NULL"`
	Kind           string `gorm:"not null"`
	ReplyEncrypted string `gorm:"not null"`
	CreatedAt      time.Time
	DeliveredAt    *time.Time `gorm:"index"`
}

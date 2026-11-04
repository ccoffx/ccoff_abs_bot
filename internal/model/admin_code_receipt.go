package model

import "time"

// AdminCodeReceipt keeps a generated batch recoverable until Telegram accepts
// its private delivery. Code plaintext is encrypted with SECURITY_PEPPER.
type AdminCodeReceipt struct {
	ID             string `gorm:"primaryKey"`
	ActorID        int64  `gorm:"not null;uniqueIndex:idx_admin_code_pending_actor,where:delivered_at IS NULL"`
	Kind           string `gorm:"not null"`
	ReplyEncrypted string `gorm:"not null"`
	CreatedAt      time.Time
	DeliveredAt    *time.Time `gorm:"index"`
}

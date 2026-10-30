package model

import "time"

// GroupReply is an exact-match plain text reply for the configured official group.
type GroupReply struct {
	ID        uint   `gorm:"primaryKey"`
	Keyword   string `gorm:"not null;uniqueIndex"`
	Content   string `gorm:"not null"`
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

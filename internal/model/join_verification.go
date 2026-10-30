package model

import "time"

// One durable current round per group/user, independent of listening accounts.
type JoinVerification struct {
	ID                  uint  `gorm:"primaryKey"`
	GroupID             int64 `gorm:"uniqueIndex:idx_join_verification_member"`
	UserID              int64 `gorm:"uniqueIndex:idx_join_verification_member"`
	ChannelID           int64
	Round               string
	Reason              string
	State               string `gorm:"index"`
	StartedAt           time.Time
	Deadline            time.Time `gorm:"index"`
	MessageID           int
	OriginalPermissions string
	OriginalUntil       int64
	RestorePermissions  string
	RestoreUntil        int64
	Muted               bool
	LastError           string
	NotifiedAt          time.Time
	UpdatedAt           time.Time
}

// Persist before acknowledging Telegram updates; replay on restart.
type JoinVerificationEvent struct {
	UpdateID  int  `gorm:"primaryKey;autoIncrement:false"`
	Done      bool `gorm:"index"`
	Payload   string
	CreatedAt time.Time
}

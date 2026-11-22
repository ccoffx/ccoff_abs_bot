package model

import "time"

// Pending entries are separate from participants: they neither deal damage nor
// increase event HP. The unique key also survives process restarts.
type ActivityEntryRequest struct {
	ID                   uint `gorm:"primaryKey"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
	Kind                 string `gorm:"not null;uniqueIndex:ux_activity_entry,priority:1"`
	EventID              string `gorm:"not null;uniqueIndex:ux_activity_entry,priority:2"`
	UserID               int64  `gorm:"not null;uniqueIndex:ux_activity_entry,priority:3"`
	Generation           int    `gorm:"not null;default:1"`
	AbsUserID            string
	UserName             string
	SectID               int64
	MemberID             uint
	ChatID               int64
	MessageID            int
	Status               string `gorm:"not null;index"` // pending / joined / failed
	DeadlineAt           time.Time
	Attempts             int
	LastAttemptAt        *time.Time
	NextAttemptAt        *time.Time
	LastError            string
	CompletedAt          *time.Time
	ResultText           string
	NotificationAttempts int
	NextNotificationAt   *time.Time
	NotifiedAt           *time.Time
}

func (ActivityEntryRequest) TableName() string { return "activity_entry_requests" }

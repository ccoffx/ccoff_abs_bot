package model

import "time"

// AnnouncementDelivery freezes the destination/content of a public notification.
// ParentKey identifies an existing post to edit; retries never resolve config anew.
type AnnouncementDelivery struct {
	ID                uint   `gorm:"primaryKey"`
	Key               string `gorm:"uniqueIndex;not null"`
	Kind              string
	ParentKey         string
	TargetChatID      int64
	ReminderGroupID   int64
	ActorID           int64
	Text              string
	MarkupJSON        string
	State             string `gorm:"index"`
	MessageID         int
	Attempts          int
	RetryAt           time.Time
	LastError         string
	ReminderAttempts  int
	ReminderState     string
	ReminderMessageID int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

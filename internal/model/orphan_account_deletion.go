package model

import "time"

// Retain completed deletions too: a login verified before deletion must never
// commit a binding to the now-deleted ABS identity.
// Bound-account deletion also writes a completed tombstone here.
type OrphanAccountDeletion struct {
	AbsUserID   string `gorm:"primaryKey"`
	ActorID     int64
	CreatedAt   time.Time
	CompletedAt *time.Time
}

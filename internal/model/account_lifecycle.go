package model

import "time"

// AccountLifecycleClaim persists the external operation before contacting ABS.
// A claim survives failures/restarts; its user cannot be renewed or rebound
// until the idempotent external operation and local finalization complete.
type AccountLifecycleClaim struct {
	UserRowID     uint   `gorm:"primaryKey;autoIncrement:false"`
	Kind          string `gorm:"not null"`
	ReferenceID   uint   // Freeze record for a listening-abuse release operation.
	ActorID       int64  // Requested deletion keeps its actor and audit context across retries.
	AuditAction   string
	AuditDetail   string
	CreatedAt     time.Time
	LastAttemptAt *time.Time
	NextRetryAt   *time.Time `gorm:"index"`
	RetryAttempts int        `gorm:"not null;default:0"`
}

type AccountLifecycleNotice struct {
	UserRowID uint   `gorm:"primaryKey;autoIncrement:false"`
	Day       string `gorm:"primaryKey"`
}

package model

import "time"

// These records are deliberately not soft-deleted: mastery and species identity
// survive account removal, trading and the next appearance of the same flame.
type SpiritFire struct {
	ID         uint   `gorm:"primaryKey"`
	Name       string `gorm:"uniqueIndex;not null"`
	Rank       int    `gorm:"uniqueIndex;not null"`
	Grade      int    `gorm:"not null;check:spirit_fire_grade,grade >= 0 AND grade <= 3"`
	OwnerID    int64  `gorm:"index;not null;default:0"`
	ListingID  uint   `gorm:"index;not null;default:0"`
	AcquiredAt time.Time
	GraceAt    *time.Time
}

type SpiritFireMastery struct {
	ID        uint  `gorm:"primaryKey"`
	UserID    int64 `gorm:"uniqueIndex:ux_fire_mastery;not null"`
	FireID    uint  `gorm:"uniqueIndex:ux_fire_mastery;not null"`
	CreatedAt time.Time
}

type SpiritFireWallet struct {
	HoldingsChangedAt time.Time
	UserID            int64 `gorm:"primaryKey;autoIncrement:false"`
	Scrolls           int   `gorm:"not null;default:0"`
	Fragments         int   `gorm:"not null;default:0"`
	SeekingSpent      int64 `gorm:"not null;default:0"`
}

type SpiritFireRules struct {
	ID           uint   `gorm:"primaryKey"`
	EffectiveDay string `gorm:"uniqueIndex;not null"`
	JSON         string `gorm:"not null"`
}

// The singleton both marks launch day and provides a cross-connection SQLite
// write lock before any mutation reads its preconditions.
type SpiritFireState struct {
	ID        uint   `gorm:"primaryKey"`
	LaunchDay string `gorm:"not null"`
}

type SpiritFireLoadout struct {
	ID           uint   `gorm:"primaryKey"`
	UserID       int64  `gorm:"uniqueIndex:ux_fire_loadout;not null"`
	EffectiveDay string `gorm:"uniqueIndex:ux_fire_loadout;not null"`
	FireIDs      string `gorm:"not null"` // JSON array, no order-dependent effects
}

type SpiritFireDaily struct {
	ID      uint   `gorm:"primaryKey"`
	UserID  int64  `gorm:"uniqueIndex:ux_fire_daily;not null"`
	DayKey  string `gorm:"uniqueIndex:ux_fire_daily;not null"`
	FireIDs string
	Percent int
}

// Quotes are server-side, single-use and versioned by the exact expected cost
// and ownership epoch. A failed/stale quote never consumes resources.
type SpiritFireQuote struct {
	ID           string `gorm:"primaryKey"`
	UserID       int64  `gorm:"index"`
	FireID       uint
	Action       string
	Points       int
	Scrolls      int
	RulesJSON    string
	Quantity     int
	ResultJSON   string
	MasteryCount int64
	AcquiredAt   time.Time
	ExpiresAt    time.Time
	Done         bool
}

type SpiritFireEvent struct {
	ChatID     int64
	ID         string `gorm:"primaryKey"` // source + existing activity ID
	RulesJSON  string
	Settled    bool
	ResultJSON string
}

// Notification outbox: recipients can also inspect the persistent result in
// their own panel when Telegram delivery fails.
type SpiritFireNotice struct {
	Attempts      int
	NextAttemptAt *time.Time `gorm:"index"`
	ID            uint       `gorm:"primaryKey"`
	UserID        int64
	Text          string
	Sent          bool `gorm:"index"`
}

package model

import (
	"time"

	"gorm.io/gorm"
)

// ABSCrossDaySessionScanCursor records progressive historical scanning. A
// batch is bounded, but there is deliberately no total page limit: the cursor
// resumes until ABS returns a short page, so histories deeper than 500 sessions
// are eventually observed without blocking normal requests.
type ABSCrossDaySessionScanCursor struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	UserID    int64  `gorm:"index;not null"`
	AbsUserID string `gorm:"uniqueIndex;not null"`

	NextPage         int
	PagesScanned     int
	SessionsObserved int
	Completed        bool       `gorm:"index;not null"`
	LastScannedAt    *time.Time `gorm:"index"`
	CompletedAt      *time.Time `gorm:"index"`
	NextRetryAt      *time.Time `gorm:"index"`
	LastErrorCode    string
}

func (ABSCrossDaySessionScanCursor) TableName() string {
	return "abs_cross_day_session_scan_cursors"
}

type ABSCrossDaySessionAdjustment struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	UserID     int64  `gorm:"index;not null"`
	AbsUserID  string `gorm:"uniqueIndex:ux_abs_cross_day_session,priority:1;index;not null"`
	SessionKey string `gorm:"uniqueIndex:ux_abs_cross_day_session,priority:2;not null"`

	SourceDay        string    `gorm:"index;not null"`
	StartedAt        time.Time `gorm:"index"`
	SessionUpdatedAt time.Time
	ListeningSeconds float64
	AllocationJSON   string
	Status           string    `gorm:"index;not null"`
	Confidence       string    `gorm:"index;not null"`
	Reason           string    `gorm:"index;not null"`
	AlgorithmVersion int       `gorm:"not null"`
	ObservedAt       time.Time `gorm:"index;not null"`
}

func (ABSCrossDaySessionAdjustment) TableName() string {
	return "abs_cross_day_session_adjustments"
}

type ListeningAbuseRecord struct {
	gorm.Model

	UserID    int64  `gorm:"index;not null"`
	AbsUserID string `gorm:"index;not null"`

	DayKey         string `gorm:"index;not null"`
	PreviousDayKey string `gorm:"index"`

	RawHours         float64
	PreviousRawHours float64

	Action string `gorm:"index;not null"` // warning / freeze
	Status string `gorm:"index;not null"` // warning_sent / active / released / release_blocked

	FreezeStartAt *time.Time `gorm:"index"`
	FreezeEndAt   *time.Time `gorm:"index"`
	ReleasedAt    *time.Time `gorm:"index"`

	NoticeError  string
	ReleaseError string
}

func (ListeningAbuseRecord) TableName() string {
	return "listening_abuse_records"
}

type DailyListeningStat struct {
	gorm.Model

	UserID    int64  `gorm:"index;not null"`
	AbsUserID string `gorm:"index"`
	DayKey    string `gorm:"index;not null"` // 北京时间日期 YYYY-MM-DD

	RawSeconds     float64
	CappedSeconds  float64
	EffectiveHours float64
	LastFetchedAt  time.Time `gorm:"index"`
	Source         string
	FetchStatus    string
	RefreshReason  string
}

func (DailyListeningStat) TableName() string {
	return "daily_listening_stats"
}

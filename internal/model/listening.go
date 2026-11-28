package model

import (
	"math"
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

	// A repair scan confirms missing historical sessions without lifting the
	// normal five-page request limit. Evidence and page advancement commit together.
	ReconcileStartedAt     *time.Time
	ReconcileEvidenceJSON  string
	DailyValidationVersion int
	// Only reductions are deferred; additions still commit page by page. Pending
	// observations retain their original timestamps across restarts and retries.
	PendingObservationsJSON string
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
	RetiredDaysJSON  string    // Dates touched by previous versions; retained for cleanup.
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
	Status string `gorm:"index;not null"` // warning_sent / revoked / active / released / release_blocked

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
	Archived  bool   `gorm:"not null;default:false;index"`

	// Official daily policy preserves legacy balances until an explicit repair.
	OfficialBaselineKind            string  `gorm:"not null;default:''"` // legacy_migration / rebind / native / historical_excluded; empty is unknown.
	OfficialPolicy                  bool    `gorm:"not null;default:false"`
	OfficialBaselineSeconds         float64 `gorm:"not null;default:0"`
	OfficialBaselineCreditedSeconds float64 `gorm:"not null;default:0"`
	OfficialBaselineHours           float64 `gorm:"not null;default:0"`

	RawSeconds           float64
	OfficialRawSeconds   float64 // Unmodified listening-days.days value for this date.
	ValidationJSON       string  // Daily credit limits and diagnostics; never penalty evidence.
	CappedSeconds        float64
	BaseCultivationHours float64
	SpiritFireHours      float64
	SectOwnerBonusHours  float64
	EffectiveHours       float64
	LastFetchedAt        time.Time `gorm:"index"`
	Source               string
	FetchStatus          string
	RefreshReason        string

	// Last applied amounts are independent of the mutable display caches.
	AppliedEligible       bool    `gorm:"not null;default:false"`
	CultivationApplied    bool    `gorm:"not null;default:false"`
	AppliedEffectiveHours float64 `gorm:"not null;default:0"`
	AppliedCappedSeconds  float64 `gorm:"not null;default:0"`
	AppliedBaseHours      float64 `gorm:"not null;default:0"`
	AppliedTotalHours     float64 `gorm:"not null;default:0"`
}

func (DailyListeningStat) TableName() string {
	return "daily_listening_stats"
}

// ABS metadata is synchronized separately from report rendering.
type ListeningReportSnapshot struct {
	UserID         int64 `gorm:"primaryKey"`
	AbsUserID      string
	StatsFetchedAt time.Time
	// Last successful official inputs and credited amounts; source mutations
	// invalidate this cache in the same transaction through database triggers.
	OfficialInputJSON string `gorm:"not null;default:''"`
	FinishedBooks     int
	ListeningBooks    int
	ProgressFetchedAt time.Time
}

// BonusListeningSeconds retains the pre-migration bonus basis. Adopting official
// totals must not silently pay the legacy gap through fire or retreat bonuses.
func (s DailyListeningStat) BonusListeningSeconds() float64 {
	if !s.OfficialPolicy {
		return s.CappedSeconds
	}
	return math.Max(0, s.OfficialBaselineCreditedSeconds+(s.CappedSeconds-s.OfficialBaselineSeconds))
}

// ListeningDayClose retains each account/date until its closing sample commits.
type ListeningDayClose struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    int64  `gorm:"not null;uniqueIndex:ux_listening_day_close,priority:1"`
	AbsUserID string `gorm:"not null;uniqueIndex:ux_listening_day_close,priority:2"`
	DayKey    string `gorm:"not null;uniqueIndex:ux_listening_day_close,priority:3"`
	Status    string `gorm:"not null;index"`
}

func (ListeningDayClose) TableName() string { return "listening_day_closes" }

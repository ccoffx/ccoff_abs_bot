package model

import (
	"time"

	"gorm.io/gorm"
)

type SectShopRenewClaim struct {
	gorm.Model

	SectID   int64  `gorm:"index;not null"`
	UserID   int64  `gorm:"index;not null"`
	MonthKey string `gorm:"index;not null"`
	SlotNo   int    `gorm:"not null"`

	PurchaseID uint `gorm:"index"`
}

func (SectShopRenewClaim) TableName() string {
	return "sect_shop_renew_claims"
}

type Sect struct {
	gorm.Model

	Name        string `gorm:"not null"`
	Level       int    `gorm:"default:1"`
	Prestige    int    `gorm:"default:0"`
	Funds       int    `gorm:"default:0"`
	MemberCount int    `gorm:"default:0"`

	OwnerID   int64 `gorm:"index;not null"`
	OwnerName string
}

func (Sect) TableName() string {
	return "sects"
}

type SectMember struct {
	gorm.Model

	SectID int64 `gorm:"index;not null"`
	UserID int64 `gorm:"not null"`

	UserName string
	Role     string `gorm:"index;default:'member'"`

	Contribution       int `gorm:"default:0"`
	WeeklyContribution int `gorm:"default:0"`
	PersonalPrestige   int `gorm:"default:0"`

	JoinedAt       time.Time  `gorm:"index"`
	CaveUnlockedAt *time.Time `gorm:"index"`
}

func (SectMember) TableName() string {
	return "sect_members"
}

type SectContributionLog struct {
	gorm.Model

	SectID int64 `gorm:"index;not null"`
	UserID int64 `gorm:"index;not null"`

	Delta        int
	Reason       string
	RefType      string `gorm:"index"`
	RefID        string `gorm:"index"`
	BalanceAfter int
}

type SectDailyTaskClaim struct {
	gorm.Model

	SectID int64 `gorm:"index;not null"`
	UserID int64 `gorm:"index;not null"`

	DayKey string `gorm:"index;not null"` // YYYY-MM-DD

	CompletedTaskCount int `gorm:"default:0"`
	RewardContribution int `gorm:"default:0"`
	RewardPrestige     int `gorm:"default:0"`
}

func (SectDailyTaskClaim) TableName() string {
	return "sect_daily_task_claims"
}

type SectWeeklyTaskSettlement struct {
	gorm.Model

	SectID  int64  `gorm:"index;not null"`
	WeekKey string `gorm:"index;not null"` // 北京时间自然周周一 YYYY-MM-DD

	SignCount      int64
	ListenHours    float64
	TaskClaimCount int64

	AchievedCount      int
	ExcessPercentTotal int
	RewardFunds        int
	RewardPrestige     int

	SettledByID   int64 `gorm:"index;not null"`
	SettledByName string
}

func (SectWeeklyTaskSettlement) TableName() string {
	return "sect_weekly_task_settlements"
}

type SectListeningDailyProgress struct {
	gorm.Model

	UserID         int64  `gorm:"index;not null"`
	DayKey         string `gorm:"index;not null"` // 北京时间日期 YYYY-MM-DD，对应 ABS days
	RawSeconds     float64
	EffectiveHours float64
	LastFetchedAt  time.Time `gorm:"index"`
}

func (SectListeningDailyProgress) TableName() string {
	return "sect_listening_daily_progresses"
}

type SectShopPurchase struct {
	gorm.Model

	SectID int64 `gorm:"index;not null"`
	UserID int64 `gorm:"index;not null"`

	ExchangeType string `gorm:"index;not null"` // prestige

	CostContribution int `gorm:"default:0"`
	RewardAmount     int `gorm:"default:0"`

	DayKey string `gorm:"index;not null"` // YYYY-MM-DD
}

func (SectShopPurchase) TableName() string {
	return "sect_shop_purchases"
}

type SectCaveRetreat struct {
	gorm.Model

	SectID int64 `gorm:"index;not null"`
	UserID int64 `gorm:"index;not null"`

	Mode string `gorm:"index;not null"` // personal / sect

	StartAt time.Time `gorm:"index;not null"`
	EndAt   time.Time `gorm:"index;not null"`

	BaseRawSeconds float64
	// Durable session observations used to prove playback inside the retreat.
	// Late playback outside StartAt/EndAt never contributes to the bonus.
	ListeningEvidenceJSON string

	PersonalPrestigeCost int `gorm:"default:0"`
	SectPrestigeCost     int `gorm:"default:0"`

	StartedByID   int64  `gorm:"index;not null"`
	StartedByName string `gorm:"index"`

	Status string `gorm:"index;not null;default:'active'"`
}

func (SectCaveRetreat) TableName() string {
	return "sect_cave_retreats"
}

func (SectContributionLog) TableName() string {
	return "sect_contribution_logs"
}

type SectHornBroadcast struct {
	gorm.Model

	HornID string `gorm:"not null"`

	Scope string `gorm:"index;not null"` // sect / world

	SectID   int64  `gorm:"index"`
	SectName string `gorm:"index"`

	SenderID   int64  `gorm:"index;not null"`
	SenderName string `gorm:"index"`
	SenderRole string `gorm:"index"`

	Cost    int
	Content string `gorm:"not null"`

	RecipientCount int
	SuccessCount   int
	FailedCount    int

	Status      string     `gorm:"index;not null"`
	SubmittedAt time.Time  `gorm:"index;not null"`
	CompletedAt *time.Time `gorm:"index"`
	LastError   string
}

func (SectHornBroadcast) TableName() string {
	return "sect_horn_broadcasts"
}

type SectHornDelivery struct {
	gorm.Model

	HornID string `gorm:"index;not null"`

	UserID   int64  `gorm:"index;not null"`
	UserName string `gorm:"index"`

	Status string `gorm:"index;not null"`
	Error  string
	SentAt *time.Time `gorm:"index"`
}

func (SectHornDelivery) TableName() string {
	return "sect_horn_deliveries"
}

type SectLottery struct {
	gorm.Model

	SectID      int64  `gorm:"index;not null"`
	SectName    string `gorm:"index"`
	CreatorID   int64  `gorm:"index;not null"`
	CreatorName string

	Title  string `gorm:"not null"`
	Status string `gorm:"index;not null"`
	Mode   string `gorm:"index;not null"`

	TargetEntryCount int
	DrawAt           *time.Time `gorm:"index"`
	DrawnAt          *time.Time `gorm:"index"`

	PrizeCount  int
	EntryCount  int
	WinnerCount int
	ResultNote  string
}

func (SectLottery) TableName() string { return "sect_lotteries" }

type SectLotteryEntry struct {
	gorm.Model

	LotteryID uint  `gorm:"index;not null"`
	SectID    int64 `gorm:"index;not null"`
	UserID    int64 `gorm:"index;not null"`
	UserName  string

	HistoricalContribution int
	JoinedAt               time.Time `gorm:"index;not null"`
}

func (SectLotteryEntry) TableName() string { return "sect_lottery_entries" }

type SectLotteryPrize struct {
	gorm.Model

	LotteryID uint   `gorm:"index;not null"`
	CodeEnc   string `gorm:"not null"`
	Preview   string
	Status    string `gorm:"index;not null"`

	AssignedUserID int64 `gorm:"index"`
	DeliveredAt    *time.Time
	DeliveryError  string
}

func (SectLotteryPrize) TableName() string { return "sect_lottery_prizes" }

type SectLotteryWinner struct {
	gorm.Model

	LotteryID uint  `gorm:"index;not null"`
	SectID    int64 `gorm:"index;not null"`
	PrizeID   uint  `gorm:"index;not null"`
	UserID    int64 `gorm:"index;not null"`
	UserName  string
	Status    string `gorm:"index;not null"`

	DeliveryError string
	DeliveredAt   *time.Time
}

func (SectLotteryWinner) TableName() string { return "sect_lottery_winners" }

type SectLotteryReminder struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	LotteryID uint  `gorm:"index;not null"`
	SectID    int64 `gorm:"index;not null"`
	UserID    int64 `gorm:"index;not null"`
	UserName  string
	Status    string `gorm:"index;not null"`

	LastError string
	SentAt    *time.Time
}

func (SectLotteryReminder) TableName() string { return "sect_lottery_reminders" }

type SectSecretRealmEvent struct {
	gorm.Model

	RealmID string `gorm:"not null"`
	SectID  int64  `gorm:"index;not null"`
	ChatID  int64  `gorm:"index"`

	Name           string
	ProfileKey     string `gorm:"index"`
	ProfileName    string
	ConfigSnapshot string
	Status         string `gorm:"index;not null"` // active / settling / settled

	OpenedByID   int64 `gorm:"index;not null"`
	OpenedByName string

	StartAt time.Time `gorm:"index;not null"`
	EndAt   time.Time `gorm:"index;not null"`

	FundsCost int

	ParticipantCount        int
	TotalDeltaHours         float64
	TotalRewardPoints       int
	TotalRewardContribution int
	TotalRewardPrestige     int
	TotalRewardDrops        int

	GuardianUserID       int64
	GuardianName         string
	GuardianMajorRealm   int
	GuardianMinorRealm   int
	GuardianBonusPercent int

	SettledAt *time.Time

	BoardChatID    int64
	BoardMessageID int
}

func (SectSecretRealmEvent) TableName() string {
	return "sect_secret_realm_events"
}

type SectSecretRealmParticipant struct {
	gorm.Model

	RealmID string `gorm:"index;not null"`
	SectID  int64  `gorm:"index;not null"`
	UserID  int64  `gorm:"index;not null"`

	UserName   string
	MajorRealm int
	MinorRealm int

	BaseHours               float64
	FinalHours              float64
	DeltaHours              float64
	BaseRawSeconds          float64
	FinalRawSeconds         float64
	RawDeltaSeconds         float64
	ObservedRawDeltaSeconds float64
	WallClockCapSeconds     float64
	SuppressedHours         float64
	RawCapped               bool

	RewardPoints             int
	RewardContribution       int
	RewardPrestige           int
	PointBonusPercent        int
	ContributionBonusPercent int
	PrestigeBonusPercent     int
	GuardianBonusPercent     int
	RewardDropItem           string
	RewardDropQuantity       int

	IsRewarded bool `gorm:"default:false"`
}

func (SectSecretRealmParticipant) TableName() string {
	return "sect_secret_realm_participants"
}

type SectTechnology struct {
	gorm.Model

	SectID  int64  `gorm:"index;not null"`
	TechKey string `gorm:"index;not null"`
	Level   int    `gorm:"default:0"`
}

func (SectTechnology) TableName() string {
	return "sect_technologies"
}

type SectTechnologyLog struct {
	gorm.Model

	SectID   int64 `gorm:"index;not null"`
	UserID   int64 `gorm:"index;not null"`
	UserName string

	TechKey  string `gorm:"index;not null"`
	OldLevel int
	NewLevel int

	FundsCost    int
	PrestigeCost int
}

func (SectTechnologyLog) TableName() string {
	return "sect_technology_logs"
}

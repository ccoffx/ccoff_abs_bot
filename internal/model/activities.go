package model

import (
	"time"

	"gorm.io/gorm"
)

type RedPacket struct {
	ID          string `gorm:"primaryKey"`
	SenderID    int64  `gorm:"not null"`
	SenderName  string
	TotalPoints int  `gorm:"not null"`
	Count       int  `gorm:"not null"`
	LeftCount   int  `gorm:"not null"`
	LeftPoints  int  `gorm:"not null"`
	IsFinished  bool `gorm:"default:false;index"`
	CreatedAt   time.Time
	RefType     string `gorm:"index"`
	RefID       string `gorm:"index"`
	ClaimScope  string `gorm:"index"`
}

// 抢红包明细防刷记录。
// 同一红包每人限领一次，由 internal/storage/schema.go 中的唯一索引保证。
type RedPacketGrab struct {
	ID          uint   `gorm:"primaryKey"`
	PacketID    string `gorm:"index;not null"`
	UserID      int64  `gorm:"index;not null"`
	GrabberName string
	Points      int
	GrabbedAt   time.Time
}

type LotteryActivity struct {
	gorm.Model

	Title                   string `gorm:"index;not null"`
	Mode                    string `gorm:"index;not null"`
	Status                  string `gorm:"index;not null;default:'active'"`
	CreatedByID             int64  `gorm:"index;not null"`
	CreatedBy               string
	AnnounceChatID          int64 `gorm:"index"`
	AnnounceIsChannel       bool
	AnnounceReminderGroupID int64

	IntroMessageID  int
	ResultMessageID int
	IntroPinned     bool `gorm:"default:false"`
	ResultPinned    bool `gorm:"default:false"`
	PinsUnpinned    bool `gorm:"default:false;index"`

	EntryCost         int
	TotalEntryCost    int
	TotalRefundPoints int
	FusionPoolPoints  int

	ParticipantLimit int
	DrawAt           *time.Time `gorm:"index"`
	DrawnAt          *time.Time `gorm:"index"`

	ClaimDeadlineAt *time.Time `gorm:"index"`

	WinnersCount int
	ResultNote   string
}

func (LotteryActivity) TableName() string {
	return "lottery_activities"
}

type LotteryPrize struct {
	gorm.Model

	ActivityID      uint   `gorm:"index;not null"`
	PrizeType       string `gorm:"index;not null"`
	Amount          int
	Quantity        int
	DisplayName     string
	SecretEncrypted string
}

func (LotteryPrize) TableName() string {
	return "lottery_prizes"
}

type LotteryParticipant struct {
	gorm.Model

	ActivityID   uint  `gorm:"index;not null"`
	UserID       int64 `gorm:"index;not null"`
	UserName     string
	JoinedAt     time.Time `gorm:"index;not null"`
	EntryCost    int
	RefundPoints int
	IsRefunded   bool `gorm:"default:false"`
	RefundedAt   *time.Time
}

func (LotteryParticipant) TableName() string {
	return "lottery_participants"
}

type LotteryWinner struct {
	gorm.Model

	ActivityID uint  `gorm:"index;not null"`
	UserID     int64 `gorm:"index;not null"`
	UserName   string

	PrizeID               uint
	PrizeType             string `gorm:"index;not null"`
	Amount                int
	Status                string `gorm:"index;not null;default:'pending'"`
	ClaimedAt             *time.Time
	PrizeCodePreview      string
	RewardResultEncrypted string
	RewardDeliveredAt     *time.Time
}

func (LotteryWinner) TableName() string {
	return "lottery_winners"
}

type LotteryClaimLog struct {
	gorm.Model

	ActivityID uint  `gorm:"index;not null"`
	WinnerID   uint  `gorm:"index"`
	UserID     int64 `gorm:"index;not null"`
	Action     string
	Detail     string
}

func (LotteryClaimLog) TableName() string {
	return "lottery_claim_logs"
}

// 不使用软删除：每局每人、每箱的唯一约束在历史记录中同样有效。
type TreasureRound struct {
	ID           uint `gorm:"primaryKey"`
	CreatedAt    time.Time
	ChatID       int64  `gorm:"index;not null"`
	DayKey       string `gorm:"index;not null"`
	Status       string `gorm:"index;not null"`
	EntryCost    int
	RewardsJSON  string `gorm:"not null"`
	MessageID    int
	ExpiresAt    time.Time `gorm:"index;not null"`
	ClosedAt     time.Time
	PoolInjected int
	PoolAfter    int
}

type TreasurePick struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	RoundID   uint  `gorm:"uniqueIndex:idx_treasure_pick_box,priority:1;uniqueIndex:idx_treasure_pick_user,priority:1;not null"`
	BoxIndex  int   `gorm:"uniqueIndex:idx_treasure_pick_box,priority:2;not null"`
	UserID    int64 `gorm:"uniqueIndex:idx_treasure_pick_user,priority:2;not null"`
	UserName  string
	Cost      int
	Reward    int
}

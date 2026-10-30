package model

import (
	"time"

	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	TelegramID      int64  `gorm:"uniqueIndex;not null"`
	Username        string `gorm:"uniqueIndex;not null"`
	DisplayName     string // 显示名缓存（Telegram @用户名>首名>ID，每次钱包事务尽力刷新），供群公告按真人称呼；非唯一
	AbsUserID       string `gorm:"not null"`
	SecurityCode    string `gorm:"not null"`
	Status          string `gorm:"default:'active'"`
	ExpireAt        *time.Time
	Points          int `gorm:"default:0"`
	LastSignAt      *time.Time
	IsWhitelist     bool `gorm:"default:false"`
	WhitelistExpire *time.Time
	Role            string `gorm:"default:'user'"`
	AccountType     string `gorm:"index;default:'formal'"`
	TrialStartedAt  *time.Time
	TrialEndsAt     *time.Time
	IsSuspended     bool `gorm:"default:false"` // 记录该账号是否已被系统封禁
}

type InviteCode struct {
	gorm.Model

	// CodeHash 用于验证用户输入的邀请码。
	CodeHash string `gorm:"index"`

	// CodePreview 只保存脱敏展示，例如 abcd****wxyz。
	CodePreview string

	IsUsed   bool `gorm:"default:false"`
	UsedByID int64

	OwnerUserID int64  `gorm:"index"`
	Source      string `gorm:"index"`

	// Cash-out is a terminal consumption path distinct from account redemption.
	CashedOutAt   *time.Time `gorm:"index"`
	CashedOutByID int64      `gorm:"index"`
	CashoutPoints int
}

type RenewCode struct {
	gorm.Model

	// CodeHash 用于验证用户输入的续期卡。
	CodeHash string `gorm:"index"`

	// CodePreview 只保存脱敏展示，例如 R30-****abcd。
	CodePreview string

	Days     int
	IsUsed   bool `gorm:"default:false"`
	UsedByID int64

	// Point-exchanged and marketplace-traded codes are restricted to their owner.
	OwnerUserID int64  `gorm:"index"`
	Source      string `gorm:"index"`

	// Cash-out is a terminal consumption path distinct from account redemption.
	CashedOutAt   *time.Time `gorm:"index"`
	CashedOutByID int64      `gorm:"index"`
	CashoutPoints int
}

type ReferralCode struct {
	gorm.Model

	UserID    int64  `gorm:"index;not null"`
	Code      string `gorm:"index;not null"`
	IsEnabled bool   `gorm:"default:true;index"`
}

func (ReferralCode) TableName() string {
	return "referral_codes"
}

type ReferralActivation struct {
	gorm.Model

	CodeID    uint  `gorm:"index;not null"`
	InviterID int64 `gorm:"index;not null"`
	InviteeID int64 `gorm:"index;not null"`

	Status string `gorm:"index;not null;default:'active'"`

	TrialStartedAt time.Time  `gorm:"index;not null"`
	TrialEndsAt    time.Time  `gorm:"index;not null"`
	EffectiveAt    *time.Time `gorm:"index"`
	ExtendedAt     *time.Time
	RewardedAt     *time.Time

	// TrialEndDaySeconds 到期日当天、试用中最后一次自动结算扫描刷新的听书秒数快照。
	// 生命周期巡检每日 03:00 才停用到期账号，到期日整日聚合值包含到期后的听书；
	// 到期后的补结算用该快照替代到期日整日聚合值，防止到期后的听书计入新人任务。
	TrialEndDaySeconds float64

	RawSecondsAtEffective float64
	RewardPoints          int
	ActivationDayKey      string `gorm:"index"`
	RewardDayKey          string `gorm:"index"`
	RewardMonthKey        string `gorm:"index"`
}

func (ReferralActivation) TableName() string {
	return "referral_activations"
}

type ReferralDailyActivationQuota struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	InviterID       int64  `gorm:"index;not null"`
	DayKey          string `gorm:"index;not null"`
	ActivationCount int    `gorm:"not null;default:0"`
}

func (ReferralDailyActivationQuota) TableName() string {
	return "referral_daily_activation_quotas"
}

type ReferralMonthlyRewardQuota struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	InviterID    int64  `gorm:"index;not null"`
	MonthKey     string `gorm:"index;not null"`
	RewardPoints int    `gorm:"not null;default:0"`
}

func (ReferralMonthlyRewardQuota) TableName() string {
	return "referral_monthly_reward_quotas"
}

// WhitelistCode grants the existing permanent whitelist privilege once.
// Raw codes are only returned privately to the generating administrator or lottery winner.
type WhitelistCode struct {
	gorm.Model
	CodeHash    string `gorm:"uniqueIndex;not null"`
	CodePreview string
	CreatedByID int64
	IsUsed      bool `gorm:"default:false;not null"`
	UsedByID    int64
	UsedAt      *time.Time
}

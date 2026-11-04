package model

import (
	"time"

	"gorm.io/gorm"
)

// 全局连续签到状态表。
// 维护用户当前连续签到状态。
// 30 天为一个奖励周期，CycleSeq 用于区分第几轮，防止断签后奖励 ref_id 冲突。
type SignInStreak struct {
	gorm.Model

	UserID int64 `gorm:"index;not null"`

	CurrentStreakDays int `gorm:"default:0"` // 当前连续签到天数
	LongestStreakDays int `gorm:"default:0"` // 历史最长连续签到天数，可用于后续排行榜
	TotalSignDays     int `gorm:"default:0"` // 累计签到总天数

	LastSignDate string     `gorm:"index"` // YYYY-MM-DD
	LastSignAt   *time.Time // 完整签到时间

	CycleSeq   int `gorm:"default:1"` // 奖励周期序号，断签或跨过 30 天周期时递增
	BreakCount int `gorm:"default:0"` // 断签次数，后续可用于统计
}

func (SignInStreak) TableName() string {
	return "sign_in_streaks"
}

// 每日签到日志表。
// 用于防止同一天重复签到，并为后续签到日历、补签、审计留数据基础。
type SignInLog struct {
	gorm.Model

	UserID int64 `gorm:"index;not null"`

	SignDate string    `gorm:"index;not null"` // YYYY-MM-DD
	SignAt   time.Time `gorm:"index;not null"`

	BasePoints int `gorm:"default:0"`

	StreakDaysAfter int `gorm:"default:0"` // 签到后的连续天数
	CycleSeq        int `gorm:"default:1"` // 签到后所在奖励周期
	DayInCycle      int `gorm:"default:1"` // 当前 30 天周期内第几天
}

func (SignInLog) TableName() string {
	return "sign_in_logs"
}

// 连签奖励领取记录表。
// 用 ref_id 做幂等保护，防止同一用户同一轮同一档位重复发奖。
type SignInRewardClaim struct {
	gorm.Model

	UserID int64 `gorm:"index;not null"`

	RewardType    string `gorm:"index;not null"` // cycle_streak
	CycleSeq      int    `gorm:"index;not null"`
	MilestoneDays int    `gorm:"index;not null"` // 3 / 7 / 14 / 21 / 30

	Points      int
	Description string

	RefID     string    `gorm:"index;not null"`
	ClaimedAt time.Time `gorm:"index;not null"`
}

func (SignInRewardClaim) TableName() string {
	return "sign_in_reward_claims"
}

// 用户积分流水。
// 只记录用户积分变化，不记录系统奖池水位。
// Delta > 0 表示积分增加，Delta < 0 表示积分减少。
type PointTransaction struct {
	gorm.Model

	UserID   int64  `gorm:"index;not null"`
	UserName string `gorm:"index"`

	Type string `gorm:"index;not null"` // sign_in / redpacket_send / treasure_cost 等

	Delta         int
	BalanceBefore int
	BalanceAfter  int

	Description string
	RefType     string `gorm:"index"`
	RefID       string `gorm:"index"`
}

func (PointTransaction) TableName() string {
	return "point_transactions"
}

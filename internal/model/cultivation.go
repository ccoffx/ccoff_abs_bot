package model

import (
	"time"

	"gorm.io/gorm"
)

// ==========================================
// 📿 昔风听书斗气系统专属数据表
// ==========================================
type Cultivation struct {
	ID               uint      `gorm:"primaryKey"`
	UserID           int64     `gorm:"index"`     // 绑定 TelegramID
	TotalAudioTime   float64   `gorm:"default:0"` // 🎧 累计听书时长（小时）- 纯闭关苦修
	PillAudioTime    float64   `gorm:"default:0"` // 💊 累计丹药时长（小时）- 氪金药力加成
	MajorRealm       int       `gorm:"default:0"` // 大境界编号 0-13：斗之气至斗帝
	MinorRealm       int       `gorm:"default:1"` // 小段位 (1:初期, 2:中期, 3:后期, 4:圆满)
	TribulationFails int       `gorm:"default:0"` // 渡劫失败次数计数器（暗藏天道保底）
	ConsolidateUntil time.Time // 境界巩固期（防止老玩家光速吃药连升的冷却时间）
}

// BreakthroughAttempt 记录每一次突破尝试。
// 作用：
// 1. 审计随机 roll、成功率、是否保底；
// 2. 记录消耗的积分 / 丹药、失败惩罚、成功返还；
// 3. 方便用户争议和运维排障。
type BreakthroughAttempt struct {
	gorm.Model

	UserID int64 `gorm:"index;not null"`

	FromMajor int
	FromMinor int
	ToMajor   int
	ToMinor   int

	Mode     string `gorm:"index"` // USE_INVENTORY / AUTO_BUY
	PillName string `gorm:"index"`

	PointsCost   int
	PillConsumed bool

	SuccessRate       float64
	Roll              int
	IsGuaranteed      bool
	BaseSuccessRate   float64
	GuardCount        int
	GuardBonusPercent int

	Result string `gorm:"index"` // success / failed
	Status string `gorm:"index"` // settled

	ResourceCostPoints int
	FailPenaltyPoints  int
	RefundPoints       int

	Detail string
}

// A confirmed breakthrough waits for volunteers; all state survives restarts.
type BreakthroughRitual struct {
	gorm.Model
	UserID           int64 `gorm:"index;not null"`
	UserName         string
	OriginChatID     int64
	ChatID           int64
	MessageID        int
	Mode             string
	FromMajor        int
	TribulationFails int
	RulesJSON        string
	Status           string    `gorm:"index;not null"`
	Deadline         time.Time `gorm:"index"`
	AttemptID        uint
}

type BreakthroughGuard struct {
	gorm.Model
	RitualID uint  `gorm:"uniqueIndex:idx_breakthrough_guard_user;not null"`
	UserID   int64 `gorm:"uniqueIndex:idx_breakthrough_guard_user;index;not null"`
	UserName string
	DayKey   string `gorm:"index;not null"`
	Status   string `gorm:"index;not null"`
}

func (BreakthroughAttempt) TableName() string {
	return "breakthrough_attempts"
}

type CultivationRealmConfig struct {
	gorm.Model

	MajorRealm int    `gorm:"not null"`
	Name       string `gorm:"not null"`
	TitleName  string

	MinTotalHours float64 `gorm:"default:0"`
	MaxTotalHours float64 `gorm:"default:0"`

	IsMaxRealm bool `gorm:"default:false"`
	Enabled    bool `gorm:"default:true"`
}

func (CultivationRealmConfig) TableName() string {
	return "cultivation_realm_configs"
}

type CultivationMinorRealmConfig struct {
	gorm.Model

	MajorRealm int `gorm:"index;not null"`
	MinorRealm int `gorm:"not null"`

	Name          string  `gorm:"not null"`
	RequiredHours float64 `gorm:"not null"`

	Enabled bool `gorm:"default:true"`
}

func (CultivationMinorRealmConfig) TableName() string {
	return "cultivation_minor_realm_configs"
}

type BreakthroughConfig struct {
	gorm.Model

	FromMajorRealm int `gorm:"not null"`
	ToMajorRealm   int `gorm:"not null"`

	PillName   string
	PointsCost int

	MinTotalHours float64 `gorm:"not null"`
	SuccessRate   float64 `gorm:"not null"`

	CooldownHours int `gorm:"default:0"`

	GuaranteeFailCount int     `gorm:"default:3"`
	RefundRate         float64 `gorm:"default:0.2"`
	FailPenaltyPoints  int     `gorm:"default:50"`

	Enabled bool `gorm:"default:true"`
}

func (BreakthroughConfig) TableName() string {
	return "breakthrough_configs"
}

// 确认单的境界和成本由服务端生成；确认成功后永久保留，兼作兑换流水和幂等凭据。
type CultivationTreasureExchange struct {
	gorm.Model
	UserID         int64 `gorm:"index;not null"`
	FromMajorRealm int
	TreasureName   string
	TokenCost      int
	Status         string `gorm:"not null"`
}

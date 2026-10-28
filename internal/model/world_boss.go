package model

import (
	"time"

	"gorm.io/gorm"
)

type WorldBossEvent struct {
	gorm.Model
	BossID string `gorm:"not null"`
	Name   string
	ChatID int64 `gorm:"index"`

	Status  string    `gorm:"index"` // active / settling / settled
	StartAt time.Time `gorm:"index"`
	EndAt   time.Time `gorm:"index"`

	MaxHP     int
	CurrentHP float64
	IsKilled  bool

	ParticipantCount int
	SettledAt        *time.Time

	BoardChatID    int64
	BoardMessageID int
}

func (WorldBossEvent) TableName() string { return "world_boss_events" }

type WorldBossParticipant struct {
	gorm.Model
	BossID   string `gorm:"index;not null"`
	UserID   int64  `gorm:"index;not null"`
	UserName string

	BaseHours  float64
	FinalHours float64
	DeltaHours float64

	BaseDamage float64
	Multiplier float64
	Damage     float64

	RewardPoints       int
	RewardContribution int
	RewardPrestige     int
	IsRewarded         bool `gorm:"default:false"`
}

func (WorldBossParticipant) TableName() string { return "world_boss_participants" }

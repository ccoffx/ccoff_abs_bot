package model

import "time"

// 游戏和荣誉不软删除，历史局的参与、结算和唯一性约束始终有效。
type TribulationRound struct {
	ID            uint `gorm:"primaryKey"`
	CreatedAt     time.Time
	ChatID        int64 `gorm:"index;not null"`
	ChatName      string
	Status        string `gorm:"index;not null"`
	MessageID     int
	EntryCost     int
	JoinUntil     time.Time
	SwitchUntil   time.Time
	ExpiresAt     time.Time `gorm:"index;not null"`
	ClosedAt      time.Time
	WinningSide   string
	TitleEligible bool
	PanelDirty    bool `gorm:"index"`
}

type TribulationEntry struct {
	ID          uint  `gorm:"primaryKey"`
	RoundID     uint  `gorm:"uniqueIndex:idx_tribulation_entry_user,priority:1;not null"`
	UserID      int64 `gorm:"uniqueIndex:idx_tribulation_entry_user,priority:2;not null"`
	UserName    string
	JoinedAt    time.Time
	InitialSide string
	Side        string
	SwitchedAt  *time.Time
	Cost        int
	Reward      int
}

type TribulationTitleStat struct {
	ChatID         int64  `gorm:"primaryKey;autoIncrement:false"`
	UserID         int64  `gorm:"primaryKey;autoIncrement:false"`
	TitleKey       string `gorm:"primaryKey"`
	UserName       string
	Count          int
	LastAchievedAt time.Time
	LastEntryID    uint
}

type TribulationTitleHolder struct {
	ChatID     int64  `gorm:"primaryKey;autoIncrement:false"`
	TitleKey   string `gorm:"primaryKey"`
	UserID     int64
	UserName   string
	Count      int
	AcquiredAt time.Time
}

type TribulationTitleHistory struct {
	ChatID          int64  `gorm:"primaryKey;autoIncrement:false"`
	UserID          int64  `gorm:"primaryKey;autoIncrement:false"`
	TitleKey        string `gorm:"primaryKey"`
	FirstAcquiredAt time.Time
}

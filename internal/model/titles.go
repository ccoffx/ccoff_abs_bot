package model

import "time"

type TitleGroup struct {
	ChatID int64 `gorm:"primaryKey;autoIncrement:false"`
	Name   string
}

type TitleMember struct {
	ChatID int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID int64 `gorm:"primaryKey;autoIncrement:false"`
}

// RankTitleHolder 的主键确保每个榜首称号全服只有一个持有者。
// SubjectID 记录宗门，宗门并列时保留原第一宗门，宗主变更仍转移称号。
type RankTitleHolder struct {
	TitleKey   string `gorm:"primaryKey"`
	UserID     int64  `gorm:"index"`
	UserName   string
	SubjectID  uint
	PeriodKey  string // 月榜 YYYY-MM，周榜为北京时间周一日期；长期榜为空。
	Score      float64
	AcquiredAt time.Time
}

type RankTitleHistory struct {
	ID           uint   `gorm:"primaryKey"`
	TitleKey     string `gorm:"index"`
	UserID       int64  `gorm:"index"`
	SubjectID    uint
	PeriodKey    string
	AcquiredAt   time.Time
	ReleasedAt   *time.Time
	NoticeState  string `gorm:"index"` // pending / sending / sent / failed / uncertain / obsolete
	NextNoticeAt time.Time
}

// Version 是期望的佩戴版本，SyncedVersion 是已应用的版本。
// 外部请求失败或途中称号易主时，后台根据最新版本继续同步。
type TitleEquipment struct {
	ChatID        int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID        int64 `gorm:"primaryKey;autoIncrement:false"`
	TitleKey      string
	DesiredTag    string
	Version       int64
	SyncedVersion int64
	AppliedTag    string
	AttemptedTag  string
	// 保留所有尚未确认的写入意图，避免连续失败覆盖较早的外部成功记录。
	UnconfirmedTagsJSON string
	NextSyncAt          time.Time
	// 服务端限流独立于佩戴版本，切换称号与重启都不能跳过等待。
	RateLimitUntil time.Time
	SyncHint       string
}

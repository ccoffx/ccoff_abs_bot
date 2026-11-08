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

// TitleMemberSyncRequest coalesces identity changes into a durable resync intent.
// Version prevents an in-flight worker from deleting a newer event.
type TitleMemberSyncRequest struct {
	ChatID        int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID        int64 `gorm:"primaryKey;autoIncrement:false"`
	GroupName     string
	Version       int64
	NextAttemptAt time.Time `gorm:"index"`
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
	// 兼容旧版普通成员标签的写入意图；新版按字段使用 OwnedTagsJSON。
	UnconfirmedTagsJSON string
	// 按 Telegram 字段保存已确认和可能成功的写入，防止身份切换后
	// 用普通成员标签的记录误删管理员手动头衔（或反过来）。
	OwnedTagsJSON string
	NextSyncAt    time.Time
	// 服务端限流独立于佩戴版本，切换称号与重启都不能跳过等待。
	RateLimitUntil time.Time
	SyncHint       string
}

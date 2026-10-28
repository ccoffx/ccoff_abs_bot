package model

import (
	"time"

	"gorm.io/gorm"
)

// 全局动态配置表
type SystemConfig struct {
	gorm.Model
	Key   string `gorm:"not null"`
	Value string
}

// 管理员审计日志。
// 所有高危操作都应该写入这里，方便事后追责和回滚核查。
type AuditLog struct {
	gorm.Model
	ActorID   int64  `gorm:"index;not null"` // 操作者 Telegram ID
	ActorRole string `gorm:"index"`          // 操作者角色
	Action    string `gorm:"index;not null"` // 操作类型
	Target    string `gorm:"index"`          // 操作对象
	Delta     int    `gorm:"default:0"`      // 积分变动等数值型变化，没有则为 0
	Detail    string // 详细说明，包含原因、旧值、新值等
}

// 安全校验失败锁定表。
// 持久化安全码错误次数，保证服务重启后锁定仍然生效。
type SecurityAttemptLock struct {
	gorm.Model

	UserID  int64  `gorm:"index;not null"`
	Purpose string `gorm:"index;not null"`

	FailCount   int `gorm:"default:0"`
	LockedUntil *time.Time
	LastFailAt  *time.Time
}

func (SecurityAttemptLock) TableName() string {
	return "security_attempt_locks"
}

type AutoDeleteMsg struct {
	ID        uint  `gorm:"primaryKey"`
	ChatID    int64 `gorm:"index"`
	MessageID int
	DeleteAt  time.Time `gorm:"index"`
}

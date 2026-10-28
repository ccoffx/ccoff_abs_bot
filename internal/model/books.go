package model

import (
	"time"

	"gorm.io/gorm"
)

// BookLibraryEvent is a local persistent snapshot of ABS library items that have
// appeared in recent scans. It lets the book-request announcement flow find books
// even after the original 20-minute ABS "recent added" window has passed.
type BookLibraryEvent struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	ItemID      string `gorm:"uniqueIndex;not null"`
	LibraryID   string `gorm:"index;not null"`
	LibraryName string `gorm:"index;not null"`
	Title       string `gorm:"index;not null"`
	Narrators   string
	RecentAt    time.Time `gorm:"index;not null"`
	Source      string    `gorm:"index;not null;default:'scan'"`
	FirstSeenAt time.Time `gorm:"index;not null"`
}

func (BookLibraryEvent) TableName() string {
	return "book_library_events"
}

type BookRequestAnnouncementDelivery struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	RequestID    uint   `gorm:"uniqueIndex;not null"`
	ItemID       string `gorm:"not null"`
	TargetChatID int64  `gorm:"index;not null"`
	Status       string `gorm:"index;not null"`
	RequestedBy  int64  `gorm:"index;not null"`

	TelegramMessageID int
	AttemptCount      int
	LeaseUntil        *time.Time `gorm:"index"`
	LastErrorCode     string
	SentAt            *time.Time `gorm:"index"`
	ResolvedAt        *time.Time `gorm:"index"`
	ResolvedBy        int64      `gorm:"index"`
	Resolution        string
}

func (BookRequestAnnouncementDelivery) TableName() string {
	return "book_request_announcement_deliveries"
}

// BookRequestAnnouncementCandidateSnapshot persists a stable candidate list so
// pagination survives process restarts. CandidateJSON contains only public ABS
// metadata used by the announcement preview; it never contains credentials.
type BookRequestAnnouncementCandidateSnapshot struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	Token         string    `gorm:"uniqueIndex;not null"`
	RequestID     uint      `gorm:"index;not null"`
	CandidateJSON string    `gorm:"type:text;not null"`
	ExpiresAt     time.Time `gorm:"index;not null"`
}

func (BookRequestAnnouncementCandidateSnapshot) TableName() string {
	return "book_request_announcement_candidate_snapshots"
}

// BookRequestAnnouncementPreviewCandidate persists the opaque callback token
// used by selection and publish buttons. Keeping item IDs server-side avoids
// exceeding Telegram's 64-byte callback limit and survives restarts.
// CandidateJSON stores the selected item's public ABS metadata snapshot so
// preview and publish do not require a second ABS read after selection.
type BookRequestAnnouncementPreviewCandidate struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	Token         string    `gorm:"uniqueIndex;not null"`
	RequestID     uint      `gorm:"index;not null"`
	ItemID        string    `gorm:"not null"`
	CandidateJSON string    `gorm:"type:text"`
	ExpiresAt     time.Time `gorm:"index;not null"`
}

func (BookRequestAnnouncementPreviewCandidate) TableName() string {
	return "book_request_announcement_preview_candidates"
}

// 用户求书工单表。
// Status:
// pending   = 待接单
// claimed   = 已接单/处理中
// need_info = 需要用户补充信息
// uploaded  = 已上传
// rejected  = 暂无资源
// cancelled = 已取消（用户撤回 / 管理员取消 / need_info 超时自动取消）
//
// 退款规则：
// cancelled 与 rejected 均按 CostPaid 退还积分；uploaded 不退。
// CostPaid 为创建工单时实际扣除的积分（按用户当时档位记录，退款时不按当前档位重算）。
// RefundedAt 非空表示已退还，防止重复退款。
// RemindCount 为当前阶段的滞留提醒次数，状态发生转移时重置为 0。
type BookRequest struct {
	gorm.Model

	UserID   int64 `gorm:"index;not null"`
	UserName string

	XmlyLink  string `gorm:"not null"`
	UserNote  string
	AdminNote string

	Status string `gorm:"index;not null;default:'pending'"`

	// AdminID/AdminName：处理该工单的管理员。对无管理员参与的取消（用户自行取消 /
	// 超时自动取消），AdminName 记录取消来源文案（“用户取消”/“系统超时取消”），仅用于展示。
	AdminID   int64
	AdminName string

	AssigneeID   int64 `gorm:"index"`
	AssigneeName string
	ClaimedAt    *time.Time
	LastActionAt *time.Time `gorm:"index"`

	AdminChatID    int64
	AdminMessageID int

	CompletedAt *time.Time

	CostPaid     int        `gorm:"not null;default:0"`
	RefundedAt   *time.Time `gorm:"index"`
	RemindCount  int        `gorm:"not null;default:0"`
	LastRemindAt *time.Time
}

func (BookRequest) TableName() string {
	return "book_requests"
}

// 求书工单处理日志。
// 记录接单、备注、要求补充信息、用户补充、上传、暂无资源等所有动作。
type BookRequestLog struct {
	gorm.Model

	RequestID uint  `gorm:"index;not null"`
	ActorID   int64 `gorm:"index;not null"`
	ActorName string

	Action string `gorm:"index;not null"`

	OldStatus string
	NewStatus string
	Note      string
}

func (BookRequestLog) TableName() string {
	return "book_request_logs"
}

package telegram

import (
	"sync"
	"time"
)

const (
	TelegramHTTPClientTimeout     = 90 * time.Second
	TelegramSendHTTPTimeout       = 12 * time.Second
	TelegramCallbackHTTPTimeout   = 4 * time.Second
	TelegramLongPollTimeout       = 30
	TelegramPollRetryDelay        = 3 * time.Second
	TelegramPollConflictError     = 409
	TelegramWebhookDropOnStartup  = true
	UserLockSlowWaitThreshold     = 2 * time.Second
	MessageQueueSlowWaitThreshold = 2 * time.Second
	MessageHandleSlowThreshold    = 5 * time.Second
	CallbackFastAckDelay          = 1200 * time.Millisecond
	CallbackHandleSlowThreshold   = 5 * time.Second
)

var userLocks sync.Map

type userLockEntry struct {
	mu sync.Mutex

	metaMu   sync.Mutex
	lastUsed time.Time
	inUse    int
}

// lockUser 获取用户级锁，并返回释放函数。
// 注意：必须 defer unlock()，不要直接操作 entry.mu。
func LockUser(userID int64) func() {
	now := time.Now()

	lockValue, _ := userLocks.LoadOrStore(userID, &userLockEntry{
		lastUsed: now,
	})

	entry := lockValue.(*userLockEntry)

	entry.metaMu.Lock()
	entry.inUse++
	entry.lastUsed = now
	entry.metaMu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()

		entry.metaMu.Lock()
		entry.inUse--
		if entry.inUse < 0 {
			entry.inUse = 0
		}
		entry.lastUsed = time.Now()
		entry.metaMu.Unlock()
	}
}

// tryLockUser 尝试获取用户级锁：锁空闲时立即返回释放函数，被占用时返回 false。
// 供轮询协程上的快速路径使用——绝不能在轮询协程上阻塞等待 worker 持有的锁，
// 否则一个慢处理会拖住整个 getUpdates 循环。
func TryLockUser(userID int64) (func(), bool) {
	now := time.Now()

	lockValue, _ := userLocks.LoadOrStore(userID, &userLockEntry{
		lastUsed: now,
	})

	entry := lockValue.(*userLockEntry)

	if !entry.mu.TryLock() {
		entry.metaMu.Lock()
		entry.lastUsed = now
		entry.metaMu.Unlock()
		return nil, false
	}

	entry.metaMu.Lock()
	entry.inUse++
	entry.lastUsed = now
	entry.metaMu.Unlock()

	return func() {
		entry.mu.Unlock()

		entry.metaMu.Lock()
		entry.inUse--
		if entry.inUse < 0 {
			entry.inUse = 0
		}
		entry.lastUsed = time.Now()
		entry.metaMu.Unlock()
	}, true
}

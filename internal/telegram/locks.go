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

// retainUserLock registers holders and waiters before the sweeper can remove an
// entry. A caller that loaded an entry just before its removal must retry.
func retainUserLock(userID int64) *userLockEntry {
	for {
		now := time.Now()
		value, _ := userLocks.LoadOrStore(userID, &userLockEntry{lastUsed: now})
		entry := value.(*userLockEntry)
		entry.metaMu.Lock()
		current, exists := userLocks.Load(userID)
		if !exists || current != entry {
			entry.metaMu.Unlock()
			continue
		}
		entry.inUse++
		entry.lastUsed = now
		entry.metaMu.Unlock()
		return entry
	}
}

func releaseUserLock(entry *userLockEntry) {
	entry.metaMu.Lock()
	entry.inUse--
	entry.lastUsed = time.Now()
	entry.metaMu.Unlock()
}

// LockUser serializes a user's messages and callbacks, including queued waiters.
func LockUser(userID int64) func() {
	entry := retainUserLock(userID)
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		releaseUserLock(entry)
	}
}

// TryLockUser never waits for another handler; the polling fast path can fall
// back to the worker queue without blocking other users' updates.
func TryLockUser(userID int64) (func(), bool) {
	entry := retainUserLock(userID)
	if !entry.mu.TryLock() {
		releaseUserLock(entry)
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		releaseUserLock(entry)
	}, true
}

func sweepIdleUserLocks(now time.Time) {
	userLocks.Range(func(key, value any) bool {
		entry := value.(*userLockEntry)
		entry.metaMu.Lock()
		if entry.inUse == 0 && now.Sub(entry.lastUsed) > 6*time.Hour {
			userLocks.CompareAndDelete(key, entry)
		}
		entry.metaMu.Unlock()
		return true
	})
}

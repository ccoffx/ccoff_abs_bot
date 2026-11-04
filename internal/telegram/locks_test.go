package telegram

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserLockRemainsExclusiveDuringCleanup(t *testing.T) {
	const userID int64 = 87123
	defer userLocks.Delete(userID)
	var active atomic.Int32
	var overlap atomic.Bool
	var workers sync.WaitGroup
	done := make(chan struct{})
	var sweeper sync.WaitGroup
	sweeper.Add(1)
	go func() {
		defer sweeper.Done()
		for {
			select {
			case <-done:
				return
			default:
				sweepIdleUserLocks(time.Now().Add(7 * time.Hour))
				runtime.Gosched()
			}
		}
	}()
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range 500 {
				var unlock func()
				if i%2 == 0 {
					var locked bool
					unlock, locked = TryLockUser(userID)
					if !locked {
						continue
					}
				} else {
					unlock = LockUser(userID)
				}
				if active.Add(1) != 1 {
					overlap.Store(true)
				}
				runtime.Gosched()
				active.Add(-1)
				unlock()
			}
		}()
	}
	workers.Wait()
	close(done)
	sweeper.Wait()
	if overlap.Load() {
		t.Fatal("cleanup assigned multiple locks to the same user")
	}
	sweepIdleUserLocks(time.Now().Add(7 * time.Hour))
	if _, exists := userLocks.Load(userID); exists {
		t.Fatal("unused locks must still be reclaimed")
	}
}

func TestSessionCleanupSkipsActiveHandler(t *testing.T) {
	const userID int64 = 87124
	defer ClearSession(userID)
	defer userLocks.Delete(userID)
	session := GetSession(userID)
	session.mu.Lock()
	session.updatedAt = time.Now().Add(-3 * time.Hour)
	session.mu.Unlock()
	unlock := LockUser(userID)
	sweepIdleSessions(time.Now())
	if current := GetSession(userID); current != session {
		t.Fatal("cleanup removed a session still held by a handler")
	}
	session.SetStep("WAITING_INPUT")
	unlock()
	sweepIdleSessions(time.Now())
	if current := GetSession(userID); current != session || current.GetStep() != "WAITING_INPUT" {
		t.Fatal("cleanup discarded the handler's refreshed state")
	}
	sweepIdleSessions(time.Now().Add(3 * time.Hour))
	if _, exists := UserSessions.Load(userID); exists {
		t.Fatal("idle session was not reclaimed")
	}
}

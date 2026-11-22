package game

import (
	"sync"
	"time"
)

type listeningRequestPriority int

const (
	listeningRequestFinal listeningRequestPriority = iota
	listeningRequestEntry
	listeningRequestLive
	listeningRequestNormal
)

type listeningRequestWaiter struct {
	group   string
	ready   chan struct{}
	granted bool
}

// The gate limits running requests and queued waiters separately. It grants
// closing snapshots first, then live activities, then ordinary synchronization.
// Activity groups rotate at each priority and may occupy at most three slots.
type listeningRequestGate struct {
	mu                      sync.Mutex
	limit, capacity, active int
	groups                  map[string]int
	queues                  [4][]*listeningRequestWaiter
}

func newListeningRequestGate(limit, capacity int) *listeningRequestGate {
	return &listeningRequestGate{limit: limit, capacity: capacity, groups: make(map[string]int)}
}

var listeningSyncSlots = newListeningRequestGate(listeningSyncWorkers, 512)

func (g *listeningRequestGate) acquire(priority listeningRequestPriority, group string, timeout time.Duration) bool {
	g.mu.Lock()
	queued := 0
	for _, queue := range g.queues {
		queued += len(queue)
	}
	if queued >= g.capacity {
		g.mu.Unlock()
		return false
	}
	w := &listeningRequestWaiter{group: group, ready: make(chan struct{})}
	g.queues[priority] = append(g.queues[priority], w)
	g.dispatchLocked()
	if w.granted {
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()
	timer := time.NewTimer(max(0, timeout))
	defer timer.Stop()
	select {
	case <-w.ready:
		return true
	case <-timer.C:
		g.mu.Lock()
		defer g.mu.Unlock()
		// A grant racing the timer owns a slot and must be released by caller.
		if w.granted {
			return true
		}
		for i, queued := range g.queues[priority] {
			if queued == w {
				g.queues[priority] = append(g.queues[priority][:i], g.queues[priority][i+1:]...)
				break
			}
		}
		return false
	}
}

func (g *listeningRequestGate) release(group string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if group != "" {
		g.groups[group]--
		if g.groups[group] == 0 {
			delete(g.groups, group)
		}
	}
	g.dispatchLocked()
}

func (g *listeningRequestGate) dispatchLocked() {
	for g.active < g.limit {
		priority, selected := -1, -1
		for p, queue := range g.queues {
			for i, w := range queue {
				if w.group != "" && g.groups[w.group] >= min(3, g.limit) {
					continue
				}
				priority, selected = p, i
				break
			}
			if selected != -1 {
				break
			}
		}
		if selected == -1 {
			return
		}
		w := g.queues[priority][selected]
		g.queues[priority] = append(g.queues[priority][:selected], g.queues[priority][selected+1:]...)
		g.active++
		if w.group != "" {
			g.groups[w.group]++
		}
		// Move this group's remaining waiters behind every other group. This
		// rotates all groups, rather than alternating only the first two.
		remaining := g.queues[priority]
		rotated := make([]*listeningRequestWaiter, 0, len(remaining))
		for _, queued := range remaining {
			if queued.group != w.group {
				rotated = append(rotated, queued)
			}
		}
		for _, queued := range remaining {
			if queued.group == w.group {
				rotated = append(rotated, queued)
			}
		}
		g.queues[priority] = rotated
		w.granted = true
		close(w.ready)
	}
}

func acquireListeningSyncSlot() bool {
	return listeningSyncSlots.acquire(listeningRequestNormal, "", listeningQueryWait)
}

func releaseListeningSyncSlot() { listeningSyncSlots.release("") }

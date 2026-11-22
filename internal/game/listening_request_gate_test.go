package game

import (
	"sync"
	"testing"
	"time"
)

func waitListeningQueue(t *testing.T, gate *listeningRequestGate, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		queued := 0
		for _, queue := range gate.queues {
			queued += len(queue)
		}
		gate.mu.Unlock()
		if queued == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request queue did not reach %d waiters", count)
}

type requestGrant struct {
	group   string
	release chan struct{}
}

func gateTestRequests(t *testing.T, gate *listeningRequestGate) (chan requestGrant, func(listeningRequestPriority, string)) {
	t.Helper()
	grants := make(chan requestGrant, 16)
	stop := make(chan struct{})
	var workers sync.WaitGroup
	t.Cleanup(func() { close(stop); workers.Wait() })
	start := func(priority listeningRequestPriority, group string) {
		workers.Go(func() {
			if !gate.acquire(priority, group, 2*time.Second) {
				return
			}
			defer gate.release(group)
			release := make(chan struct{})
			grants <- requestGrant{group, release}
			select {
			case <-release:
			case <-stop:
			}
		})
	}
	return grants, start
}

func nextGateGrant(t *testing.T, grants chan requestGrant, group string) {
	t.Helper()
	select {
	case grant := <-grants:
		if grant.group != group {
			t.Fatalf("granted %s before %s", grant.group, group)
		}
		close(grant.release)
	case <-time.After(2 * time.Second):
		t.Fatal("request was not granted")
	}
}

func TestListeningRequestsPrioritizeFinalThenLiveThenOrdinary(t *testing.T) {
	gate := newListeningRequestGate(1, 8)
	if !gate.acquire(listeningRequestNormal, "", 0) {
		t.Fatal("initial slot unavailable")
	}
	grants, start := gateTestRequests(t, gate)
	start(listeningRequestNormal, "ordinary")
	waitListeningQueue(t, gate, 1)
	start(listeningRequestLive, "live")
	waitListeningQueue(t, gate, 2)
	start(listeningRequestFinal, "final")
	waitListeningQueue(t, gate, 3)
	start(listeningRequestEntry, "entry")
	waitListeningQueue(t, gate, 4)
	gate.release("")
	for _, group := range []string{"final", "entry", "live", "ordinary"} {
		nextGateGrant(t, grants, group)
	}
}

func TestListeningRequestsRotateAllActivities(t *testing.T) {
	gate := newListeningRequestGate(1, 8)
	gate.acquire(listeningRequestNormal, "", 0)
	grants, start := gateTestRequests(t, gate)
	for i, group := range []string{"boss", "boss", "realm-a", "realm-a", "realm-b", "realm-b"} {
		start(listeningRequestFinal, group)
		waitListeningQueue(t, gate, i+1)
	}
	gate.release("")
	for _, group := range []string{"boss", "realm-a", "realm-b", "boss", "realm-a", "realm-b"} {
		nextGateGrant(t, grants, group)
	}
}

func TestListeningRequestsLimitEachActivityToThreeAndCancelWaiters(t *testing.T) {
	gate := newListeningRequestGate(4, 8)
	for range 3 {
		if !gate.acquire(listeningRequestLive, "boss", 0) {
			t.Fatal("activity slot unavailable")
		}
	}
	if gate.acquire(listeningRequestLive, "boss", time.Millisecond) {
		t.Fatal("one activity acquired a fourth slot")
	}
	if !gate.acquire(listeningRequestFinal, "realm", 0) {
		t.Fatal("another activity could not use spare capacity")
	}
	if gate.acquire(listeningRequestNormal, "", time.Millisecond) {
		t.Fatal("global concurrency exceeded four")
	}
	gate.release("realm")
	for range 3 {
		gate.release("boss")
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 || len(gate.groups) != 0 {
		t.Fatal("timed out request leaked capacity")
	}
	for _, queue := range gate.queues {
		if len(queue) != 0 {
			t.Fatal("timed out request leaked queue entry")
		}
	}
}

func TestListeningRequestQueueIsBounded(t *testing.T) {
	gate := newListeningRequestGate(1, 1)
	gate.acquire(listeningRequestNormal, "", 0)
	grants, start := gateTestRequests(t, gate)
	start(listeningRequestFinal, "boss")
	waitListeningQueue(t, gate, 1)
	if gate.acquire(listeningRequestLive, "realm", 0) {
		t.Fatal("full queue accepted another waiter")
	}
	gate.release("")
	nextGateGrant(t, grants, "boss")
}

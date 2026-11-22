package background

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestQueueBoundsDeduplicatesAndKeepsLatest(t *testing.T) {
	q := New(1, 1)
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var versions []int
	if q.Submit("panel", func() { close(started); <-release; mu.Lock(); versions = append(versions, 1); mu.Unlock() }, false) != Accepted {
		t.Fatal("initial job rejected")
	}
	<-started
	if q.Submit("other", func() {}, false) != Full {
		t.Fatal("capacity was not enforced")
	}
	for _, v := range []int{2, 3} {
		if q.Submit("panel", func() { mu.Lock(); versions = append(versions, v); mu.Unlock() }, true) != Pending {
			t.Fatal("duplicate was not coalesced")
		}
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 3 {
		t.Fatalf("latest state lost or stale states replayed: %v", versions)
	}
	if q.Submit("after", func() { panic("probe") }, false) != Accepted {
		t.Fatal("completed capacity leaked")
	}
	if err := q.WaitIdle(ctx); err != nil {
		t.Fatal("panic stranded queue", err)
	}
}
func TestSlowTaskDoesNotBlockAnotherKey(t *testing.T) {
	q := New(2, 2)
	started, release, other := make(chan struct{}), make(chan struct{}), make(chan struct{})
	q.Submit("slow", func() { close(started); <-release }, false)
	<-started
	q.Submit("other", func() { close(other) }, false)
	select {
	case <-other:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("another key waited for a slow task")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

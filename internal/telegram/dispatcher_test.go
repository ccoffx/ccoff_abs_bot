package telegram

import (
	"sync"
	"sync/atomic"
	"testing"
)

func testTelegramDispatcher() *telegramDispatcher {
	return &telegramDispatcher{
		high: make(chan TelegramAsyncJob, 1), normal: make(chan TelegramAsyncJob, 1),
		low: make(chan TelegramAsyncJob, 1), dedupe: make(map[string]struct{}),
	}
}

func TestAsyncDuplicateDoesNotTriggerSynchronousFallback(t *testing.T) {
	d := testTelegramDispatcher()
	var sends atomic.Int64
	job := TelegramAsyncJob{DedupeKey: "board:one", Send: func() error { sends.Add(1); return nil }}
	if !d.enqueue(job) {
		t.Fatal("initial job was rejected")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !d.enqueue(job) {
				// Callers fall back to sending synchronously when admission fails.
				_ = job.Send()
			}
		}()
	}
	wg.Wait()
	if sends.Load() != 0 || d.length() != 1 {
		t.Fatalf("deduplication caused fallback sends=%d queued=%d", sends.Load(), d.length())
	}
	queued := d.nextJob()
	d.runJob(1, queued)
	// An in-flight job must remain accepted even when its queue slot is free.
	if !d.enqueue(job) || d.length() != 0 {
		t.Fatal("an in-flight duplicate bypassed deduplication")
	}
	d.releaseDedupe(queued.DedupeKey)
	if sends.Load() != 1 || !d.enqueue(job) {
		t.Fatal("finished job did not release its deduplication key")
	}
}

func TestAsyncFullQueueReleasesRejectedKey(t *testing.T) {
	d := testTelegramDispatcher()
	job := TelegramAsyncJob{Priority: TelegramAsyncPriorityNormal, Send: func() error { return nil }}
	if !d.enqueue(job) {
		t.Fatal("initial job was rejected")
	}
	job.DedupeKey = "rejected"
	if d.enqueue(job) {
		t.Fatal("full queue accepted a new job")
	}
	<-d.normal
	if !d.enqueue(job) || d.length() != 1 {
		t.Fatal("full queue retained a key for a job that never entered it")
	}
}

func TestAsyncJobPanicIsContainedWithoutRetry(t *testing.T) {
	d := testTelegramDispatcher()
	attempts := 0
	d.runJob(1, TelegramAsyncJob{Kind: "partial-send", MaxAttempts: 3, Send: func() error {
		attempts++
		panic("failure after send")
	}})
	if attempts != 1 {
		t.Fatalf("panicking job was replayed %d times", attempts)
	}
	completed := false
	d.runJob(1, TelegramAsyncJob{MaxAttempts: 1, Send: func() error { completed = true; return nil }})
	if !completed {
		t.Fatal("worker could not process the next job after a panic")
	}
}

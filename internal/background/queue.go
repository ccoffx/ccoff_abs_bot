// Package background runs bounded, deduplicated, retryable presentation and
// synchronization work. Asset changes retain their durable business records.
package background

import (
	"context"
	"log"
	"sync"
)

type Result int

const (
	Accepted Result = iota
	Pending
	Full
)

type task struct {
	key string
	run func()
}
type Queue struct {
	mu       sync.Mutex
	jobs     chan *task
	tasks    map[string]*task
	idle     chan struct{}
	capacity int
}

func New(workers, capacity int) *Queue {
	q := &Queue{jobs: make(chan *task, capacity), tasks: make(map[string]*task), idle: make(chan struct{}), capacity: capacity}
	close(q.idle)
	for range workers {
		go q.worker()
	}
	return q
}

var Default = New(2, 64)

// Panels are independent of long ABS synchronization jobs.
var Panels = New(4, 128)

func Submit(key string, run func()) Result { return Default.Submit(key, run, false) }
func (q *Queue) Submit(key string, run func(), latest bool) Result {
	if key == "" || run == nil {
		return Full
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if existing := q.tasks[key]; existing != nil {
		if latest {
			existing.run = run
		}
		return Pending
	}
	if len(q.tasks) >= q.capacity {
		return Full
	}
	if len(q.tasks) == 0 {
		q.idle = make(chan struct{})
	}
	job := &task{key: key, run: run}
	q.tasks[key] = job
	q.jobs <- job // All running and queued keys together are bounded by capacity.
	return Accepted
}

func (q *Queue) worker() {
	for job := range q.jobs {
		q.mu.Lock()
		run := job.run
		job.run = nil
		q.mu.Unlock()
		func() {
			defer func() {
				if recover() != nil {
					log.Printf("后台任务异常，已释放队列: key=%s", job.key)
				}
			}()
			run()
		}()
		q.mu.Lock()
		if job.run != nil {
			q.jobs <- job
		} else {
			delete(q.tasks, job.key)
			if len(q.tasks) == 0 {
				close(q.idle)
			}
		}
		q.mu.Unlock()
	}
}

// WaitIdle is useful for graceful shutdown and deterministic integration tests.
func (q *Queue) WaitIdle(ctx context.Context) error {
	q.mu.Lock()
	idle := q.idle
	q.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

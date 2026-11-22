package game

import (
	"log"
	"sync"

	"ccoff-abs-bot/internal/textfmt"
)

const activityEventCapacity = 128

// Database status and participant retry timestamps are the durable queue.
// Admission is bounded and keyed across live refreshes, settlement, and ticks;
// skipped events remain discoverable in the next database scan after restart.
var activityEventTurns = struct {
	sync.Mutex
	active map[string]bool
}{active: make(map[string]bool)}

func tryActivityEventTurn(key string) (func(), bool) {
	activityEventTurns.Lock()
	defer activityEventTurns.Unlock()
	if activityEventTurns.active[key] || len(activityEventTurns.active) >= activityEventCapacity {
		return nil, false
	}
	activityEventTurns.active[key] = true
	return func() {
		activityEventTurns.Lock()
		delete(activityEventTurns.active, key)
		activityEventTurns.Unlock()
	}, true
}

// A slow realm must not make all later realms miss their closing window.
// Each admitted event still has at most three participant workers; all requests
// share the priority gate and its global four-slot limit.
func runActivityEventBatch(count int, work func(int)) {
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(activityEventCapacity, count) {
		workers.Go(func() {
			for i := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("活动调度异常: %s", textfmt.FormatPlainValue(r))
						}
					}()
					work(i)
				}()
			}
		})
	}
	for i := range count {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
}

func countActivityFailures(failures []error) int {
	count := 0
	for _, err := range failures {
		if err != nil {
			count++
		}
	}
	return count
}

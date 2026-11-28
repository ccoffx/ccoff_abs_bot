package game

import (
	"fmt"
	"log"
	"sync"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"ccoff-abs-bot/internal/titles"
)

// Bound ABS batch concurrency and leave one worker slot for interactive work.
func runListeningSyncBatch(count int, work func(int) error) []error {
	errors := make([]error, count)
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(listeningSyncWorkers-1, count) {
		workers.Go(func() {
			for i := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							errors[i] = fmt.Errorf("listening sync panic: %s", textfmt.FormatPlainValue(r))
						}
					}()
					errors[i] = work(i)
				}()
			}
		})
	}
	for i := range count {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return errors
}

// A batch owns the write lock through title publication. Other ABS refresh
// paths take a read lock, so individual refreshes can still run concurrently
// with each other, but cannot replace batch data before it is published.
var listeningRefreshMu sync.RWMutex

// The legacy force argument is retained for callers; a full ranking refresh now
// always fetches every account, so fresh individual caches cannot skew the batch.
func RefreshAllDailyListeningStatsWithOptions(_ bool) (int, int, int, error) {
	// The independent rolling scheduler owns book counts. Waiting for each
	// user's optional progress here would delay full-refresh callers again.
	return refreshAllDailyListeningStatsBatch()
}

type listeningBatchResult struct {
	success, total, skipped int
	err                     error
}
type listeningBatchCall struct {
	done   chan struct{}
	result listeningBatchResult
}

var listeningBatchFlights struct {
	sync.Mutex
	call *listeningBatchCall
}

func refreshAllDailyListeningStatsBatch() (int, int, int, error) {
	listeningBatchFlights.Lock()
	if call := listeningBatchFlights.call; call != nil {
		listeningBatchFlights.Unlock()
		<-call.done
		r := call.result
		return r.success, r.total, r.skipped, r.err
	}
	call := &listeningBatchCall{
		done:   make(chan struct{}),
		result: listeningBatchResult{err: fmt.Errorf("完整听书刷新异常中止")},
	}
	listeningBatchFlights.call = call
	listeningBatchFlights.Unlock()
	defer func() {
		listeningBatchFlights.Lock()
		listeningBatchFlights.call = nil
		close(call.done)
		listeningBatchFlights.Unlock()
	}()
	s, t, skipped, err := runAllDailyListeningStatsBatch()
	call.result = listeningBatchResult{s, t, skipped, err}
	return s, t, skipped, err
}

func runAllDailyListeningStatsBatch() (int, int, int, error) {
	// 手动全服刷新和后台刷新共用锁，避免并行批次交叉发布。
	listeningRefreshMu.Lock()
	defer listeningRefreshMu.Unlock()
	batchStartedAt := time.Now()
	var users []model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("TRIM(abs_user_id) <> ''").Find(&users).Error; err != nil {
		log.Printf("⚠️ 查询每日净修为刷新用户失败: err=%s", textfmt.FormatPlainError(err))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return 0, 0, 0, err
	}

	success := 0
	skipped := 0
	failed := 0
	// Fetch users concurrently while preserving the exclusive publication barrier.
	jobs := make(chan int)
	results := make([]bool, len(users))
	var workers sync.WaitGroup
	for range min(listeningSyncWorkers, len(users)) {
		workers.Go(func() {
			for index := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("全服听书任务异常: user=%d err=%s", users[index].TelegramID, textfmt.FormatPlainValue(r))
						}
					}()
					u := users[index]
					if _, ok := refreshDailyListeningStatsFromABSUnlocked(u.TelegramID, u.AbsUserID, true); ok {
						_, results[index] = syncCultivationFromDailyListeningStats(u.TelegramID)
					}
				}()
			}
		})
	}
	for index := range users {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	for _, ok := range results {
		if ok {
			success++
		} else {
			failed++
		}
	}

	if failed > 0 {
		err := fmt.Errorf("每日净修为刷新失败: %d/%d 个用户", failed, len(users))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return success, len(users), skipped, err
	}
	if err := titles.RefreshRankTitlesAfterListeningBatch(batchStartedAt, time.Now()); err != nil {
		publishErr := fmt.Errorf("完整听书刷新后的称号发布失败: %w", err)
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, publishErr)
		return success, len(users), skipped, publishErr
	}
	return success, len(users), skipped, nil
}

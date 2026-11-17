package game

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

const (
	ListeningSyncInterval = 5 * time.Minute
	ListeningMaxAge       = 10 * time.Minute
	listeningSyncWorkers  = 4
	listeningQueryWait    = 20 * time.Second
)

// Bound remote synchronization across foreground, scheduled and full-batch work.
var listeningSyncSlots = make(chan struct{}, listeningSyncWorkers)

var ErrListeningNotFresh = errors.New("听书修为尚未成功同步")

func requireFreshListening(userID int64) error {
	if freshness := EnsureListeningFresh(userID); !freshness.Fresh {
		return fmt.Errorf("%w: %s", ErrListeningNotFresh, freshness.Notice)
	}
	return nil
}

type listeningFetchResult struct {
	days map[string]float64
	ok   bool
}
type listeningFetchCall struct {
	done    chan struct{}
	result  listeningFetchResult
	waiters int
}

var listeningFetches = struct {
	sync.Mutex
	calls map[string]*listeningFetchCall
}{calls: make(map[string]*listeningFetchCall)}

// Callers acquire the batch barrier BEFORE joining a flight. This ordering
// prevents a full batch from waiting on a flight blocked by its own write lock.
func coalesceListeningFetch(key string, work func() listeningFetchResult) listeningFetchResult {
	listeningFetches.Lock()
	if call := listeningFetches.calls[key]; call != nil {
		call.waiters++
		listeningFetches.Unlock()
		<-call.done
		return call.result
	}
	call := &listeningFetchCall{done: make(chan struct{})}
	listeningFetches.calls[key] = call
	listeningFetches.Unlock()
	defer func() {
		listeningFetches.Lock()
		delete(listeningFetches.calls, key)
		close(call.done)
		listeningFetches.Unlock()
	}()
	call.result = work()
	return call.result
}

func acquireListeningSyncSlot() bool {
	timer := time.NewTimer(listeningQueryWait)
	defer timer.Stop()
	select {
	case listeningSyncSlots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func listeningTimestampFresh(at, now time.Time, maxAge time.Duration) bool {
	return !at.IsZero() && !now.Before(at) && now.Sub(at) < maxAge
}

func listeningDailyTimestampFresh(at, now time.Time, maxAge time.Duration) bool {
	return listeningTimestampFresh(at, now, maxAge) && SectDayKey(at) == SectDayKey(now)
}

type ListeningFreshness struct {
	Fresh     bool
	UpdatedAt time.Time
	Notice    string
}

// Today's applied row alone cannot prove that the profile includes a late
// historical correction. Include tombstones, whose previous credit must be
// removed by settlement. Keep this check shared by queries and the scheduler.
func listeningHistorySettledSQL(userExpression string) string {
	return `NOT EXISTS (SELECT 1 FROM daily_listening_stats h WHERE h.user_id = ` + userExpression + ` AND (
		h.cultivation_applied = false OR
		h.applied_effective_hours <> h.effective_hours OR h.applied_capped_seconds <> h.capped_seconds OR
		(h.deleted_at IS NULL AND h.day_key <= ? AND h.applied_eligible = false) OR
		(h.applied_eligible = true AND (h.applied_base_hours <> h.base_cultivation_hours OR
		 h.applied_total_hours <> h.base_cultivation_hours + h.spirit_fire_hours + h.sect_owner_bonus_hours)) OR
		((h.deleted_at IS NOT NULL OR h.day_key > ?) AND (h.applied_total_hours <> 0 OR h.applied_base_hours <> 0))
	))`
}

func listeningFreshness(userID int64, absID string, now time.Time) ListeningFreshness {
	var snapshot struct {
		model.DailyListeningStat
		ProfileInitialized bool
	}
	if storage.DB.Model(&model.DailyListeningStat{}).
		Select("daily_listening_stats.*, (EXISTS (SELECT 1 FROM cultivations c WHERE c.user_id = ? AND c.listening_initialized = ?) AND "+listeningHistorySettledSQL("?")+") AS profile_initialized", userID, true, userID, SectDayKey(now), SectDayKey(now)).
		Where("user_id = ? AND abs_user_id = ? AND day_key = ?", userID, absID, SectDayKey(now)).First(&snapshot).Error != nil {
		return ListeningFreshness{}
	}
	stat := snapshot.DailyListeningStat
	return ListeningFreshness{Fresh: snapshot.ProfileInitialized && listeningDailyTimestampFresh(stat.LastFetchedAt, now, ListeningMaxAge) && DailyListeningCultivationSettled(stat), UpdatedAt: stat.LastFetchedAt}
}

func staleListeningNotice(at time.Time) string {
	if at.IsZero() {
		return "⚠️ 听书同步未完成，暂无成功同步时间；修为数据待同步。"
	}
	return fmt.Sprintf("⚠️ 听书同步未完成，展示旧数据；最近成功获取：%s（北京时间）。", at.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"))
}

// EnsureListeningFresh keeps the common path local. Network requests happen
// only for missing, expired or unapplied data; failures never claim freshness.
func EnsureListeningFresh(userID int64) ListeningFreshness {
	if storage.DB == nil {
		return ListeningFreshness{Notice: "⚠️ 听书数据暂时读取失败。"}
	}
	var user model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", userID).First(&user).Error; err != nil {
		return ListeningFreshness{Notice: "⚠️ 听书账号暂时读取失败。"}
	}
	if strings.TrimSpace(user.AbsUserID) == "" {
		return ListeningFreshness{Fresh: true} // A local wallet has no ABS source.
	}
	state := listeningFreshness(userID, user.AbsUserID, time.Now())
	if state.Fresh {
		return state
	}
	// Bound the foreground wait behind a full ranking batch.
	deadline := time.Now().Add(listeningQueryWait)
	for !listeningRefreshMu.TryRLock() {
		if time.Now().After(deadline) {
			state = listeningFreshness(userID, user.AbsUserID, time.Now())
			if !state.Fresh {
				state.Notice = staleListeningNotice(state.UpdatedAt)
			}
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	func() {
		defer listeningRefreshMu.RUnlock()
		state = listeningFreshness(userID, user.AbsUserID, time.Now())
		if state.Fresh {
			return
		}
		if _, ok := refreshDailyListeningStatsFromABSUnlocked(userID, user.AbsUserID, false); ok {
			syncCultivationFromDailyListeningStats(userID)
		}
	}()
	state = listeningFreshness(userID, user.AbsUserID, time.Now())
	if !state.Fresh {
		state.Notice = staleListeningNotice(state.UpdatedAt)
	}
	return state
}

type listeningSyncUser struct {
	TelegramID        int64
	AbsUserID         string
	LastFetchedAt     time.Time
	ProgressFetchedAt time.Time
	Settled           bool
}

func listeningSyncUsers(now time.Time) ([]listeningSyncUser, error) {
	var users []listeningSyncUser
	err := storage.DB.Model(&model.User{}).
		Select("users.telegram_id, users.abs_user_id, d.last_fetched_at, p.progress_fetched_at, (c.id IS NOT NULL AND c.listening_initialized AND d.cultivation_applied AND d.applied_eligible AND d.applied_effective_hours = d.effective_hours AND d.applied_capped_seconds = d.capped_seconds AND d.applied_base_hours = d.base_cultivation_hours AND d.applied_total_hours = d.base_cultivation_hours + d.spirit_fire_hours + d.sect_owner_bonus_hours AND "+listeningHistorySettledSQL("users.telegram_id")+") AS settled", SectDayKey(now), SectDayKey(now)).
		Joins("LEFT JOIN daily_listening_stats d ON d.user_id = users.telegram_id AND d.abs_user_id = users.abs_user_id AND d.day_key = ? AND d.deleted_at IS NULL", SectDayKey(now)).
		Joins("LEFT JOIN cultivations c ON c.user_id = users.telegram_id").
		Joins("LEFT JOIN listening_report_snapshots p ON p.user_id = users.telegram_id AND p.abs_user_id = users.abs_user_id").
		Where("TRIM(users.abs_user_id) <> ''").Find(&users).Error
	// Compare time.Time instants rather than SQLite timestamp text offsets.
	sort.SliceStable(users, func(i, j int) bool { return users[i].LastFetchedAt.Before(users[j].LastFetchedAt) })
	return users, err
}

var dueListeningSyncMu sync.Mutex

// RefreshDueListeningUsers is separate from full title batches. A rolling
// refresh never publishes titles from a mixture of individual snapshots.
func RefreshDueListeningUsers(now time.Time) error {
	if storage.DB == nil || !dueListeningSyncMu.TryLock() {
		return nil
	}
	defer dueListeningSyncMu.Unlock()
	users, err := listeningSyncUsers(now)
	if err != nil {
		return err
	}
	jobs := make(chan listeningSyncUser)
	var workers sync.WaitGroup
	for range listeningSyncWorkers - 1 { // Leave capacity for interactive work.
		workers.Go(func() {
			for user := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("听书同步任务异常: user=%d err=%v", user.TelegramID, r)
						}
					}()
					if !user.Settled || !listeningDailyTimestampFresh(user.LastFetchedAt, time.Now(), ListeningSyncInterval) {
						// Recheck after queueing, as a foreground refresh may have won.
						func() {
							listeningRefreshMu.RLock()
							defer listeningRefreshMu.RUnlock()
							state := listeningFreshness(user.TelegramID, user.AbsUserID, time.Now())
							if !listeningDailyTimestampFresh(state.UpdatedAt, time.Now(), ListeningSyncInterval) || !state.Fresh {
								if _, ok := refreshDailyListeningStatsFromABSUnlocked(user.TelegramID, user.AbsUserID, !listeningDailyTimestampFresh(state.UpdatedAt, time.Now(), ListeningSyncInterval)); ok {
									syncCultivationFromDailyListeningStats(user.TelegramID)
								}
							}
						}()
					}
					refreshListeningReportProgress(user.TelegramID, user.AbsUserID, time.Now())
				}()
			}
		})
	}
	for _, user := range users {
		currentNow := time.Now()
		if !user.Settled || !listeningDailyTimestampFresh(user.LastFetchedAt, currentNow, ListeningSyncInterval) || !listeningTimestampFresh(user.ProgressFetchedAt, currentNow, ListeningSyncInterval) {
			jobs <- user
		}
	}
	close(jobs)
	workers.Wait()
	// Make a missed freshness target observable; do not record a failed pass as
	// successful merely because the worker queue drained.
	remaining, err := listeningSyncUsers(time.Now())
	if err != nil {
		return err
	}
	staleStats, staleProgress := 0, 0
	for _, user := range remaining {
		if !user.Settled || !listeningDailyTimestampFresh(user.LastFetchedAt, time.Now(), ListeningMaxAge) {
			staleStats++
		}
		if !listeningTimestampFresh(user.ProgressFetchedAt, time.Now(), ListeningMaxAge) {
			staleProgress++
		}
	}
	if staleStats+staleProgress > 0 {
		return fmt.Errorf("听书同步未达到10分钟新鲜度：修为 %d 人，书籍进度 %d 人", staleStats, staleProgress)
	}
	return nil
}

// Every bound account can change the top ten. Checking only the displayed
// rows (or just the caller) would miss a stale user who should enter the board.
func ensureListeningRankFresh() string {
	users, err := listeningSyncUsers(time.Now())
	if err != nil {
		return "⚠️ 榜单新鲜度检查失败，以下为已保存数据。"
	}
	stale := false
	for _, user := range users {
		if !user.Settled || !listeningDailyTimestampFresh(user.LastFetchedAt, time.Now(), ListeningMaxAge) {
			stale = true
			break
		}
	}
	if !stale {
		return ""
	}
	_, _, _, batchErr := refreshAllDailyListeningStatsBatch()
	users, err = listeningSyncUsers(time.Now())
	if err == nil {
		for _, user := range users {
			if !user.Settled || !listeningDailyTimestampFresh(user.LastFetchedAt, time.Now(), ListeningMaxAge) {
				return "⚠️ 榜单未能全部更新，排名可能滞后。\n" + staleListeningNotice(user.LastFetchedAt)
			}
		}
	}
	if err != nil || batchErr != nil {
		return "⚠️ 全服批次未全部完成，榜单使用已保存数据，称号未重新发布。"
	}
	return ""
}

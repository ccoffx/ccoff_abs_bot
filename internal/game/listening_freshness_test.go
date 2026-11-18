package game

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

func setupListeningFreshnessTest(t *testing.T) {
	t.Helper()
	setupRetreatBackfillDB(t)
	contributionMust(t, storage.DB.Exec("PRAGMA journal_mode=WAL").Error)
	pool, err := storage.DB.DB()
	contributionMust(t, err)
	pool.SetMaxOpenConns(4)
	contributionMust(t, storage.DB.Exec("CREATE UNIQUE INDEX ux_freshness_profile ON cultivations(user_id)").Error)
}

func seedListeningFreshnessUser(t *testing.T, id int64, age time.Duration, hours float64) {
	t.Helper()
	absID := fmt.Sprint("abs", id)
	if id != 1 {
		contributionMust(t, storage.DB.Create(&model.User{TelegramID: id, Username: fmt.Sprint("fresh", id), AbsUserID: absID}).Error)
	}
	at := time.Now().Add(-age)
	contributionMust(t, storage.DB.Create(&model.Cultivation{UserID: id, ListeningInitialized: true, ListeningBaseHours: hours, TotalAudioTime: hours}).Error)
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{
		UserID: id, AbsUserID: absID, DayKey: SectDayKey(time.Now()), LastFetchedAt: at,
		RawSeconds: hours * 3600, CappedSeconds: hours * 3600, EffectiveHours: hours, BaseCultivationHours: hours,
		CultivationApplied: true, AppliedEligible: true, AppliedEffectiveHours: hours,
		AppliedCappedSeconds: hours * 3600, AppliedBaseHours: hours, AppliedTotalHours: hours,
	}).Error)
	contributionMust(t, storage.DB.Create(&model.ListeningReportSnapshot{UserID: id, AbsUserID: absID, RawTotalSeconds: hours * 3600, StatsFetchedAt: at, ProgressFetchedAt: at}).Error)
}

func TestListeningFreshnessCutoffAndMidnight(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age   time.Duration
		fresh bool
	}{
		{0, true}, {7 * time.Minute, true}, {10*time.Minute - time.Nanosecond, true}, {10 * time.Minute, false}, {11 * time.Minute, false}, {-time.Second, false},
	} {
		if got := listeningTimestampFresh(now.Add(-tc.age), now, ListeningMaxAge); got != tc.fresh {
			t.Fatalf("age=%s fresh=%t", tc.age, got)
		}
	}
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 0, .5)
	var stat model.DailyListeningStat
	contributionMust(t, storage.DB.First(&stat).Error)
	contributionMust(t, storage.DB.Model(&stat).UpdateColumn("day_key", SectDayKey(now.AddDate(0, 0, -1))).Error)
	if state := listeningFreshness(1, "abs1", now); state.Fresh {
		t.Fatal("yesterday's fresh timestamp was accepted as today's snapshot")
	}
}

func TestListeningFreshnessRejectsUnsettledHistory(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 0, .5)
	now := time.Now()
	old := model.DailyListeningStat{
		UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now.AddDate(0, 0, -1)),
		EffectiveHours: 2, CappedSeconds: 7200,
	}
	contributionMust(t, storage.DB.Create(&old).Error)
	// Today's zero/unchanged input may already be settled when a historical
	// correction commits and the following cultivation transaction fails.
	if state := listeningFreshness(1, "abs1", now); state.Fresh {
		t.Error("fresh today hid an unsettled historical correction")
	}
	users, err := listeningSyncUsers(now)
	contributionMust(t, err)
	if len(users) != 1 || users[0].Settled {
		t.Error("rolling scheduler and ranking accepted an unsettled history")
	}
	if _, ok := syncCultivationFromDailyListeningStatsAt(1, now); !ok {
		t.Fatal("settlement failed")
	}
	if state := listeningFreshness(1, "abs1", now); !state.Fresh {
		t.Fatal("settled history did not restore freshness")
	}
	contributionMust(t, storage.DB.Model(&old).UpdateColumn("effective_hours", 1).Error)
	if state := listeningFreshness(1, "abs1", now); state.Fresh {
		t.Error("changed previously settled history was considered fresh")
	}
	if _, ok := syncCultivationFromDailyListeningStatsAt(1, now); !ok {
		t.Fatal("historical correction settlement failed")
	}
	contributionMust(t, storage.DB.Delete(&old).Error)
	if state := listeningFreshness(1, "abs1", now); state.Fresh {
		t.Error("soft-deleted credited history was considered settled")
	}
	users, err = listeningSyncUsers(now)
	contributionMust(t, err)
	if len(users) != 1 || users[0].Settled {
		t.Error("scheduler accepted soft-deleted credited history")
	}
}

func TestPreviousDayFetchCannotMakeTodayFresh(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, time.Minute, .5)
	now := time.Date(2026, 11, 18, 0, 1, 0, 0, storage.DailyOperationsLocation)
	fetched := now.Add(-2 * time.Minute)
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").Updates(map[string]any{
		"day_key": SectDayKey(now), "last_fetched_at": fetched,
	}).Error)
	if state := listeningFreshness(1, "abs1", now); state.Fresh {
		t.Fatalf("yesterday's request marked today's snapshot fresh: %+v", state)
	}
	users, err := listeningSyncUsers(now)
	contributionMust(t, err)
	if len(users) != 1 || listeningDailyTimestampFresh(users[0].LastFetchedAt, now, ListeningMaxAge) {
		t.Fatalf("queue/rank accepted yesterday's request: %+v", users)
	}
	if _, ok := cachedDailyListeningDaysWithinTTL(1, "abs1", now); ok {
		t.Fatal("daily cache reused yesterday's request")
	}
}

func TestSectTaskFallbackRequiresFreshCurrentBinding(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, time.Minute, .5)
	now := time.Now()
	if hours, ok := freshTodaySectListeningHoursFromCache(1, now); !ok || hours != .5 {
		t.Fatalf("fresh current binding was rejected: hours=%v ok=%t", hours, ok)
	}
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").UpdateColumn("last_fetched_at", now.Add(-11*time.Minute)).Error)
	if _, ok := freshTodaySectListeningHoursFromCache(1, now); ok {
		t.Fatal("stale listening cache was accepted for a sect task")
	}
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").Updates(map[string]any{
		"last_fetched_at": now, "abs_user_id": "old-binding",
	}).Error)
	if _, ok := freshTodaySectListeningHoursFromCache(1, now); ok {
		t.Fatal("old binding's listening cache was accepted for a sect task")
	}
}

func TestListeningQueueSkipsWhitespaceOnlyBinding(t *testing.T) {
	setupListeningFreshnessTest(t)
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").UpdateColumn("abs_user_id", "  ").Error)
	users, err := listeningSyncUsers(time.Now())
	contributionMust(t, err)
	if len(users) != 0 {
		t.Fatalf("whitespace-only ABS binding entered the sync queue: %+v", users)
	}
}

func TestSecretRealmFallbackRejectsFutureSnapshot(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, -time.Hour, .5)
	if _, ok := freshSectSecretRealmSnapshotFromLocalDailyStats("abs1"); ok {
		t.Fatal("future timestamp made the secret realm fallback look fresh")
	}
}

func TestSecretRealmFallbackFindsValidSnapshotBehindFutureRow(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, -time.Hour, .5)
	now := time.Now()
	yesterday := SectDayKey(now.In(storage.DailyOperationsLocation).AddDate(0, 0, -1))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{
		UserID: 1, AbsUserID: "abs1", DayKey: yesterday,
		LastFetchedAt: now.Add(-time.Minute), RawSeconds: 3600, EffectiveHours: 1,
	}).Error)
	if _, ok := freshSectSecretRealmSnapshotFromLocalDailyStats("abs1"); !ok {
		t.Fatal("future historical row hid a valid recent snapshot")
	}
}

func TestFutureListeningTimestampsCanRecover(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, -time.Hour, .5)
	day := SectDayKey(time.Now())
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true}]}`)
	})
	refreshListeningReportProgress(1, "abs1", time.Now())
	var snapshot model.ListeningReportSnapshot
	contributionMust(t, storage.DB.Where("user_id=1").First(&snapshot).Error)
	if snapshot.FinishedBooks != 1 || snapshot.ProgressFetchedAt.After(time.Now()) {
		t.Fatalf("future book progress timestamp blocked recovery: %+v", snapshot)
	}
	contributionMust(t, saveListeningReportStats(1, "abs1", 7200, time.Now()))
	contributionMust(t, storage.DB.Where("user_id=1").First(&snapshot).Error)
	if snapshot.RawTotalSeconds != 7200 || snapshot.StatsFetchedAt.After(time.Now()) {
		t.Fatalf("future report timestamp blocked recovery: %+v", snapshot)
	}
	contributionMust(t, recordPreparedDailyListeningStats(1, "abs1", map[string]float64{day: 3600}, nil, time.Now()))
	var stat model.DailyListeningStat
	contributionMust(t, storage.DB.Where("user_id=1 AND day_key=?", day).First(&stat).Error)
	if stat.RawSeconds != 3600 || stat.LastFetchedAt.After(time.Now()) {
		t.Fatalf("future daily timestamp blocked recovery: %+v", stat)
	}
}

func TestBookProgressCanStartBeforeStatsSnapshotExists(t *testing.T) {
	setupListeningFreshnessTest(t)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true},{"isFinished":false}]}`)
	})
	refreshListeningReportProgress(1, "abs1", time.Now())
	var snapshot model.ListeningReportSnapshot
	contributionMust(t, storage.DB.Where("user_id=1").First(&snapshot).Error)
	if requests.Load() != 1 || snapshot.FinishedBooks != 1 || snapshot.ListeningBooks != 1 || snapshot.ProgressFetchedAt.IsZero() {
		t.Fatalf("book progress remained blocked by missing stats: %+v requests=%d", snapshot, requests.Load())
	}
	contributionMust(t, saveListeningReportStats(1, "abs1", 3600, time.Now()))
	contributionMust(t, storage.DB.Where("user_id=1").First(&snapshot).Error)
	if snapshot.RawTotalSeconds != 3600 || snapshot.FinishedBooks != 1 || snapshot.ListeningBooks != 1 {
		t.Fatalf("later stats refresh erased earlier book progress: %+v", snapshot)
	}
	if report := GetPersonalReport(nil, "abs1"); !strings.Contains(report, "已经听完书籍: 1 本") || !strings.Contains(report, "今日净修为: 待同步") {
		t.Fatalf("report lost independent book progress: %s", report)
	}
}

func TestBookProgressRejectsOldBindingBeforeFirstSnapshot(t *testing.T) {
	setupListeningFreshnessTest(t)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := storage.DB.Model(&model.User{}).Where("telegram_id=1").UpdateColumn("abs_user_id", "replacement").Error; err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true}]}`)
	})
	refreshListeningReportProgress(1, "abs1", time.Now())
	var count int64
	contributionMust(t, storage.DB.Model(&model.ListeningReportSnapshot{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("late old-binding book progress created a snapshot")
	}
}

func TestExpiredListeningQueryRefreshesOrLabelsOldData(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", failure), func(t *testing.T) {
			setupListeningFreshnessTest(t)
			seedListeningFreshnessUser(t, 1, 11*time.Minute, .5)
			var before model.DailyListeningStat
			contributionMust(t, storage.DB.First(&before).Error)
			var stats, sessions atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					stats.Add(1)
					if failure {
						w.WriteHeader(503)
						return
					}
					fmt.Fprintf(w, `{"days":{%q:3600}}`, SectDayKey(time.Now()))
				} else {
					sessions.Add(1)
					fmt.Fprint(w, `{"sessions":[]}`)
				}
			})
			state := EnsureListeningFresh(1)
			var stat model.DailyListeningStat
			var cul model.Cultivation
			contributionMust(t, storage.DB.First(&stat).Error)
			contributionMust(t, storage.DB.First(&cul).Error)
			if failure {
				if state.Fresh || !strings.Contains(state.Notice, "展示旧数据") || !strings.Contains(state.Notice, before.LastFetchedAt.In(storage.DailyOperationsLocation).Format("15:04:05")) || !stat.LastFetchedAt.Equal(before.LastFetchedAt) || cul.TotalAudioTime != .5 {
					t.Fatalf("failed refresh changed data or hid its age: state=%+v stat=%+v cul=%+v", state, stat, cul)
				}
			} else {
				if !state.Fresh || state.Notice != "" || cul.TotalAudioTime != 1 || !DailyListeningCultivationSettled(stat) || sessions.Load() != 0 {
					t.Fatalf("refresh not settled: state=%+v stat=%+v cul=%+v", state, stat, cul)
				}
				EnsureListeningFresh(1)
			}
			if stats.Load() != 1 {
				t.Fatalf("stats requests=%d", stats.Load())
			}
		})
	}
}

func TestFreshPendingListeningSettlesWithoutNetwork(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, time.Minute, .5)
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").UpdateColumn("effective_hours", .4).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	if state := EnsureListeningFresh(1); !state.Fresh {
		t.Fatal(state)
	}
	var cul model.Cultivation
	contributionMust(t, storage.DB.First(&cul).Error)
	if requests.Load() != 0 || cul.TotalAudioTime != .4 {
		t.Fatalf("requests=%d total=%v", requests.Load(), cul.TotalAudioTime)
	}
}

func TestFreshListeningRebuildsUninitializedProfileWithoutNetwork(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, time.Minute, .5)
	contributionMust(t, storage.DB.Model(&model.Cultivation{}).Where("user_id=1").UpdateColumns(map[string]any{
		"listening_initialized": false, "listening_base_hours": 0, "total_audio_time": 0,
	}).Error)
	if state := listeningFreshness(1, "abs1", time.Now()); state.Fresh || state.UpdatedAt.IsZero() {
		t.Fatalf("uninitialized profile accepted or source timestamp lost: %+v", state)
	}
	users, err := listeningSyncUsers(time.Now())
	contributionMust(t, err)
	if len(users) != 1 || users[0].Settled {
		t.Fatalf("uninitialized profile accepted by queue/rank check: %+v", users)
	}
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	if state := EnsureListeningFresh(1); !state.Fresh {
		t.Fatal(state)
	}
	var cul model.Cultivation
	contributionMust(t, storage.DB.First(&cul).Error)
	if requests.Load() != 0 || !cul.ListeningInitialized || cul.TotalAudioTime != .5 {
		t.Fatalf("requests=%d rebuilt profile=%+v", requests.Load(), cul)
	}
}

func TestConcurrentListeningRefreshSharesOneABSResponse(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 11*time.Minute, .5)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			if requests.Add(1) == 1 {
				close(started)
			}
			<-release
			fmt.Fprintf(w, `{"days":{%q:3600}}`, SectDayKey(time.Now()))
		} else {
			fmt.Fprint(w, `{"sessions":[]}`)
		}
	})
	const callers = 8
	done := make(chan bool, callers)
	go func() { _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); done <- ok }()
	<-started
	for range callers - 1 {
		go func() { _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); done <- ok }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		listeningFetches.Lock()
		call := listeningFetches.calls[fmt.Sprintf("stats:%d:%q:%q", 1, "abs1", []string(nil))]
		joined := call != nil && call.waiters == callers-1
		listeningFetches.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("callers did not join the in-flight request")
		}
		time.Sleep(time.Millisecond)
	}
	releaseOnce.Do(func() { close(release) })
	for range callers {
		if !<-done {
			t.Fatal("shared refresh failed")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("duplicate ABS stats requests: %d", requests.Load())
	}
}

func TestDueListeningQueueRefreshesOnlyExpiredUsersAndProgress(t *testing.T) {
	setupListeningFreshnessTest(t)
	for id := int64(1); id <= 9; id++ {
		seedListeningFreshnessUser(t, id, 6*time.Minute, .5)
	}
	seedListeningFreshnessUser(t, 10, time.Minute, .5)
	seedListeningFreshnessUser(t, 11, time.Minute, .5)
	contributionMust(t, storage.DB.Model(&model.ListeningReportSnapshot{}).Where("user_id=11").UpdateColumn("progress_fetched_at", time.Now().Add(-6*time.Minute)).Error)
	var current, maximum, stats, progress, freshStats atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := current.Add(1)
		defer current.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			stats.Add(1)
			if strings.Contains(r.URL.Path, "/abs10/") || strings.Contains(r.URL.Path, "/abs11/") {
				freshStats.Add(1)
			}
			fmt.Fprintf(w, `{"days":{%q:3600}}`, SectDayKey(time.Now()))
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			fmt.Fprint(w, `{"sessions":[]}`)
		default:
			progress.Add(1)
			fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true},{"isFinished":false}]}`)
		}
	})
	contributionMust(t, RefreshDueListeningUsers(time.Now()))
	if stats.Load() != 9 || freshStats.Load() != 0 || progress.Load() != 10 || maximum.Load() > listeningSyncWorkers-1 || maximum.Load() < 2 {
		t.Fatalf("stats=%d fresh=%d progress=%d concurrency=%d", stats.Load(), freshStats.Load(), progress.Load(), maximum.Load())
	}
	contributionMust(t, RefreshDueListeningUsers(time.Now()))
	if stats.Load() != 9 || progress.Load() != 10 {
		t.Fatal("fresh users were resynchronized")
	}
}

func TestExpiredBookProgressIsRefreshedAndFailureIsLabelled(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, time.Minute, .5)
	contributionMust(t, storage.DB.Model(&model.ListeningReportSnapshot{}).Where("user_id=1").Updates(map[string]any{"finished_books": 3, "progress_fetched_at": time.Now().Add(-11 * time.Minute)}).Error)
	var fail atomic.Bool
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/listening-stats") || strings.HasSuffix(r.URL.Path, "/listening-sessions") {
			t.Error("fresh cultivation contacted stats API")
		}
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true}]}`)
	})
	if report := GetPersonalReport(nil, "abs1"); !strings.Contains(report, "已经听完书籍: 1 本") {
		t.Fatal(report)
	}
	fail.Store(true)
	contributionMust(t, storage.DB.Model(&model.ListeningReportSnapshot{}).Where("user_id=1").UpdateColumn("progress_fetched_at", time.Now().Add(-11*time.Minute)).Error)
	if report := GetPersonalReport(nil, "abs1"); !strings.Contains(report, "已经听完书籍: 1 本") || !strings.Contains(report, "更新失败，展示旧数据") {
		t.Fatal(report)
	}
	if requests.Load() != 2 {
		t.Fatal(requests.Load())
	}
}

func TestBreakthroughDoesNotSpendUsingExpiredListeningData(t *testing.T) {
	setupGuardTestDB(t)
	before := guardTestBalance(t, 1)
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").UpdateColumn("last_fetched_at", time.Now().Add(-11*time.Minute)).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	if _, err := ResolveBreakthrough(1, "USE_INVENTORY", 0, time.Now()); !errors.Is(err, ErrListeningNotFresh) {
		t.Fatalf("stale breakthrough accepted: %v", err)
	}
	if _, err := CreateBreakthroughRitual(1, "user", 1, -100, "USE_INVENTORY", time.Now()); !errors.Is(err, ErrListeningNotFresh) {
		t.Fatalf("stale recruitment accepted: %v", err)
	}
	if guardTestBalance(t, 1) != before {
		t.Fatal("failed synchronization spent points")
	}
	var count int64
	contributionMust(t, storage.DB.Model(&model.BreakthroughAttempt{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("stale synchronization created an attempt")
	}
}

func TestInvalidBreakthroughRitualCanCancelDuringABSOutage(t *testing.T) {
	for _, invalid := range []string{"deleted_account", "missing_profile", "suspended", "expired"} {
		t.Run(invalid, func(t *testing.T) {
			setupGuardTestDB(t)
			ritual := guardTestRitual(t)
			contributionMust(t, guardTestJoin(ritual.ID, 2))
			contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").UpdateColumn("last_fetched_at", time.Now().Add(-11*time.Minute)).Error)
			switch invalid {
			case "deleted_account":
				contributionMust(t, storage.DB.Where("telegram_id=1").Delete(&model.User{}).Error)
			case "missing_profile":
				contributionMust(t, storage.DB.Where("user_id=1").Delete(&model.Cultivation{}).Error)
			case "suspended":
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").UpdateColumn("admin_suspended", true).Error)
			case "expired":
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").UpdateColumn("expire_at", time.Now().Add(-time.Hour)).Error)
			}
			var requests atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
			_, err := ResolveBreakthrough(1, ritual.Mode, ritual.ID, time.Now().Add(2*time.Minute))
			if !isBreakthroughCancellationError(err) || requests.Load() != 0 {
				t.Fatalf("invalid ritual blocked by listening sync: err=%v requests=%d", err, requests.Load())
			}
			contributionMust(t, CancelBreakthroughRitual(ritual.ID, 1))
			contributionMust(t, CancelBreakthroughRitual(ritual.ID, 1))
			if balance := guardTestBalance(t, 2); balance != 100 {
				t.Fatalf("guard was not refunded exactly once: %d", balance)
			}
		})
	}
}

func TestValidBreakthroughRitualRemainsRetryableDuringABSOutage(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	contributionMust(t, guardTestJoin(ritual.ID, 2))
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1").UpdateColumn("last_fetched_at", time.Now().Add(-11*time.Minute)).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	_, err := ResolveBreakthrough(1, ritual.Mode, ritual.ID, time.Now().Add(2*time.Minute))
	if !errors.Is(err, ErrListeningNotFresh) || isBreakthroughCancellationError(err) {
		t.Fatalf("temporary sync failure did not remain retryable: %v", err)
	}
	contributionMust(t, storage.DB.First(ritual, ritual.ID).Error)
	if ritual.Status != BreakthroughRitualWaiting || guardTestBalance(t, 2) != 95 || guardTestBalance(t, 1) != 5000 {
		t.Fatal("temporary sync failure changed ritual or payments")
	}
}

func TestRankFreshnessIncludesUsersOutsideTopTen(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", failure), func(t *testing.T) {
			setupListeningFreshnessTest(t)
			contributionMust(t, storage.DB.AutoMigrate(&model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}))
			for id := int64(1); id <= 11; id++ {
				age := time.Minute
				if id == 11 {
					age = 11 * time.Minute
				}
				seedListeningFreshnessUser(t, id, age, .5)
			}
			var requests atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/listening-stats") {
					fmt.Fprint(w, `{"sessions":[]}`)
					return
				}
				requests.Add(1)
				seconds := 3600
				if strings.Contains(r.URL.Path, "/abs11/") {
					if failure {
						w.WriteHeader(503)
						return
					}
					seconds = 7200
				}
				fmt.Fprintf(w, `{"days":{%q:%d}}`, SectDayKey(time.Now()), seconds)
			})
			notice := ensureListeningRankFresh()
			if requests.Load() != 11 {
				t.Fatalf("did not refresh a full batch: %d requests", requests.Load())
			}
			var holder model.RankTitleHolder
			err := storage.DB.Where("title_key = ?", rankings.CultivationTitle).First(&holder).Error
			if failure {
				if !strings.Contains(notice, "排名可能滞后") || err == nil {
					t.Fatalf("partial batch published a title or hid staleness: notice=%s holder=%+v", notice, holder)
				}
			} else if notice != "" || err != nil || holder.UserID != 11 {
				t.Fatalf("stale user outside top ten did not become first: notice=%s holder=%+v err=%v", notice, holder, err)
			}
		})
	}
}

func TestSevenMinuteSourceCanSettleWithoutABS(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 7*time.Minute, .5)
	contributionMust(t, storage.DB.Model(&model.Cultivation{}).Where("user_id=1").UpdateColumn("listening_initialized", false).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	state := EnsureListeningFresh(1)
	if !state.Fresh || requests.Load() != 0 {
		t.Fatalf("fresh=%v requests=%d notice=%s", state.Fresh, requests.Load(), state.Notice)
	}
}

func TestTodayListeningReaderRejectsPreviousBinding(t *testing.T) {
	for _, binding := range []string{"replacement", "", "   "} {
		t.Run(fmt.Sprintf("binding=%q", binding), func(t *testing.T) {
			setupListeningFreshnessTest(t)
			seedListeningFreshnessUser(t, 1, time.Minute, .5)
			contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", binding).Error)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
			EnsureListeningFresh(1)
			if stat, ok, err := GetTodayDailyListeningStatChecked(1, time.Now()); err != nil || ok {
				t.Fatalf("previous binding accepted: stat=%+v ok=%v err=%v", stat, ok, err)
			}
			if _, ok := GetTodaySectListeningHoursFromCache(1, time.Now()); ok {
				t.Fatal("previous binding accepted for sect listening")
			}
		})
	}
}

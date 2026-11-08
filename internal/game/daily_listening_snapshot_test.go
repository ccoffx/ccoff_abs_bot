package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyListeningClearWithOlderStatisticsCannotIncreaseCredit(t *testing.T) {
	for _, scenario := range []struct{ unseenHours, remainingHours int64 }{{0, 0}, {6, 0}, {0, 3}, {6, 3}} {
		unseenHours, remainingHours := scenario.unseenHours, scenario.remainingHours
		t.Run(fmt.Sprintf("unseen_%dh_remaining_%dh", unseenHours, remainingHours), func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
			start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
			day := SectDayKey(start)
			var cleared, newPlayback atomic.Bool
			var rawHours atomic.Int64
			rawHours.Store(18 + unseenHours)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: float64(rawHours.Load()) * 3600}})
					return
				}
				a := bossSession("a", start, start.Add(6*time.Hour), 6*3600)
				b := bossSession("b", start, start.Add(6*time.Hour), 6*3600)
				c := bossSession("c", start.Add(6*time.Hour), start.Add(12*time.Hour), 6*3600)
				for _, s := range []map[string]interface{}{a, b, c} {
					s["date"] = day
				}
				if cleared.Load() {
					a["timeListening"] = remainingHours * 3600
					a["updatedAt"] = start.Add(7 * time.Hour).Format(time.RFC3339Nano)
				}
				sessions := []any{a, b, c}
				if newPlayback.Load() {
					d := bossSession("d", start.Add(12*time.Hour), start.Add(14*time.Hour), 2*3600)
					d["date"] = day
					sessions = append(sessions, d)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
			})
			want := float64(12+unseenHours) * 3600
			if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != want {
				t.Fatalf("initial=%v ok=%t want=%g", days, ok, want)
			}
			before := recoveryListeningDay(t, day)
			cleared.Store(true)
			for i := 0; i < 2; i++ {
				if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
					t.Fatal("stale statistics accepted")
				}
				after := recoveryListeningDay(t, day)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed refresh changed prior daily snapshot")
				}
				var row model.ABSCrossDaySessionAdjustment
				if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil {
					t.Fatal(err)
				}
				if row.Status != crossDayAdjustmentStatusSameDay || row.ListeningSeconds != 6*3600 {
					t.Fatalf("failed correction consumed its evidence: %+v", row)
				}
			}
			rawHours.Store(12 + remainingHours + unseenHours)
			if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != want {
				t.Fatalf("consistent retry=%v ok=%t", days, ok)
			}
			before = recoveryListeningDay(t, day)
			rawHours.Store(18 + unseenHours)
			for i := 0; i < 2; i++ {
				if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
					t.Fatal("stale total recreated cleared playback")
				}
				if after := recoveryListeningDay(t, day); !reflect.DeepEqual(before, after) {
					t.Fatal("late stale total changed successful snapshot")
				}
			}
			newPlayback.Store(true)
			rawHours.Store(14 + remainingHours + unseenHours)
			if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != want+7200 {
				t.Fatalf("new playback was blocked: %v %t", days, ok)
			}
		})
	}
}
func TestDailyListeningCrossDayCorrectionRetainsOrdinaryHistory(t *testing.T) {
	setupRetreatBackfillDB(t)
	now, _ := time.Parse(time.RFC3339, "2026-11-08T20:00:00+08:00")
	midnight := now.Add(-20 * time.Hour)
	previous, today := SectDayKey(midnight.Add(-time.Hour)), SectDayKey(midnight)
	a := absListeningSession{SessionKey: "a", DateKey: previous, StartedAt: midnight.Add(-time.Hour), UpdatedAt: midnight.Add(time.Hour), ListeningSeconds: 7200, HasListeningTime: true}
	b := absListeningSession{SessionKey: "b", DateKey: today, StartedAt: midnight.Add(time.Hour), UpdatedAt: midnight.Add(3 * time.Hour), ListeningSeconds: 7200, HasListeningTime: true}
	observed := time.Now().Add(-time.Hour)
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{previous: 7200, today: 7200}, absListeningSessionSnapshot{StartedAt: observed, Sessions: []absListeningSession{a, b}, Complete: true}); err != nil {
		t.Fatal(err)
	}
	// ABS corrects a to the same physical interval as b.
	a.DateKey, a.StartedAt, a.UpdatedAt = today, b.StartedAt, b.UpdatedAt
	official := map[string]float64{today: 14400}
	current := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true, Sessions: []absListeningSession{a, b}}
	cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", official, current)
	if err != nil {
		t.Fatal(err)
	}
	adjusted, _ := rebalanceABSDaysForCrossDaySessions(official, cross)
	if seconds, exists := adjusted[previous]; !exists || seconds != 0 {
		t.Fatalf("old allocation date was not cleared: %v", adjusted)
	}
	sessions, err := loadDailyListeningValidationSessions(1, "abs1", current, cross)
	if err != nil {
		t.Fatal(err)
	}
	if got := validateDailyListeningCredit(official, adjusted, sessions, now).Seconds[today]; got != 7200 {
		t.Fatalf("first corrected snapshot=%gh", got/3600)
	}
	// Both sessions subsequently move beyond recent five pages.
	later := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: false}
	cross, err = persistAndLoadABSCrossDaySessions(1, "abs1", official, later)
	if err != nil {
		t.Fatal(err)
	}
	adjusted, _ = rebalanceABSDaysForCrossDaySessions(official, cross)
	sessions, err = loadDailyListeningValidationSessions(1, "abs1", later, cross)
	if err != nil {
		t.Fatal(err)
	}
	got := validateDailyListeningCredit(official, adjusted, sessions, now).Seconds[today]
	var row model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if got != 7200 {
		t.Errorf("corrected history status=%s: credit changed from2h to%gh after pagination", row.Status, got/3600)
	}
	if row.Status != crossDayAdjustmentStatusSameDay || row.SourceDay != today || row.ListeningSeconds != 7200 {
		t.Fatalf("valid correction was not retained: %+v", row)
	}
	var retired []string
	if err := json.Unmarshal([]byte(row.RetiredDaysJSON), &retired); err != nil || !reflect.DeepEqual(retired, []string{previous, today}) {
		t.Fatalf("retired dates lost: %v err=%v", retired, err)
	}
	// A late reply carrying the original cross-day version cannot undo it.
	a.DateKey, a.StartedAt, a.UpdatedAt = previous, midnight.Add(-time.Hour), midnight.Add(time.Hour)
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", official, absListeningSessionSnapshot{StartedAt: observed, Sessions: []absListeningSession{a, b}}); err != nil {
		t.Fatal(err)
	}
	var replayed model.ABSCrossDaySessionAdjustment
	if err := storage.DB.First(&replayed, row.ID).Error; err != nil || !reflect.DeepEqual(row, replayed) {
		t.Fatalf("late original session replaced current history: %+v err=%v", replayed, err)
	}
	// Cleaning an old allocation must preserve unrelated official listening.
	withOtherListening := map[string]float64{previous: 3600, today: 14400}
	adjusted, _ = rebalanceABSDaysForCrossDaySessions(withOtherListening, cross)
	if adjusted[previous] != 3600 {
		t.Fatalf("retired date cleanup erased other listening: %v", adjusted)
	}
}
func TestDailyListeningMismatchedBudgetDoesNotSilentlyUndercountUnion(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var stage atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		i := stage.Load()
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: float64(8+4*i) * 3600}})
			return
		}
		a := bossSession("a", start, start.Add(8*time.Hour), 8*3600)
		b := bossSession("b", start.Add(4*time.Hour), start.Add(12*time.Hour), 8*3600)
		a["date"], b["date"] = day, day
		sessions := []any{a}
		if i > 0 {
			sessions = append(sessions, b)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 8*3600 {
		t.Fatalf("initial=%v %t", days, ok)
	}
	before := recoveryListeningDay(t, day)
	stage.Store(1)
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatalf("inconsistent budget accepted with credit=%v", days)
	}
	if after := recoveryListeningDay(t, day); !reflect.DeepEqual(before, after) {
		t.Fatal("mismatch replaced prior cache")
	}
	stage.Store(2)
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("consistent union=%v %t", days, ok)
	}
	if stat := recoveryListeningDay(t, day); stat.OfficialRawSeconds != 16*3600 || stat.CappedSeconds != 12*3600 {
		t.Fatalf("original and credited totals lost: %+v", stat)
	}
}

func TestDailyListeningBackfillCorrectionRequiresMatchingStatistics(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var cleared, failStats, caughtUp atomic.Bool
	var statsCalls atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			statsCalls.Add(1)
			if failStats.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			seconds := 18.0 * 3600
			if caughtUp.Load() {
				seconds = 12 * 3600
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: seconds}})
			return
		}
		a := bossSession("a", start, start.Add(6*time.Hour), 6*3600)
		b := bossSession("b", start, start.Add(6*time.Hour), 6*3600)
		c := bossSession("c", start.Add(6*time.Hour), start.Add(12*time.Hour), 6*3600)
		for _, s := range []map[string]interface{}{a, b, c} {
			s["date"] = day
		}
		if cleared.Load() {
			a["timeListening"] = 0
			a["updatedAt"] = start.Add(7 * time.Hour).Format(time.RFC3339Nano)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{a, b, c}})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	before := recoveryListeningDay(t, day)
	if err := EnsureABSCrossDaySessionScanCursor(1, "abs1"); err != nil {
		t.Fatal(err)
	}
	cleared.Store(true)
	batchNow := time.Now()
	for _, fail := range []bool{true, false} {
		failStats.Store(fail)
		calls := statsCalls.Load()
		if err := advanceABSCrossDaySessionBackfill(batchNow); err == nil {
			t.Fatal("inconsistent backfill correction accepted")
		}
		batchNow = batchNow.Add(absCrossDayBackfillRetryDelay)
		if statsCalls.Load() != calls+1 {
			t.Fatal("backfill reduction did not fetch fresh statistics")
		}
		var cursor model.ABSCrossDaySessionScanCursor
		if err := storage.DB.First(&cursor).Error; err != nil || cursor.NextPage != 0 || cursor.Completed {
			t.Fatalf("failed correction consumed page: %+v err=%v", cursor, err)
		}
		var row model.ABSCrossDaySessionAdjustment
		if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil || row.ListeningSeconds != 6*3600 || row.Status != crossDayAdjustmentStatusSameDay {
			t.Fatalf("failed backfill overwrote evidence: %+v err=%v", row, err)
		}
		if after := recoveryListeningDay(t, day); !reflect.DeepEqual(before, after) {
			t.Fatal("failed backfill changed cache")
		}
	}
	caughtUp.Store(true)
	if err := advanceABSCrossDaySessionBackfill(batchNow); err != nil {
		t.Fatal(err)
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&cursor).Error; err != nil || cursor.NextPage != 1 || !cursor.Completed {
		t.Fatalf("consistent retry did not commit: %+v err=%v", cursor, err)
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("corrected history=%v ok=%t", days, ok)
	}
}

func TestDailyListeningSourceRepairFindsNewHistoryOutsideRecentPages(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var hidden atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := 12.0 * 3600
			if hidden.Load() {
				seconds += 7200
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: seconds}})
			return
		}
		sessions := []any{}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if hidden.Load() && page < 5 {
			for i := 0; i < dailyListeningSessionPageSize; i++ {
				s := bossSession(fmt.Sprintf("filler-%d-%d", page, i), start, start.Add(time.Minute), 0)
				s["date"] = day
				sessions = append(sessions, s)
			}
		} else {
			for i, id := range []string{"a", "b", "c", "d"} {
				if id == "d" && !hidden.Load() {
					continue
				}
				a, b, seconds := start, start.Add(6*time.Hour), 6.0*3600
				if i == 0 {
					seconds = 0
				}
				if i == 2 {
					a, b = start.Add(6*time.Hour), start.Add(12*time.Hour)
				}
				if i == 3 {
					a, b, seconds = start.Add(12*time.Hour), start.Add(14*time.Hour), 7200
				}
				s := bossSession(id, a, b, seconds)
				s["date"] = day
				sessions = append(sessions, s)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("initial=%v %t", days, ok)
	}
	before := recoveryListeningDay(t, day)
	hidden.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("unconfirmed source remainder accepted")
	}
	if after := recoveryListeningDay(t, day); !reflect.DeepEqual(before, after) {
		t.Fatal("pending history repair changed cache")
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&cursor).Error; err != nil || cursor.ReconcileStartedAt == nil {
		t.Fatalf("source repair not scheduled: %+v err=%v", cursor, err)
	}
	for i := 0; i < 2; i++ {
		if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 14*3600 {
		t.Fatalf("new historical playback not recovered: %v %t", days, ok)
	}
}

func TestDailyListeningDeletedCrossDaySessionRejectsLaterStaleTotal(t *testing.T) {
	setupRetreatBackfillDB(t)
	midnight := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	midnight = time.Date(midnight.Year(), midnight.Month(), midnight.Day(), 0, 0, 0, 0, midnight.Location())
	source, target := SectDayKey(midnight.Add(-time.Hour)), SectDayKey(midnight)
	var stage atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		i := stage.Load()
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			days := map[string]float64{}
			if i != 1 {
				days[source] = 7200
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
			return
		}
		sessions := []any{}
		if i == 0 {
			s := bossSession("a", midnight.Add(-time.Hour), midnight.Add(time.Hour), 7200)
			s["date"] = source
			sessions = append(sessions, s)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[source] != 3600 || days[target] != 3600 {
		t.Fatalf("initial=%v %t", days, ok)
	}
	stage.Store(1)
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[source] != 0 || days[target] != 0 {
		t.Fatalf("confirmed deletion=%v %t", days, ok)
	}
	before := recoveryListeningDay(t, source)
	stage.Store(2)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("stale total recreated deleted cross-day playback")
	}
	if after := recoveryListeningDay(t, source); !reflect.DeepEqual(before, after) {
		t.Fatal("stale cross-day total changed successful snapshot")
	}
}

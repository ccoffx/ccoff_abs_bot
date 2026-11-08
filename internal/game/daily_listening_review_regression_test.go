package game

import (
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRejectedInvalidatedResponseCannotReenterDailyCredit(t *testing.T) {
	setupRetreatBackfillDB(t)
	midnight := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	midnight = time.Date(midnight.Year(), midnight.Month(), midnight.Day(), 0, 0, 0, 0, midnight.Location())
	source, target := SectDayKey(midnight.Add(-time.Hour)), SectDayKey(midnight)
	a := absListeningSession{SessionKey: "a", DateKey: source, StartedAt: midnight.Add(-time.Hour), UpdatedAt: midnight.Add(time.Hour), ListeningSeconds: 7200, HasListeningTime: true}
	official := map[string]float64{source: 7200}
	t0, t1, t2 := time.Now().Add(-3*time.Hour), time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	original := absListeningSessionSnapshot{StartedAt: t0, Sessions: []absListeningSession{a}, Complete: true}
	cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", official, original)
	if err != nil {
		t.Fatal(err)
	}
	adjusted, corrected := rebalanceABSDaysForCrossDaySessions(official, cross)
	sessions, err := loadDailyListeningValidationSessions(1, "abs1", original, cross)
	if err != nil {
		t.Fatal(err)
	}
	validation := validateDailyListeningCredit(official, adjusted, sessions, time.Now())
	if err := recordPreparedDailyListeningStats(1, "abs1", adjusted, corrected, t0, validation); err != nil {
		t.Fatal(err)
	}
	// Background history confirms deletion; daily cache is updated on the next refresh.
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{}, absListeningSessionSnapshot{StartedAt: t2, Complete: true}); err != nil {
		t.Fatal(err)
	}
	// The request began before deletion, and its old positive response arrives now.
	late := absListeningSessionSnapshot{StartedAt: t1, Sessions: []absListeningSession{a}, Complete: true}
	cross, err = persistAndLoadABSCrossDaySessions(1, "abs1", official, late)
	if err != nil {
		t.Fatal(err)
	}
	var row model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != crossDayAdjustmentStatusInvalidated {
		t.Fatalf("fixture did not reject old response: %+v", row)
	}
	if err := validateABSCrossDaySnapshot(official, cross); err != nil {
		t.Fatal(err)
	}
	adjusted, corrected = rebalanceABSDaysForCrossDaySessions(official, cross)
	sessions, err = loadDailyListeningValidationSessions(1, "abs1", late, cross)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateABSCrossDaySnapshot(official, latestDailyListeningSessions(sessions)); err != nil {
		t.Fatal(err)
	}
	if len(latestDailyListeningSessions(sessions)) != 0 {
		t.Fatal("rejected raw response reentered validation")
	}
	if err := validateABSListeningSourceRemainders(1, "abs1", official, sessions); err == nil {
		t.Fatal("stale source budget accepted after confirmed invalidation")
	}
	credited := recoveryListeningDay(t, source).CappedSeconds + recoveryListeningDay(t, target).CappedSeconds
	if credited != 7200 {
		t.Fatalf("failed stale refresh changed prior credit: %g", credited)
	}
	// A genuinely newer observation can restore a session.
	a.UpdatedAt = a.UpdatedAt.Add(time.Hour)
	a.ListeningSeconds = 10800
	official[source] = 10800
	restored := absListeningSessionSnapshot{StartedAt: time.Now(), Sessions: []absListeningSession{a}, Complete: true}
	cross, err = persistAndLoadABSCrossDaySessions(1, "abs1", official, restored)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err = loadDailyListeningValidationSessions(1, "abs1", restored, cross)
	if err != nil || len(latestDailyListeningSessions(sessions)) != 1 {
		t.Fatalf("new restoration lost: %+v %v", sessions, err)
	}

}

func TestLegacyNullValidationVersionReenrollsCompletedHistory(t *testing.T) {
	setupRetreatBackfillDB(t)
	cursor := model.ABSCrossDaySessionScanCursor{UserID: 1, AbsUserID: "abs1", NextPage: 8, Completed: true}
	if err := storage.DB.Create(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	// Simulate the old database, then run the exact additive schema upgrade.
	if err := storage.DB.Migrator().DropColumn(&model.ABSCrossDaySessionScanCursor{}, "daily_validation_version"); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(&model.ABSCrossDaySessionScanCursor{}); err != nil {
		t.Fatal(err)
	}
	var nullCount int64
	if err := storage.DB.Model(&model.ABSCrossDaySessionScanCursor{}).Where("daily_validation_version IS NULL").Count(&nullCount).Error; err != nil || nullCount != 1 {
		t.Fatalf("migration fixture: null=%d err=%v", nullCount, err)
	}
	if err := seedABSCrossDaySessionScanCursors(); err != nil {
		t.Fatal(err)
	}
	var after model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&after, cursor.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Completed || after.NextPage != 0 {
		t.Errorf("upgraded legacy cursor incorrectly skipped rescan: version=%d completed=%t page=%d", after.DailyValidationVersion, after.Completed, after.NextPage)
	}
}

func TestSplitCorrectionAcrossPagesResumesWithoutChangingPriorCredit(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var corrected atomic.Bool
	var otherRequests atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "abs2") {
			otherRequests.Add(1)
			fmt.Fprint(w, `{"sessions":[]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: 12 * 3600}})
			return
		}
		a := bossSession("a", start, start.Add(6*time.Hour), 6*3600)
		b := bossSession("b", start.Add(6*time.Hour), start.Add(12*time.Hour), 6*3600)
		d := bossSession("d", start.Add(12*time.Hour), start.Add(18*time.Hour), 6*3600)
		a["date"], b["date"], d["date"] = day, day, day
		if !corrected.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{a, b}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		sessions := []any{}
		if page < 5 {
			for i := 0; i < dailyListeningSessionPageSize; i++ {
				s := bossSession(fmt.Sprintf("filler-%d-%d", page, i), start, start.Add(time.Minute), 0)
				s["date"] = day
				if page == 0 && i == 0 {
					a["timeListening"] = 0
					a["updatedAt"] = start.Add(7 * time.Hour).Format(time.RFC3339Nano)
					s = a
				}
				sessions = append(sessions, s)
			}
		} else if page == 5 {
			sessions = []any{b, d}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("initial=%v %t", days, ok)
	}
	before := recoveryListeningDay(t, day)
	corrected.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("fixture expected pending history")
	}
	if err := storage.DB.Create(&model.User{TelegramID: 2, AbsUserID: "abs2", Username: "review_other", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnsureABSCrossDaySessionScanCursor(2, "abs2"); err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	var paused model.ABSCrossDaySessionScanCursor
	if err := storage.DB.Where("abs_user_id = ?", "abs1").First(&paused).Error; err != nil {
		t.Fatal(err)
	}
	if paused.NextPage != 5 || paused.PendingObservationsJSON == "" || paused.Completed {
		t.Fatalf("correction not durably deferred: %+v", paused)
	}
	var original model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.ListeningSeconds != 21600 || original.Status != crossDayAdjustmentStatusSameDay {
		t.Fatalf("pending correction overwrote known playback: %+v", original)
	}
	if got := recoveryListeningDay(t, day); !reflect.DeepEqual(before, got) {
		t.Fatal("partial scan changed daily cache")
	}
	// Each invocation reloads all progress and pending evidence from the DB,
	// exactly as a scanner restart does; no process-local pending state exists.
	for i := 0; i < 2; i++ {
		if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.Where("abs_user_id = ?", "abs1").First(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	if !cursor.Completed {
		t.Errorf("valid correction never reaches replacement session on page 5: page=%d retry=%v last_error=%q", cursor.NextPage, cursor.NextRetryAt, cursor.LastErrorCode)
	}
	if cursor.PendingObservationsJSON != "" {
		t.Fatal("completed scan kept pending corrections")
	}
	if got := recoveryListeningDay(t, day); !reflect.DeepEqual(before, got) {
		t.Fatal("background scan rewrote daily cache")
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 43200 {
		t.Fatalf("completed correction did not refresh: %v %t", days, ok)
	}
	if otherRequests.Load() == 0 {
		t.Error("one user's mismatch starved unrelated abs2 history across all batches")
	}
}

func TestBackfillMismatchBacksOffAndRescansEarlierPages(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var corrected, replacement atomic.Bool
	var requests, otherRequests atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "abs2") {
			otherRequests.Add(1)
			fmt.Fprint(w, `{"sessions":[]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: 43200}})
			return
		}
		requests.Add(1)
		a := bossSession("a", start, start.Add(6*time.Hour), 21600)
		b := bossSession("b", start.Add(6*time.Hour), start.Add(12*time.Hour), 21600)
		d := bossSession("d", start.Add(12*time.Hour), start.Add(18*time.Hour), 21600)
		a["date"], b["date"], d["date"] = day, day, day
		if !corrected.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{a, b}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		sessions := []any{b}
		if page < 5 {
			sessions = make([]any, dailyListeningSessionPageSize)
			for i := range sessions {
				s := bossSession(fmt.Sprintf("f-%d-%d", page, i), start, start.Add(time.Minute), 0)
				s["date"] = day
				sessions[i] = s
			}
			if page == 0 {
				a["timeListening"] = 0
				a["updatedAt"] = start.Add(7 * time.Hour).Format(time.RFC3339Nano)
				sessions[0] = a
				if replacement.Load() {
					sessions[1] = d
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	before := recoveryListeningDay(t, day)
	corrected.Store(true)
	if err := EnsureABSCrossDaySessionScanCursor(1, "abs1"); err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := advanceABSCrossDaySessionBackfill(now); err == nil {
		t.Fatal("inconsistent completed history accepted")
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.Where("abs_user_id = ?", "abs1").First(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	if cursor.NextPage != 0 || cursor.Completed || cursor.NextRetryAt == nil || !cursor.NextRetryAt.Equal(now.Add(absCrossDayBackfillRetryDelay)) || cursor.LastErrorCode == "" || cursor.ReconcileStartedAt == nil {
		t.Fatalf("failure did not schedule a fresh pass with backoff: %+v", cursor)
	}
	calls := requests.Load()
	if err := storage.DB.Create(&model.User{TelegramID: 2, AbsUserID: "abs2", Username: "other", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnsureABSCrossDaySessionScanCursor(2, "abs2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := advanceABSCrossDaySessionBackfill(now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != calls || otherRequests.Load() == 0 {
		t.Fatal("failed user retried too early or blocked another user")
	}
	if got := recoveryListeningDay(t, day); !reflect.DeepEqual(before, got) {
		t.Fatal("inconsistent pass changed daily cache")
	}
	var row model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil || row.ListeningSeconds != 21600 {
		t.Fatalf("failed pass discarded old evidence: %+v %v", row, err)
	}
	// Newly arrived evidence is on a page already visited in the failed pass.
	replacement.Store(true)
	retryAt := now.Add(absCrossDayBackfillRetryDelay)
	for i := 0; i < 2; i++ {
		if err := advanceABSCrossDaySessionBackfill(retryAt); err != nil {
			t.Fatal(err)
		}
	}
	cursor = model.ABSCrossDaySessionScanCursor{}
	if err := storage.DB.Where("abs_user_id = ?", "abs1").First(&cursor).Error; err != nil || !cursor.Completed || cursor.PendingObservationsJSON != "" {
		t.Fatalf("fresh pass did not complete: %+v %v", cursor, err)
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 43200 {
		t.Fatalf("recovered=%v %t", days, ok)
	}
}

func TestPendingCorrectionCannotReplaceNewerAcceptedObservation(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, storage.DailyOperationsLocation)
	day := SectDayKey(start)
	original := absListeningSession{SessionKey: "a", DateKey: day, StartedAt: start, UpdatedAt: start.Add(6 * time.Hour), ListeningSeconds: 21600, HasListeningTime: true}
	t0 := time.Now().Add(-time.Hour)
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 21600}, absListeningSessionSnapshot{StartedAt: t0, Sessions: []absListeningSession{original}}); err != nil {
		t.Fatal(err)
	}
	cleared := original
	cleared.ListeningSeconds = 0
	cleared.UpdatedAt = cleared.UpdatedAt.Add(time.Hour)
	pending, _ := buildABSCrossDaySessionObservations(1, "abs1", []absListeningSession{cleared}, t0.Add(time.Minute))
	// Foreground supplies a newer observation with the same ABS session clock.
	original.UpdatedAt = cleared.UpdatedAt
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 21600}, absListeningSessionSnapshot{StartedAt: t0.Add(2 * time.Minute), Sessions: []absListeningSession{original}}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	cursor := model.ABSCrossDaySessionScanCursor{UserID: 1, AbsUserID: "abs1", NextPage: 5, DailyValidationVersion: dailyListeningValidationVersion, PendingObservationsJSON: string(body)}
	if err := storage.DB.Create(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"sessions":[]}`) })
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	var row model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusSameDay || row.ListeningSeconds != 21600 || !row.ObservedAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("pending response replaced newer ledger: %+v %v", row, err)
	}
}

func TestDailyValidationUsesAcceptedSameClockVersion(t *testing.T) {
	for _, status := range []string{crossDayAdjustmentStatusDeleted, crossDayAdjustmentStatusInvalidated, crossDayAdjustmentStatusSameDay} {
		t.Run(status, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start := time.Date(2026, 11, 1, 0, 0, 0, 0, storage.DailyOperationsLocation)
			old := absListeningSession{SessionKey: "a", DateKey: SectDayKey(start), StartedAt: start, UpdatedAt: start.Add(6 * time.Hour), ListeningSeconds: 21600, HasListeningTime: true}
			rows, _ := buildABSCrossDaySessionObservations(1, "abs1", []absListeningSession{old}, time.Now())
			rows[0].Status = status
			if status == crossDayAdjustmentStatusSameDay {
				rows[0].ListeningSeconds = 10800
			}
			if err := storage.DB.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{old.DateKey: 21600}, absListeningSessionSnapshot{StartedAt: time.Now().Add(-time.Hour), Sessions: []absListeningSession{old}})
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := loadDailyListeningValidationSessions(1, "abs1", absListeningSessionSnapshot{Sessions: []absListeningSession{old}}, cross)
			if err != nil {
				t.Fatal(err)
			}
			active := latestDailyListeningSessions(sessions)
			if status == crossDayAdjustmentStatusSameDay {
				if len(active) != 1 || active[0].ListeningSeconds != 10800 {
					t.Fatalf("rejected same-clock payload returned: %+v", active)
				}
			} else if len(active) != 0 {
				t.Fatalf("tombstone resurrected: %+v", active)
			}
		})
	}
}

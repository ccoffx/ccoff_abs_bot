package game

import (
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDailyListeningZeroCorrectionPreservesValidBudget(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var zero atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := 18.0 * 3600
			if zero.Load() {
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
		if zero.Load() {
			a["timeListening"] = 0
			a["updatedAt"] = start.Add(7 * time.Hour).Format(time.RFC3339Nano)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{a, b, c}})
	})
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("initial=%v ok=%t", days, ok)
	}
	zero.Store(true)
	days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
	if !ok || days[day] != 12*3600 {
		t.Errorf("after ABS zeroed a: credited=%v ok=%t want12h", days, ok)
	}
	stat := recoveryListeningDay(t, day)
	if stat.OfficialRawSeconds != 12*3600 || stat.RawSeconds != 12*3600 || stat.CappedSeconds != 12*3600 {
		t.Fatalf("clear correction changed official/eligible totals incorrectly: %+v", stat)
	}
}

func TestDailyListeningConfirmedDeletionSurvivesIncompletePages(t *testing.T) {
	setupRetreatBackfillDB(t)
	now, _ := time.Parse(time.RFC3339, "2026-11-08T20:00:00+08:00")
	start := now.Add(-20 * time.Hour)
	day := SectDayKey(start)
	mk := func(id string, a, b int) absListeningSession {
		return absListeningSession{SessionKey: id, DateKey: day, StartedAt: start.Add(time.Duration(a) * time.Hour), UpdatedAt: start.Add(time.Duration(b) * time.Hour), ListeningSeconds: float64(b-a) * 3600, HasListeningTime: true}
	}
	old := []absListeningSession{mk("a", 0, 6), mk("b", 0, 6), mk("c", 6, 12)}
	observed := time.Now().Add(-time.Hour)
	rows, _ := buildABSCrossDaySessionObservations(1, "abs1", old, observed)
	if err := storage.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// A later complete snapshot proves a was removed. A subsequent five-page
	// snapshot no longer contains this old date, so the ledger is used again.
	current := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true, Sessions: old[1:]}
	official := map[string]float64{day: 12 * 3600}
	cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", official, current)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := loadDailyListeningValidationSessions(1, "abs1", current, cross)
	if err != nil {
		t.Fatal(err)
	}
	if got := validateDailyListeningCredit(official, official, sessions, now).Seconds[day]; got != 12*3600 {
		t.Fatalf("complete snapshot credited=%g", got/3600)
	}
	var deleted model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "a").First(&deleted).Error; err != nil || deleted.Status != crossDayAdjustmentStatusDeleted {
		t.Fatalf("deletion not persisted: %+v err=%v", deleted, err)
	}
	later := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: false}
	sessions, err = loadDailyListeningValidationSessions(1, "abs1", later, cross)
	if err != nil {
		t.Fatal(err)
	}
	if got := validateDailyListeningCredit(official, official, sessions, now).Seconds[day]; got != 12*3600 {
		t.Errorf("after history moved beyond recent pages credited=%gh want12h", got/3600)
	}
}

func TestDailyListeningBoundedCrossDayOverlapRemainsLowerBound(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-08T20:00:00+08:00")
	start := now.Add(-24 * time.Hour)
	source, target := SectDayKey(start), SectDayKey(now)
	sessions := []absListeningSession{}
	for i, pair := range [][2]float64{{12, 8}, {20, 9}} {
		s := absListeningSession{SessionKey: []string{"a", "b"}[i], DateKey: source, StartedAt: start, UpdatedAt: start.Add(time.Duration(pair[0]) * time.Hour), ListeningSeconds: pair[1] * 3600, HasListeningTime: true}
		s.CrossDayStatus, _, _, _ = classifyABSCrossDaySession(s)
		if s.CrossDayStatus != crossDayAdjustmentStatusBounded {
			t.Fatal("fixture not bounded")
		}
		if err := json.Unmarshal([]byte(crossDaySessionMinimumAllocationJSON(s)), &s.StoredAllocation); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	official := map[string]float64{source: 17 * 3600}
	adjusted, _ := rebalanceABSDaysForCrossDaySessions(official, sessions)
	got := validateDailyListeningCredit(official, adjusted, sessions, now)
	// Feasible playback: both listen yesterday20-24; a listens today00-04,
	// b listens today00-05. Today's true union can therefore be just5h.
	if math.Abs(got.Seconds[target]-5*3600) > 1e-6 {
		t.Errorf("claimed guaranteed today=%gh, feasible union=5h", got.Seconds[target]/3600)
	}
}

func TestDailyListeningZeroAndDeletionRejectLateResponses(t *testing.T) {
	for _, kind := range []string{"zero_same_clock", "zero_missing_clock", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
			start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
			day := SectDayKey(start)
			original := absListeningSession{SessionKey: "a", DateKey: day, StartedAt: start, UpdatedAt: start.Add(time.Hour), ListeningSeconds: 3600, HasListeningTime: true}
			oldRequest := time.Now().Add(-time.Minute)
			if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 3600}, absListeningSessionSnapshot{StartedAt: oldRequest, Sessions: []absListeningSession{original}}); err != nil {
				t.Fatal(err)
			}
			clear := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true}
			if kind != "deleted" {
				zero := original
				zero.ListeningSeconds = 0
				if kind == "zero_missing_clock" {
					zero.UpdatedAt = time.Time{}
				}
				clear.Sessions = []absListeningSession{zero}
			}
			if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 0}, clear); err != nil {
				t.Fatal(err)
			}
			var row model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusDeleted {
				t.Fatalf("missing tombstone: %+v err=%v", row, err)
			}
			// Even an apparently newer ABS clock cannot override a deletion if
			// this HTTP response was requested before the deletion was observed.
			late := original
			late.UpdatedAt = late.UpdatedAt.Add(time.Minute)
			oldSnapshot := absListeningSessionSnapshot{StartedAt: oldRequest.Add(time.Second), Sessions: []absListeningSession{late}}
			cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 3600}, oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := loadDailyListeningValidationSessions(1, "abs1", oldSnapshot, cross)
			if err != nil || len(latestDailyListeningSessions(sessions)) != 0 {
				t.Fatalf("late response restored deleted evidence: %+v err=%v", sessions, err)
			}
			// A genuinely new observation may confirm a returned session.
			oldSnapshot.StartedAt = time.Now()
			cross, err = persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 3600}, oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err = loadDailyListeningValidationSessions(1, "abs1", oldSnapshot, cross)
			if err != nil || len(latestDailyListeningSessions(sessions)) != 1 {
				t.Fatalf("new response could not restore evidence: %+v err=%v", sessions, err)
			}
		})
	}
}

func TestDailyListeningZeroedCrossDayClearsPreviousAllocation(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -3)
	start = time.Date(start.Year(), start.Month(), start.Day(), 23, 0, 0, 0, start.Location())
	s := absListeningSession{SessionKey: "cross", DateKey: SectDayKey(start), StartedAt: start, UpdatedAt: start.Add(2 * time.Hour), ListeningSeconds: 7200, HasListeningTime: true}
	day, target := s.DateKey, SectDayKey(s.UpdatedAt)
	cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 7200}, absListeningSessionSnapshot{StartedAt: time.Now().Add(-time.Minute), Sessions: []absListeningSession{s}})
	if err != nil {
		t.Fatal(err)
	}
	adjusted, _ := rebalanceABSDaysForCrossDaySessions(map[string]float64{day: 7200}, cross)
	if adjusted[target] != 3600 {
		t.Fatalf("initial cross-day allocation=%v", adjusted)
	}
	s.ListeningSeconds = 0
	cross, err = persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{}, absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true, Sessions: []absListeningSession{s}})
	if err != nil {
		t.Fatal(err)
	}
	adjusted, _ = rebalanceABSDaysForCrossDaySessions(map[string]float64{}, cross)
	if value, ok := adjusted[target]; !ok || value != 0 || adjusted[day] != 0 {
		t.Fatalf("cleared session left previous allocations: %v", adjusted)
	}
}

func TestDailyListeningOrdinaryDeletionBackfillResumes(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -3)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day, recentDay := SectDayKey(start), SectDayKey(start.AddDate(0, 0, 1))
	var deleted, failLast atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := 18.0 * 3600
			if deleted.Load() {
				seconds = 12 * 3600
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: seconds, recentDay: 500 * 60}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 5 && failLast.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sessions := []any{}
		if page < 5 {
			for i := 0; i < dailyListeningSessionPageSize; i++ {
				a := start.AddDate(0, 0, 1).Add(time.Duration(page*100+i) * time.Minute)
				s := bossSession(fmt.Sprintf("recent-%d-%d", page, i), a, a.Add(time.Minute), 60)
				s["date"] = recentDay
				sessions = append(sessions, s)
			}
		} else if page == 5 {
			for i, id := range []string{"a", "b", "c"} {
				if id == "a" && deleted.Load() {
					continue
				}
				a := start
				if i == 2 {
					a = a.Add(6 * time.Hour)
				}
				s := bossSession(id, a, a.Add(6*time.Hour), 6*3600)
				s["date"] = day
				sessions = append(sessions, s)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if err := EnsureABSCrossDaySessionScanCursor(1, "abs1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 12*3600 {
		t.Fatalf("initial credited=%v ok=%t", days, ok)
	}
	before := recoveryListeningDay(t, day)
	deleted.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("unconfirmed deletion spent a reduced budget on old sessions")
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&cursor).Error; err != nil || cursor.NextPage != 5 || cursor.ReconcileStartedAt == nil {
		t.Fatalf("reconciliation not scheduled: %+v err=%v", cursor, err)
	}
	// A refresh and a failed last page must not restart or finish the scan.
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("incomplete reconciliation accepted")
	}
	failLast.Store(true)
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err == nil {
		t.Fatal("failed final page accepted")
	}
	after := recoveryListeningDay(t, day)
	if after.CappedSeconds != before.CappedSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) {
		t.Fatal("failed reconciliation changed daily cache")
	}
	failLast.Store(false)
	if err := advanceABSCrossDaySessionBackfill(time.Now().Add(absCrossDayBackfillRetryDelay + time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || days[day] != 12*3600 {
			t.Fatalf("confirmed deletion did not preserve valid playback: %v %t", days, ok)
		}
	}
}

func TestDailyListeningMinimumCoverageMatchesExhaustiveSchedules(t *testing.T) {
	start := time.Date(2026, 11, 8, 0, 0, 0, 0, storage.DailyOperationsLocation)
	random := rand.New(rand.NewSource(42))
	for trial := 0; trial < 500; trial++ {
		type requirement struct{ left, right, minimum int }
		requirements := []requirement{}
		segments := []dailyListeningSegment{}
		for i, count := 0, 1+random.Intn(5); i < count; i++ {
			left := random.Intn(8)
			right := left + 1 + random.Intn(8-left)
			minimum := 1 + random.Intn(right-left)
			requirements = append(requirements, requirement{left, right, minimum})
			segments = append(segments, dailyListeningSegment{start: start.Add(time.Duration(left) * time.Second), end: start.Add(time.Duration(right) * time.Second), density: float64(minimum) / float64(right-left), uncertain: true})
		}
		want := 8
		for mask := uint(0); mask < 1<<8; mask++ {
			count := bits.OnesCount(mask)
			if count >= want {
				continue
			}
			valid := true
			for _, r := range requirements {
				if bits.OnesCount((mask>>r.left)&((1<<(r.right-r.left))-1)) < r.minimum {
					valid = false
					break
				}
			}
			if valid {
				want = count
			}
		}
		for pass := 0; pass < 2; pass++ {
			got, _ := unionDailyListeningSegments(segments)
			if math.Abs(got-float64(want)) > 1e-8 {
				t.Fatalf("trial=%d requirements=%v got=%g want=%d", trial, requirements, got, want)
			}
			random.Shuffle(len(segments), func(i, j int) { segments[i], segments[j] = segments[j], segments[i] })
		}
	}
}

func TestDailyListeningDeletionRequiresMatchingTotalsAndCommitsAtomically(t *testing.T) {
	for _, kind := range []string{"unchanged_stats", "inconsistent_stats", "write_failure"} {
		t.Run(kind, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
			start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
			day := SectDayKey(start)
			sessions := []absListeningSession{}
			for i, id := range []string{"a", "b", "c"} {
				a := start
				if i == 2 {
					a = a.Add(6 * time.Hour)
				}
				sessions = append(sessions, absListeningSession{SessionKey: id, DateKey: day, StartedAt: a, UpdatedAt: a.Add(6 * time.Hour), ListeningSeconds: 6 * 3600, HasListeningTime: true})
			}
			observedAt := time.Now().Add(-time.Minute)
			if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{day: 18 * 3600}, absListeningSessionSnapshot{StartedAt: observedAt, Sessions: sessions}); err != nil {
				t.Fatal(err)
			}
			seconds := 18.0 * 3600
			if kind == "inconsistent_stats" {
				seconds = 14 * 3600
			}
			if kind == "write_failure" {
				seconds = 12 * 3600
				if err := storage.DB.Exec("CREATE TRIGGER reject_session_deletion BEFORE UPDATE OF status ON abs_cross_day_session_adjustments WHEN NEW.status = 'deleted' BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			snapshot := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true, Sessions: sessions[1:]}
			official := map[string]float64{day: seconds}
			cross, err := persistAndLoadABSCrossDaySessions(1, "abs1", official, snapshot)
			if kind == "write_failure" {
				if err == nil {
					t.Fatal("deletion transaction failure was ignored")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var row model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "a").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusSameDay {
				t.Fatalf("unconfirmed/failed deletion retired evidence: %+v err=%v", row, err)
			}
			if kind == "write_failure" {
				row = model.ABSCrossDaySessionAdjustment{}
				if err := storage.DB.Where("session_key = ?", "b").First(&row).Error; err != nil || !row.ObservedAt.Equal(observedAt) {
					t.Fatalf("failed transaction advanced another observation: %+v err=%v", row, err)
				}
				return
			}
			loaded, err := loadDailyListeningValidationSessions(1, "abs1", snapshot, cross)
			if err != nil {
				t.Fatal(err)
			}
			evidence := missingABSListeningSessionEvidence(official, snapshot, loaded)
			if kind == "inconsistent_stats" && len(evidence.MissingSessions) != 1 {
				t.Fatalf("inconsistent deletion did not require reconciliation: %+v", evidence)
			}
			if kind == "unchanged_stats" {
				credit := validateDailyListeningCredit(official, official, loaded, time.Now())
				if credit.Seconds[day] != 12*3600 || len(evidence.MissingSessions) != 0 {
					t.Fatalf("partial response discarded known overlap: %+v evidence=%+v", credit, evidence)
				}
			}
		})
	}
}

func TestDailyListeningConservativeUnionWithMixedEvidence(t *testing.T) {
	start := time.Date(2026, 11, 8, 0, 0, 0, 0, storage.DailyOperationsLocation)
	segment := func(a, b, seconds float64, uncertain bool) dailyListeningSegment {
		return dailyListeningSegment{start: start.Add(time.Duration(a * float64(time.Second))), end: start.Add(time.Duration(b * float64(time.Second))), density: seconds / (b - a), uncertain: uncertain}
	}
	for _, tc := range []struct {
		name     string
		segments []dailyListeningSegment
		want     float64
	}{
		{"partly overlapping lower bounds", []dailyListeningSegment{segment(0, 10, 8, true), segment(5, 15, 8, true)}, 11},
		{"full playback overlaps uncertain playback", []dailyListeningSegment{segment(0, 4, 4, false), segment(0, 12, 6, true)}, 6},
		{"paused estimate does not invent a playback position", []dailyListeningSegment{segment(0, 8, 4, false), segment(0, 4, 4, true)}, 4},
		{"isolated ordinary estimates retain existing policy", []dailyListeningSegment{segment(0, 8, 4, false), segment(4, 12, 4, false), segment(20, 22, 1, true)}, 7},
		{"fractional intervals", []dailyListeningSegment{segment(0, 8, 4, true), segment(0, 16, 5.5, true)}, 5.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := unionDailyListeningSegments(tc.segments)
			if math.Abs(got-tc.want) > 1e-8 {
				t.Fatalf("credited=%g want=%g", got, tc.want)
			}
		})
	}
}

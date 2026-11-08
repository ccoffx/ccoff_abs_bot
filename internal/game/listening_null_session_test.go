package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

// Reproduce the production failure: a removed 0.071915-second session plus
// an unrelated ABS session whose timeListening is explicitly null.
func nullListeningFixture(t *testing.T, emptyKind string, mismatch bool) string {
	t.Helper()
	setupRetreatBackfillDB(t)
	start := storage.DailyOperationsLocalTime(time.Now()).AddDate(0, 0, -1)
	start = time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	const retained = 2565.7204967320545
	const removed = 0.07191503047943115
	var changed atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		seconds := retained + removed
		if changed.Load() {
			seconds = retained
			if mismatch {
				seconds--
			}
		}
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: seconds}})
			return
		}
		kept := bossSession("retained", start, start.Add(time.Hour), retained)
		kept["date"] = day
		extra := bossSession("removed", start.Add(2*time.Hour), start.Add(2*time.Hour+time.Second), removed)
		extra["date"] = day
		if changed.Load() {
			extra["id"] = "empty"
			switch emptyKind {
			case "null", "cross_null":
				extra["timeListening"] = nil
				if emptyKind == "cross_null" {
					extra["startedAt"] = start.Add(12*time.Hour - 30*time.Second).Format(time.RFC3339Nano)
					extra["updatedAt"] = start.Add(12*time.Hour + 30*time.Second).Format(time.RFC3339Nano)
				}
			case "missing":
				delete(extra, "timeListening")
			case "string":
				extra["timeListening"] = "invalid"
			case "negative":
				extra["timeListening"] = -1
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{kept, extra}})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial snapshot failed")
	}
	changed.Store(true)
	return day
}

func TestNullListeningSessionAllowsConfirmedDeletionAndMonitorCompletion(t *testing.T) {
	for _, kind := range []string{"null", "cross_null"} {
		t.Run(kind, func(t *testing.T) {
			day := nullListeningFixture(t, kind, false)
			before := recoveryListeningDay(t, day)
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("explicit null session blocked confirmed removal")
			}
			after := recoveryListeningDay(t, day)
			if math.Abs(after.RawSeconds-2565.7204967320545) > 1e-6 || !after.LastFetchedAt.After(before.LastFetchedAt) {
				t.Fatalf("daily statistics did not recover: %+v", after)
			}
			var removed model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "removed").First(&removed).Error; err != nil {
				t.Fatal(err)
			}
			if removed.Status != crossDayAdjustmentStatusDeleted || removed.Reason != "missing_from_complete_history" {
				t.Fatalf("old session was not retired: %+v", removed)
			}
			var emptyCount int64
			if err := storage.DB.Model(&model.ABSCrossDaySessionAdjustment{}).Where("session_key = ?", "empty").Count(&emptyCount).Error; err != nil || emptyCount != 0 {
				t.Fatalf("null session became playback/correction evidence: count=%d err=%v", emptyCount, err)
			}
			date, _ := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
			RunListeningAbuseMonitorIfNeeded(nil, date.AddDate(0, 0, 1).Add(10*time.Minute))
			if completed, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || completed != day {
				t.Fatalf("monitor remains blocked: completed=%q err=%v", completed, err)
			}
		})
	}
}

func TestNullListeningSessionDoesNotWeakenHistoryValidation(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		mismatch   bool
	}{
		{"missing field", "missing", false},
		{"malformed field", "string", false},
		{"negative field", "negative", false},
		{"null with inconsistent total", "null", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			day := nullListeningFixture(t, tc.kind, tc.mismatch)
			before := recoveryListeningDay(t, day)
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
				t.Fatal("unverified deletion was accepted")
			}
			after := recoveryListeningDay(t, day)
			if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) {
				t.Fatal("failed refresh changed the last successful snapshot")
			}
			var old model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "removed").First(&old).Error; err != nil || old.Status != crossDayAdjustmentStatusSameDay {
				t.Fatalf("unverified history was discarded: %+v err=%v", old, err)
			}
		})
	}
}

func TestNullListeningSessionAllowsProgressiveHistoryRepair(t *testing.T) {
	for _, kind := range []string{"null", "cross_null"} {
		t.Run(kind, func(t *testing.T) {
			day := nullListeningFixture(t, kind, false)
			evidence := newABSCrossDayReconciliationEvidence()
			evidence.addCandidate("removed", day)
			if err := scheduleABSListeningReconciliationEvidence(1, "abs1", evidence); err != nil {
				t.Fatal(err)
			}
			if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
				t.Fatal(err)
			}
			var old model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "removed").First(&old).Error; err != nil || old.Status != crossDayAdjustmentStatusDeleted {
				t.Fatalf("background repair did not retire old session: %+v err=%v", old, err)
			}
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("refresh did not recover after background repair")
			}
		})
	}
}

func TestNullListeningTimeDoesNotClearKnownPositiveSession(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := storage.DailyOperationsLocalTime(time.Now()).AddDate(0, 0, -1)
	start = time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var changed atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := 60
			if changed.Load() {
				seconds = 0
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]int{day: seconds}})
			return
		}
		s := bossSession("known", start, start.Add(time.Minute), 60)
		s["date"] = day
		if changed.Load() {
			s["timeListening"] = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{s}})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial snapshot failed")
	}
	changed.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("null treated as an explicit zero correction")
	}
	if stat := recoveryListeningDay(t, day); stat.RawSeconds != 60 {
		t.Fatalf("null erased confirmed playback: %+v", stat)
	}
}

func TestABSBackfillUnknownDurationPreservesPendingCorrection(t *testing.T) {
	now := time.Now()
	valid := model.ABSCrossDaySessionAdjustment{
		AbsUserID: "abs1", SessionKey: "pending-correction", ListeningSeconds: 30,
		Status: crossDayAdjustmentStatusAccepted, SessionUpdatedAt: now, ObservedAt: now,
	}
	unknown := valid
	unknown.ListeningSeconds = 0
	unknown.Status, unknown.Reason = crossDayAdjustmentStatusUncertain, "listening_invalid"
	unknown.SessionUpdatedAt, unknown.ObservedAt = now.Add(time.Minute), now.Add(time.Minute)
	merged := mergeABSBackfillObservations([]model.ABSCrossDaySessionAdjustment{valid}, []model.ABSCrossDaySessionAdjustment{unknown})
	if len(merged) != 1 || merged[0].Status != valid.Status || merged[0].ListeningSeconds != valid.ListeningSeconds || !merged[0].SessionUpdatedAt.Equal(valid.SessionUpdatedAt) {
		t.Fatalf("unknown duration replaced a pending valid correction: %+v", merged)
	}
}

func TestCrossDayUnknownListeningTimeDoesNotClearKnownPositiveSession(t *testing.T) {
	for _, tc := range []struct {
		kind                       string
		progressive, legacyPending bool
	}{
		{"null", false, false}, {"null", true, false}, {"null", true, true},
		{"missing", false, false}, {"malformed", false, false}, {"negative", false, false},
	} {
		t.Run(fmt.Sprintf("%s/progressive_%t/legacy_pending_%t", tc.kind, tc.progressive, tc.legacyPending), func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start := storage.DailyOperationsLocalTime(time.Now()).AddDate(0, 0, -2)
			start = time.Date(start.Year(), start.Month(), start.Day(), 23, 59, 30, 0, start.Location())
			day := SectDayKey(start)
			var changed atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					seconds := 60
					if changed.Load() {
						seconds = 0
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]int{day: seconds}})
					return
				}
				s := bossSession("known-cross-day", start, start.Add(time.Minute), 60)
				s["date"] = day
				if changed.Load() {
					switch tc.kind {
					case "null":
						s["timeListening"] = nil
					case "missing":
						delete(s, "timeListening")
					case "malformed":
						s["timeListening"] = "invalid"
					case "negative":
						s["timeListening"] = -1
					}
					s["updatedAt"] = start.Add(2 * time.Minute).UnixMilli()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{s}})
			})
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("initial cross-day snapshot failed")
			}
			before := recoveryListeningDay(t, day)
			nextDay := SectDayKey(start.Add(time.Minute))
			beforeNextDay := recoveryListeningDay(t, nextDay)
			changed.Store(true)
			if tc.progressive {
				evidence := newABSCrossDayReconciliationEvidence()
				evidence.addCandidate("known-cross-day", day)
				if err := scheduleABSListeningReconciliationEvidence(1, "abs1", evidence); err != nil {
					t.Fatal(err)
				}
				if tc.legacyPending {
					// Older scanners deferred this uncertain zero when a null
					// cross-day observation appeared before their final page.
					pending, err := json.Marshal([]model.ABSCrossDaySessionAdjustment{{
						UserID: 1, AbsUserID: "abs1", SessionKey: "known-cross-day", SourceDay: day,
						StartedAt: start, SessionUpdatedAt: start.Add(2 * time.Minute), ListeningSeconds: 0,
						Status: crossDayAdjustmentStatusUncertain, Confidence: crossDayAdjustmentConfidenceUncertain,
						Reason: "listening_invalid", AlgorithmVersion: dailyListeningCrossDayAlgorithmVersion, ObservedAt: time.Now(),
					}})
					if err != nil {
						t.Fatal(err)
					}
					if err := storage.DB.Model(&model.ABSCrossDaySessionScanCursor{}).Where("abs_user_id = ?", "abs1").Update("pending_observations_json", string(pending)).Error; err != nil {
						t.Fatal(err)
					}
				}
				if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
				t.Fatal("unknown cross-day duration treated as an explicit zero correction")
			}
			after := recoveryListeningDay(t, day)
			if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) {
				t.Fatal("unknown duration erased the confirmed cross-day snapshot")
			}
			afterNextDay := recoveryListeningDay(t, nextDay)
			if afterNextDay.RawSeconds != beforeNextDay.RawSeconds || !afterNextDay.LastFetchedAt.Equal(beforeNextDay.LastFetchedAt) {
				t.Fatal("unknown duration erased the confirmed next-day snapshot")
			}
			var known model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "known-cross-day").First(&known).Error; err != nil || known.ListeningSeconds != 60 || known.Status != crossDayAdjustmentStatusAccepted {
				t.Fatalf("unknown duration replaced the confirmed ledger entry: %+v err=%v", known, err)
			}
		})
	}
}

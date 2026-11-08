package game

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyListeningRecomputesDatesWithoutStoredAllocations(t *testing.T) {
	for _, sessionKind := range []string{"uncertain_cross_day", "same_day_other_source"} {
		for _, correction := range []string{"zero", "missing_foreground", "missing_background", "moved"} {
			t.Run(sessionKind+"/"+correction, func(t *testing.T) {
				setupRetreatBackfillDB(t)
				dayStart := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -5)
				dayStart = time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 0, 0, 0, 0, dayStart.Location())
				source, target := SectDayKey(dayStart), SectDayKey(dayStart.AddDate(0, 0, 1))
				otherDay := SectDayKey(dayStart.AddDate(0, 0, -1))
				start, end, listened := dayStart.Add(23*time.Hour), dayStart.Add(25*time.Hour), 10800.0
				if sessionKind == "same_day_other_source" {
					start, end, listened = dayStart.Add(25*time.Hour), dayStart.Add(26*time.Hour), 3600
				}
				var changed atomic.Bool
				bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
					seconds := listened
					if changed.Load() && correction != "moved" {
						seconds = 0
					}
					if strings.HasSuffix(r.URL.Path, "/listening-stats") {
						days := map[string]float64{otherDay: 900}
						if seconds > 0 {
							days[source] = seconds
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
						return
					}
					sessions := []any{}
					if !changed.Load() || !strings.HasPrefix(correction, "missing_") {
						a, b := start, end
						if changed.Load() {
							if correction == "zero" {
								// A clear can reset both timestamps, losing its old physical dates.
								a, b = time.Time{}, time.Time{}
							} else {
								a, b = a.AddDate(0, 0, 2), b.AddDate(0, 0, 2)
							}
						}
						s := bossSession("allocation-free", a, b, seconds)
						s["date"] = source
						sessions = append(sessions, s)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
				})

				days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
				if !ok || days[target] != 3600 {
					t.Fatalf("initial credit=%v ok=%t", days, ok)
				}
				var row model.ABSCrossDaySessionAdjustment
				if err := storage.DB.Where("session_key = ?", "allocation-free").First(&row).Error; err != nil || row.AllocationJSON != "" {
					t.Fatalf("expected observation without a stored allocation: %+v err=%v", row, err)
				}
				changed.Store(true)
				if correction == "missing_background" {
					evidence := newABSCrossDayReconciliationEvidence()
					evidence.addCandidate(row.SessionKey, row.SourceDay)
					if err := scheduleABSListeningReconciliationEvidence(1, "abs1", evidence); err != nil {
						t.Fatal(err)
					}
					if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
						t.Fatal(err)
					}
				}

				for attempt := 0; attempt < 2; attempt++ {
					days, ok = refreshDailyListeningStatsFromABS(1, "abs1", true)
					if !ok {
						t.Fatalf("refresh after correction failed: %v", days)
					}
					stat := recoveryListeningDay(t, target)
					if value, exists := days[target]; !exists || value != 0 || stat.CappedSeconds != 0 || stat.EffectiveHours != 0 {
						t.Fatalf("old target date retained credit: days=%v stat=%+v", days, stat)
					}
					if days[otherDay] != 900 || recoveryListeningDay(t, otherDay).CappedSeconds != 900 {
						t.Fatalf("unobserved official history was changed: %v", days)
					}
					if correction == "moved" {
						newTarget := SectDayKey(dayStart.AddDate(0, 0, 3))
						if days[newTarget] != 3600 || recoveryListeningDay(t, newTarget).CappedSeconds != 3600 {
							t.Fatalf("corrected session lost its new allocation: %v", days)
						}
					}
				}
			})
		}
	}
}

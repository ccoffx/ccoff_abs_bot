package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func recoveryListeningDay(t *testing.T, day string) model.DailyListeningStat {
	t.Helper()
	var stat model.DailyListeningStat
	if err := storage.DB.Where("user_id = ? AND day_key = ?", 1, day).First(&stat).Error; err != nil {
		t.Fatal(err)
	}
	return stat
}

func recoveryCrossDaySession() (map[string]interface{}, string, string) {
	now := time.Now().In(leaderboardLocation)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, leaderboardLocation)
	session := bossSession("cross", midnight.Add(-time.Hour), midnight.Add(time.Hour), 7200)
	source, target := SectDayKey(midnight.Add(-time.Hour)), SectDayKey(midnight)
	session["date"] = source
	return session, source, target
}

func TestDeletedCrossDaySessionRecoversAndClearsOldAllocation(t *testing.T) {
	for _, remaining := range []float64{0, 3600} {
		t.Run(fmt.Sprintf("remaining=%g", remaining), func(t *testing.T) {
			setupRetreatBackfillDB(t)
			session, source, today := recoveryCrossDaySession()
			start, _ := time.Parse(time.RFC3339Nano, session["startedAt"].(string))
			ordinary := bossSession("remaining", start.Add(-2*time.Hour), start.Add(-time.Hour), remaining)
			ordinary["date"] = source
			var deleted atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					days := map[string]float64{source: 7200 + remaining, today: 60}
					if deleted.Load() {
						days = map[string]float64{today: 600}
						if remaining > 0 {
							days[source] = remaining
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
					return
				}
				sessions := []any{}
				if !deleted.Load() {
					sessions = append(sessions, session)
				}
				if remaining > 0 {
					sessions = append(sessions, ordinary)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
			})
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("initial refresh failed")
			}
			deleted.Store(true)
			for i := 0; i < 3; i++ {
				if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
					t.Fatal("verified deletion still blocks refresh")
				}
				if got := recoveryListeningDay(t, today).RawSeconds; got != 600 {
					t.Fatalf("today=%g, want 600", got)
				}
				if got := recoveryListeningDay(t, source).RawSeconds; got != remaining {
					t.Fatalf("source=%g, want %g", got, remaining)
				}
			}
			var row model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "cross").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusInvalidated {
				t.Fatalf("deletion not recorded: %+v, %v", row, err)
			}
		})
	}
}

func TestMissingCrossDaySourceCannotOverwriteCorrectedToday(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint("empty_days=", empty), func(t *testing.T) {
			setupRetreatBackfillDB(t)
			session, source, today := recoveryCrossDaySession()
			var omitSource atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					days := map[string]float64{source: 7200}
					if omitSource.Load() {
						days = map[string]float64{}
						if !empty {
							days[today] = 60
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
			})
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("initial refresh failed")
			}
			before := recoveryListeningDay(t, today)
			omitSource.Store(true)
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
				t.Fatal("missing source day accepted despite live session evidence")
			}
			after := recoveryListeningDay(t, today)
			if after.RawSeconds != 3600 || !before.LastFetchedAt.Equal(after.LastFetchedAt) || after.Source != dailyListeningCrossDaySource {
				t.Fatalf("corrected cache overwritten: %+v", after)
			}
		})
	}
}

func TestMissingSourceAllowsAbuseEvidenceWithoutWritingDailyCache(t *testing.T) {
	setupRetreatBackfillDB(t)
	session, source, today := recoveryCrossDaySession()
	var omitSource atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			days := map[string]float64{source: 7200}
			if omitSource.Load() {
				days = map[string]float64{today: 60}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	before := recoveryListeningDay(t, today)
	omitSource.Store(true)
	proven, ok := refreshDailyListeningStatsFromABS(1, "abs1", true, today, source)
	if !ok || proven[today] != 3600 || proven[source] != 3600 {
		t.Fatalf("missing source suppressed guaranteed session evidence: %v, %t", proven, ok)
	}
	after := recoveryListeningDay(t, today)
	if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) || after.Source != before.Source {
		t.Fatalf("evidence-only scan changed daily cache: %+v", after)
	}
}

func TestPersonalReportUsesCorrectedCacheWhenRefreshFails(t *testing.T) {
	for _, failure := range []string{"sessions", "snapshot", "missing_days"} {
		t.Run(failure, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			session, source, today := recoveryCrossDaySession()
			var fail atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/listening-stats"):
					payload := map[string]any{"totalTime": 7200, "days": map[string]float64{source: 7200}}
					if fail.Load() && failure == "snapshot" {
						payload["days"] = map[string]float64{source: 3600}
					}
					if fail.Load() && failure == "missing_days" {
						delete(payload, "days")
					}
					_ = json.NewEncoder(w).Encode(payload)
				case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
					if fail.Load() && failure == "sessions" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
				default:
					fmt.Fprint(w, `{"mediaProgress":[]}`)
				}
			})
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
				t.Fatal("initial refresh failed")
			}
			before := recoveryListeningDay(t, today)
			fail.Store(true)
			report := GetPersonalReport(abs.DefaultClient, "abs1")
			for _, want := range []string{"今日实际听书: 1.00 小时", "今日净修为: 1.00 小时", "净修炼时长: 2.00 小时", "本次刷新失败", "使用上次成功统计"} {
				if !strings.Contains(report, want) {
					t.Errorf("report missing %q: %s", want, report)
				}
			}
			after := recoveryListeningDay(t, today)
			if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) {
				t.Fatalf("failed report changed cache: %+v", after)
			}
		})
	}
}

func TestPersonalReportWithoutCacheDoesNotDisplayUncorrectedZero(t *testing.T) {
	setupRetreatBackfillDB(t)
	_, source, _ := recoveryCrossDaySession()
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 7200, "days": map[string]float64{source: 7200}})
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, `{"mediaProgress":[]}`)
		}
	})
	report := GetPersonalReport(abs.DefaultClient, "abs1")
	for _, want := range []string{"今日实际听书: 读取失败", "今日净修为: 读取失败", "净修炼时长: 读取失败", "暂无可用的每日统计缓存"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q: %s", want, report)
		}
	}
}

func TestHistoricalDeletionReconciliationResumesAndCommitsAtomically(t *testing.T) {
	setupRetreatBackfillDB(t)
	session, _, today := recoveryCrossDaySession()
	start := time.Now().In(leaderboardLocation).AddDate(0, 0, -10)
	start = time.Date(start.Year(), start.Month(), start.Day(), 23, 0, 0, 0, leaderboardLocation)
	source, target := SectDayKey(start), SectDayKey(start.Add(2*time.Hour))
	session["startedAt"], session["updatedAt"], session["date"] = start.Format(time.RFC3339Nano), start.Add(2*time.Hour).Format(time.RFC3339Nano), source
	ordinary := bossSession("remaining", start.Add(-2*time.Hour), start.Add(-time.Hour), 3600)
	ordinary["date"] = source
	var deleted, failLastPage atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			days := map[string]float64{source: 10800, today: 60}
			if deleted.Load() {
				days[source], days[today] = 3600, 500
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
			return
		}
		if !deleted.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session, ordinary}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 5 {
			if failLastPage.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{ordinary}})
			return
		}
		if page > 5 {
			t.Errorf("unexpected page %d", page)
		}
		sessions := make([]any, dailyListeningSessionPageSize)
		for i := range sessions {
			recent := bossSession(fmt.Sprintf("recent-%d-%d", page, i), start.AddDate(0, 0, 10), start.AddDate(0, 0, 10).Add(time.Second), 1)
			recent["date"] = today
			sessions[i] = recent
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	deleted.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("truncated history was treated as proof of deletion")
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&cursor).Error; err != nil || cursor.NextPage != 5 || cursor.Completed || cursor.ReconcileStartedAt == nil {
		t.Fatalf("repair did not pause after five pages: %+v, %v", cursor, err)
	}
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("unverified deletion accepted")
	}
	var resumed model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&resumed).Error; err != nil || resumed.NextPage != 5 || !resumed.ReconcileStartedAt.Equal(*cursor.ReconcileStartedAt) {
		t.Fatalf("manual refresh restarted repair: %+v, %v", resumed, err)
	}
	failLastPage.Store(true)
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err == nil {
		t.Fatal("last-page failure was ignored")
	}
	failLastPage.Store(false)
	if err := storage.DB.Exec("CREATE TRIGGER reject_repair BEFORE UPDATE OF next_page ON abs_cross_day_session_scan_cursors BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now().Add(absCrossDayBackfillRetryDelay)); err == nil {
		t.Fatal("cursor failure did not abort repair")
	}
	var adjustment model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "cross").First(&adjustment).Error; err != nil || adjustment.Status != crossDayAdjustmentStatusAccepted {
		t.Fatalf("failed repair committed invalidation: %+v, %v", adjustment, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER reject_repair").Error; err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now().Add(absCrossDayBackfillRetryDelay)); err != nil {
		t.Fatal(err)
	}
	resumed = model.ABSCrossDaySessionScanCursor{}
	if err := storage.DB.First(&resumed, cursor.ID).Error; err != nil || !resumed.Completed || resumed.ReconcileStartedAt != nil {
		t.Fatalf("repair failed to finish: %+v, %v", resumed, err)
	}
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("verified historical deletion still blocks refresh")
	}
	if got := recoveryListeningDay(t, today).RawSeconds; got != 500 {
		t.Fatalf("today=%g, want 500", got)
	}
	if got := recoveryListeningDay(t, target).RawSeconds; got != 0 {
		t.Fatalf("obsolete historical target allocation survived: %g", got)
	}
}

func TestCrossDayInvalidationRequiresCompleteConsistentEvidence(t *testing.T) {
	for _, failure := range []string{"partial_history", "stats_disagree", "malformed_session", "newer_observation"} {
		t.Run(failure, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			raw, source, _ := recoveryCrossDaySession()
			session, _ := parseABSListeningSession(raw, "abs1")
			startedAt := time.Now().Add(-time.Minute)
			if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{source: 7200}, absListeningSessionSnapshot{
				Sessions: []absListeningSession{session}, StartedAt: startedAt, Complete: true,
			}); err != nil {
				t.Fatal(err)
			}
			snapshot := absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true}
			days := map[string]float64{}
			switch failure {
			case "partial_history":
				snapshot.Complete = false
			case "stats_disagree":
				days[source] = 3600
			case "malformed_session":
				snapshot.Sessions = []absListeningSession{{SessionKey: "invalid"}}
			case "newer_observation":
				if err := storage.DB.Model(&model.ABSCrossDaySessionAdjustment{}).Where("session_key = ?", "cross").Update("observed_at", snapshot.StartedAt.Add(time.Second)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", days, snapshot); err != nil {
				t.Fatal(err)
			}
			var row model.ABSCrossDaySessionAdjustment
			if err := storage.DB.Where("session_key = ?", "cross").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusAccepted {
				t.Fatalf("unproven deletion invalidated evidence: %+v, %v", row, err)
			}
		})
	}
}

func TestLateSessionResponseCannotRestoreInvalidatedAllocation(t *testing.T) {
	setupRetreatBackfillDB(t)
	raw, source, _ := recoveryCrossDaySession()
	session, _ := parseABSListeningSession(raw, "abs1")
	oldStart := time.Now().Add(-time.Minute)
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{source: 7200}, absListeningSessionSnapshot{
		Sessions: []absListeningSession{session}, StartedAt: oldStart, Complete: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := persistAndLoadABSCrossDaySessions(1, "abs1", map[string]float64{}, absListeningSessionSnapshot{StartedAt: time.Now(), Complete: true}); err != nil {
		t.Fatal(err)
	}
	// Even a later ABS updatedAt in a response requested before deletion must
	// not resurrect evidence that a newer complete observation invalidated.
	session.UpdatedAt = session.UpdatedAt.Add(time.Minute)
	observations, _ := buildABSCrossDaySessionObservations(1, "abs1", []absListeningSession{session}, oldStart.Add(time.Second))
	write := func() {
		t.Helper()
		if err := storage.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockCurrentListeningBindingInTx(tx, 1, "abs1"); err != nil {
				return err
			}
			return upsertABSCrossDaySessionObservations(tx, observations)
		}); err != nil {
			t.Fatal(err)
		}
	}
	write()
	var row model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("session_key = ?", "cross").First(&row).Error; err != nil || row.Status != crossDayAdjustmentStatusInvalidated {
		t.Fatalf("late response restored obsolete allocation: %+v, %v", row, err)
	}
	// A genuinely newer response may confirm that a session has reappeared.
	observations[0].ObservedAt = time.Now()
	write()
	if err := storage.DB.First(&row, row.ID).Error; err != nil || row.Status != crossDayAdjustmentStatusAccepted {
		t.Fatalf("new evidence could not restore a returned session: %+v, %v", row, err)
	}
}

func TestSessionCorrectedToSameDayClearsCrossDayAllocation(t *testing.T) {
	setupRetreatBackfillDB(t)
	session, source, today := recoveryCrossDaySession()
	var corrected atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			days := map[string]float64{source: 7200}
			if corrected.Load() {
				days = map[string]float64{today: 7200}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
			return
		}
		current := session
		if corrected.Load() {
			end, _ := time.Parse(time.RFC3339Nano, session["updatedAt"].(string))
			current = bossSession("cross", end, end.Add(2*time.Hour), 7200)
			current["date"] = today
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{current}})
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	corrected.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("same-day correction was rejected")
	}
	if got := recoveryListeningDay(t, source).RawSeconds; got != 0 {
		t.Fatalf("old source allocation survived: %g", got)
	}
	if got := recoveryListeningDay(t, today).RawSeconds; got != 7200 {
		t.Fatalf("corrected today=%g, want 7200", got)
	}
}

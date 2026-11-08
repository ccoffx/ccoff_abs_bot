package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyListeningCreditCollectsSessionDatesBeforeAllocating(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -5)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	source, middle, last := SectDayKey(start), SectDayKey(start.AddDate(0, 0, 1)), SectDayKey(start.AddDate(0, 0, 2))
	var reverse atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{source: 28 * 3600}})
			return
		}
		// The overstated session has no persisted cross-day allocation. Its
		// middle date is introduced by the later-starting same-day session.
		long := bossSession("long", start.Add(23*time.Hour), start.Add(49*time.Hour), 27*3600)
		short := bossSession("short", start.Add(25*time.Hour), start.Add(26*time.Hour), 3600)
		long["date"], short["date"] = source, source
		sessions := []any{long, short}
		if reverse.Load() {
			sessions = []any{short, long}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	for attempt := 0; attempt < 3; attempt++ {
		reverse.Store(attempt%2 == 1)
		days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || days[source] != 3600 || days[middle] != 86400 || days[last] != 3600 {
			t.Fatalf("refresh %d changed calendar allocation: %v ok=%t", attempt, days, ok)
		}
		stat := recoveryListeningDay(t, middle)
		if stat.CappedSeconds != 86400 || stat.EffectiveHours != calculateSectEffectiveHoursFromSeconds(86400) {
			t.Fatalf("refresh %d persisted incomplete middle-day credit: %+v", attempt, stat)
		}
	}
}

func TestDailyListeningCreditDeduplicatesAndLimitsElapsedTime(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-08T12:00:00+08:00")
	midnight := now.Add(-12 * time.Hour)
	session := func(id string, a, b, listen float64) absListeningSession {
		return absListeningSession{SessionKey: id, DateKey: SectDayKey(midnight), StartedAt: midnight.Add(time.Duration(a * float64(time.Hour))), UpdatedAt: midnight.Add(time.Duration(b * float64(time.Hour))), ListeningSeconds: listen * 3600, HasListeningTime: true}
	}
	for _, tc := range []struct {
		name      string
		raw, want float64
		sessions  []absListeningSession
		reason    string
	}{
		{"different IDs same twelve hours", 24, 12, []absListeningSession{session("a", 0, 12, 12), session("b", 0, 12, 12)}, "overlapping_sessions"},
		{"partial overlap", 16, 12, []absListeningSession{session("a", 0, 8, 8), session("b", 4, 12, 8)}, "overlapping_sessions"},
		{"future thirteen-hour session", 13, 12, []absListeningSession{session("a", 0, 13, 13)}, "future_session_time"},
		{"thirteen hours in twelve-hour span", 13, 12, []absListeningSession{session("a", 0, 12, 13)}, "listening_exceeds_wall"},
		{"official-only elapsed limit", 24, 12, nil, "daily_elapsed_limit"},
		{"paused session keeps listening budget", 6, 6, []absListeningSession{session("a", 0, 12, 6)}, ""},
		{"duplicate paused sessions", 12, 6, []absListeningSession{session("a", 0, 12, 6), session("b", 0, 12, 6)}, "overlapping_sessions"},
		{"independent playback", 8, 8, []absListeningSession{session("a", 0, 4, 4), session("b", 8, 12, 4)}, ""},
		{"missing start", 4, 0, []absListeningSession{{SessionKey: "a", DateKey: SectDayKey(now), UpdatedAt: now, ListeningSeconds: 4 * 3600, HasListeningTime: true}}, "invalid_session_time"},
		{"unseen history retains official budget", 10, 10, []absListeningSession{session("a", 8, 12, 4)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			days := map[string]float64{SectDayKey(now): tc.raw * 3600}
			before, _ := json.Marshal(days)
			got := validateDailyListeningCredit(days, days, tc.sessions, now)
			day := SectDayKey(now)
			if math.Abs(got.Seconds[day]-tc.want*3600) > 1e-6 {
				t.Fatalf("credited=%v want=%v; details=%+v", got.Seconds, tc.want*3600, got.Details)
			}
			if tc.reason != "" && got.Details[day].Anomalies[tc.reason] == 0 {
				t.Fatalf("missing diagnostic %q: %+v", tc.reason, got.Details)
			}
			after, _ := json.Marshal(days)
			if string(before) != string(after) {
				t.Fatalf("validation changed official statistics: before=%s after=%s", before, after)
			}
		})
	}
}

func TestDailyListeningValidationBoundedCrossDayRefresh(t *testing.T) {
	setupRetreatBackfillDB(t)
	dayStart := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -1)
	dayStart = time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 0, 0, 0, 0, dayStart.Location())
	start := dayStart.Add(-4 * time.Hour)
	previous, target := SectDayKey(start), SectDayKey(dayStart)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{previous: 6 * 3600}})
			return
		}
		s := bossSession("long-pauses", start, start.Add(12*time.Hour), 6*3600)
		s["date"] = previous
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{s}})
	})
	for i := 0; i < 2; i++ {
		days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || days[previous] != 0 || days[target] != 2*3600 {
			t.Fatalf("bounded refresh %d: days=%v ok=%t", i, days, ok)
		}
		previousStat, targetStat := recoveryListeningDay(t, previous), recoveryListeningDay(t, target)
		if previousStat.OfficialRawSeconds != 6*3600 || previousStat.RawSeconds != 4*3600 || targetStat.RawSeconds != 2*3600 || targetStat.OfficialRawSeconds != 0 {
			t.Fatalf("bounded allocation lost: previous=%+v target=%+v", previousStat, targetStat)
		}
	}
}

func TestDailyListeningValidationBackfillsSameDayOverlapAndResumes(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -3)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day, recentDay := SectDayKey(start), SectDayKey(start.AddDate(0, 0, 1))
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: 24 * 3600, recentDay: 500 * 60}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		sessions := []any{}
		if page < 5 {
			for i := 0; i < dailyListeningSessionPageSize; i++ {
				a := start.AddDate(0, 0, 1).Add(time.Duration(page*100+i) * time.Minute)
				s := bossSession(fmt.Sprintf("recent-%d-%d", page, i), a, a.Add(time.Minute), 60)
				s["date"] = recentDay
				sessions = append(sessions, s)
			}
		} else if page == 5 {
			for _, id := range []string{"old-first", "old-second"} {
				s := bossSession(id, start, start.Add(12*time.Hour), 12*3600)
				s["date"] = day
				sessions = append(sessions, s)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	// The completed pre-upgrade cursor must be enrolled once for ordinary
	// sessions, then retain its page across subsequent seeding/restarts.
	cursor := model.ABSCrossDaySessionScanCursor{UserID: 1, AbsUserID: "abs1", NextPage: 8, Completed: true}
	if err := storage.DB.Create(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	if err := seedABSCrossDaySessionScanCursors(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&cursor, cursor.ID).Error; err != nil || cursor.Completed || cursor.NextPage != 0 || cursor.DailyValidationVersion != dailyListeningValidationVersion {
		t.Fatalf("old cursor not reset: %+v err=%v", cursor, err)
	}
	if days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok || days[day] != 24*3600 {
		t.Fatalf("unobserved history not preserved: %v %t", days, ok)
	}
	for batch := 0; batch < 2; batch++ {
		if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := seedABSCrossDaySessionScanCursors(); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.First(&cursor, cursor.ID).Error; err != nil || !cursor.Completed || cursor.NextPage != 6 {
		t.Fatalf("cursor failed to resume: %+v err=%v", cursor, err)
	}
	for i := 0; i < 2; i++ {
		days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || days[day] != 12*3600 {
			t.Fatalf("persisted old overlap not deducted: days=%v ok=%t", days, ok)
		}
	}
	stat := recoveryListeningDay(t, day)
	if stat.OfficialRawSeconds != 24*3600 || stat.RawSeconds != 24*3600 || stat.CappedSeconds != 12*3600 {
		t.Fatalf("history totals corrupted: %+v", stat)
	}
}

func TestDailyListeningValidationReportAndAtomicFailure(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now().In(storage.DailyOperationsLocation)
	day := SectDayKey(now)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// Relative to today's elapsed time, so this also runs shortly after midnight.
	span := now.Sub(start) / 2
	var fail atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 86400, "days": map[string]float64{day: 24 * 3600}})
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			if fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			sessions := []any{}
			for _, id := range []string{"a", "b"} {
				s := bossSession(id, start, start.Add(span), 12*3600)
				s["date"] = day
				sessions = append(sessions, s)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
		default:
			fmt.Fprint(w, `{"mediaProgress":[]}`)
		}
	})
	report := GetPersonalReport(abs.DefaultClient, "abs1")
	stat := recoveryListeningDay(t, day)
	for _, want := range []string{"今日实际听书: 24.00 小时", fmt.Sprintf("今日有效计入听书: %.2f 小时", span.Hours()), "已去除重叠听书"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q: %s", want, report)
		}
	}
	detail := formatDailyListeningStat("test", stat, stat.EffectiveHours)
	if !strings.Contains(detail, "ABS 原始归属统计：`24.00` 小时") || !strings.Contains(detail, "有效计入听书") {
		t.Fatalf("detail does not distinguish raw and eligible: %s", detail)
	}
	cache, ok := cachedDailyListeningDaysWithinTTL(1, "abs1", time.Now())
	if !ok || cache[day] != stat.CappedSeconds {
		t.Fatalf("cache bypassed validation: %v %t", cache, ok)
	}
	if err := storage.DB.Exec("CREATE TRIGGER reject_daily_validation BEFORE UPDATE ON daily_listening_stats BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("injected daily write failure accepted")
	}
	after := recoveryListeningDay(t, day)
	if after.OfficialRawSeconds != stat.OfficialRawSeconds || after.RawSeconds != stat.RawSeconds || after.CappedSeconds != stat.CappedSeconds || after.ValidationJSON != stat.ValidationJSON || !after.LastFetchedAt.Equal(stat.LastFetchedAt) {
		t.Fatalf("failed transaction changed raw, credit or audit: before=%+v after=%+v", stat, after)
	}
	fail.Store(true)
	report = GetPersonalReport(abs.DefaultClient, "abs1")
	if !strings.Contains(report, "使用上次成功统计") || !strings.Contains(report, fmt.Sprintf("今日有效计入听书: %.2f 小时", stat.CappedSeconds/3600)) || !strings.Contains(report, "已去除重叠听书") {
		t.Fatalf("cached report lost validated credit: %s", report)
	}
}

func TestDailyListeningCreditSplitsCrossDayBeforeDeduplication(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-08T12:00:00+08:00")
	start := now.Add(-16 * time.Hour)
	yesterday, today := SectDayKey(start), SectDayKey(now)
	for _, tc := range []struct {
		name              string
		count             int
		listen            float64
		wantPrev, wantNow float64
	}{
		{"continuous cross-day", 1, 12, 4, 8},
		{"duplicate cross-day", 2, 12, 4, 8},
		{"small pauses preserve estimated allocation", 1, 11, 11.0 / 3, 22.0 / 3},
		{"unknown long pauses use day lower bounds", 1, 6, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := []absListeningSession{}
			for i := 0; i < tc.count; i++ {
				s := absListeningSession{SessionKey: fmt.Sprint(i), DateKey: yesterday, StartedAt: start, UpdatedAt: now.Add(-4 * time.Hour), ListeningSeconds: tc.listen * 3600, HasListeningTime: true}
				s.CrossDayStatus, _, _, _ = classifyABSCrossDaySession(s)
				if s.CrossDayStatus == crossDayAdjustmentStatusBounded {
					if err := json.Unmarshal([]byte(crossDaySessionMinimumAllocationJSON(s)), &s.StoredAllocation); err != nil {
						t.Fatal(err)
					}
				}
				sessions = append(sessions, s)
			}
			official := map[string]float64{yesterday: float64(tc.count) * tc.listen * 3600}
			adjusted, _ := rebalanceABSDaysForCrossDaySessions(official, sessions)
			got := validateDailyListeningCredit(official, adjusted, sessions, now)
			if math.Abs(got.Seconds[yesterday]-tc.wantPrev*3600) > 1e-6 || math.Abs(got.Seconds[today]-tc.wantNow*3600) > 1e-6 {
				t.Fatalf("credited=%v want previous=%g today=%g", got.Seconds, tc.wantPrev*3600, tc.wantNow*3600)
			}
			if tc.count == 1 && tc.listen == 12 && (len(got.Details[yesterday].Anomalies) != 0 || len(got.Details[today].Anomalies) != 0) {
				t.Fatalf("normal cross-day session flagged: %+v", got.Details)
			}
		})
	}
}

func TestDailyListeningValidationPersistsRawAndSurvivesRetry(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now().In(storage.DailyOperationsLocation)
	start := now.AddDate(0, 0, -1)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	day := SectDayKey(start)
	var fail atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{day: 24 * 3600}})
			return
		}
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		sessions := []any{}
		for _, id := range []string{"first", "second"} {
			s := bossSession(id, start, start.Add(12*time.Hour), 12*3600)
			s["date"] = day
			sessions = append(sessions, s)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	for i := 0; i < 2; i++ {
		eligible, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || eligible[day] != 12*3600 {
			t.Fatalf("repeat refresh=%v ok=%t", eligible, ok)
		}
	}
	stat := recoveryListeningDay(t, day)
	if stat.OfficialRawSeconds != 24*3600 || stat.RawSeconds != 24*3600 || stat.CappedSeconds != 12*3600 || math.Abs(stat.EffectiveHours-calculateSectEffectiveHoursFromSeconds(12*3600)) > 1e-9 {
		t.Fatalf("raw or credited statistics lost: %+v", stat)
	}
	var detail dailyListeningValidationDetail
	if err := json.Unmarshal([]byte(stat.ValidationJSON), &detail); err != nil || detail.Anomalies["overlapping_sessions"] == 0 || detail.OverlapSeconds != 12*3600 {
		t.Fatalf("diagnostics not persisted: %+v err=%v", detail, err)
	}
	fail.Store(true)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("failed session request accepted")
	}
	after := recoveryListeningDay(t, day)
	if stat.CappedSeconds != after.CappedSeconds || !stat.LastFetchedAt.Equal(after.LastFetchedAt) || stat.ValidationJSON != after.ValidationJSON {
		t.Fatal("failed refresh changed verified cache")
	}
	var penalties int64
	if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&penalties).Error; err != nil || penalties != 0 {
		t.Fatalf("reward validation created penalties: %d err=%v", penalties, err)
	}
}

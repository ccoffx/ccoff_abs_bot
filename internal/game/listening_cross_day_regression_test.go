package game

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestManualListeningRefreshBypassesFreshCache(t *testing.T) {
	setupRetreatBackfillDB(t)
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.SectMember{}, &model.Sect{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec("CREATE UNIQUE INDEX ux_refresh_cultivation ON cultivations(user_id)").Error; err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	today := SectDayKey(time.Now())
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := requests.Add(1) * 60
			fmt.Fprintf(w, `{"days":{%q:%d}}`, today, seconds)
			return
		}
		fmt.Fprint(w, `{"sessions":[]}`)
	})
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial refresh failed")
	}
	if days, ok := RefreshDailyListeningStatsFromABS(1, "abs1"); !ok || days[today] != 60 || requests.Load() != 1 {
		t.Fatalf("automatic view did not reuse cache: days=%v calls=%d", days, requests.Load())
	}
	stat, _, ok, err := refreshUserDailyListeningAndCultivation(1)
	if err != nil || !ok || stat.RawSeconds != 120 || requests.Load() != 2 {
		t.Fatalf("manual self refresh used cache: stat=%+v calls=%d err=%v", stat, requests.Load(), err)
	}
	success, total, skipped, err := RefreshAllDailyListeningStatsWithOptions(true)
	if err != nil || success != 1 || total != 1 || skipped != 0 || requests.Load() != 3 {
		t.Fatalf("forced all refresh: success=%d total=%d skipped=%d calls=%d err=%v", success, total, skipped, requests.Load(), err)
	}
	if err := storage.DB.Create(&model.SectMember{SectID: 1, UserID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if n := refreshSectMembersDailyListeningStatsWithOptions(1, 20, true); n != 1 || requests.Load() != 4 {
		t.Fatalf("forced sect refresh: success=%d calls=%d", n, requests.Load())
	}
}

func TestLateCrossDayReportCorrectsBothDaysAndPreservesOnFailure(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now().In(leaderboardLocation)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, leaderboardLocation)
	start := midnight.AddDate(0, 0, -1).Add(16*time.Hour + 34*time.Minute + 2*time.Second + 95*time.Millisecond)
	end := midnight.Add(9*time.Hour + 36*time.Minute + 23*time.Second + 678*time.Millisecond)
	yesterday, today := SectDayKey(start), SectDayKey(end)
	var phase atomic.Int32
	var trace bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&trace)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		mode := phase.Load()
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			seconds := 61338.942 + 2328.233
			if mode == 0 || mode == 3 {
				seconds = 10666.337
			}
			fmt.Fprintf(w, `{"days":{%q:%.9f}}`, yesterday, seconds)
			return
		}
		if mode == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sessions := []any{}
		if mode != 0 {
			session := bossSession("late-cross-day", start, end, 61338.942)
			session["date"] = yesterday
			sessions = append(sessions, session)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	readDay := func(day string) model.DailyListeningStat {
		t.Helper()
		var stat model.DailyListeningStat
		if err := storage.DB.Where("user_id = ? AND day_key = ?", 1, day).First(&stat).Error; err != nil {
			t.Fatal(err)
		}
		return stat
	}
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("initial snapshot failed")
	}
	if stat := readDay(yesterday); math.Abs(stat.EffectiveHours-2.962871388888889) > 1e-9 {
		t.Fatalf("unexpected initial value: %+v", stat)
	}
	if stat := readDay(today); stat.RawSeconds != 0 {
		t.Fatalf("invented listening before ABS report: %+v", stat)
	}
	phase.Store(1)
	for i := 0; i < 2; i++ {
		if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
			t.Fatal("late report failed")
		}
		previous, current := readDay(yesterday), readDay(today)
		if math.Abs(previous.RawSeconds-29084.985965382642) > 1e-6 || math.Abs(current.RawSeconds-34582.189034617564) > 1e-6 ||
			math.Abs(previous.EffectiveHours-6.418094347008422) > 1e-9 || previous.Source != dailyListeningCrossDaySource {
			t.Fatalf("incorrect or non-idempotent allocation: yesterday=%+v today=%+v", previous, current)
		}
	}
	before := readDay(yesterday)
	for _, mode := range []int32{2, 3} {
		phase.Store(mode)
		if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
			t.Fatalf("incomplete snapshot %d accepted", mode)
		}
		after := readDay(yesterday)
		if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) || after.Source != before.Source {
			t.Fatalf("failed correction overwrote valid data: before=%+v after=%+v", before, after)
		}
	}
	for _, field := range []string{`"force":true`, `"stats_duration_ms":`, `"updated_at":`, `"listening_seconds":61338.942`,
		`"official_seconds":`, `"adjusted_seconds":`, `"previous_raw_seconds":`, `"previous_effective_hours":`,
		`"correction_ready":false`, "ABS_CROSS_DAY_SNAPSHOT_MISMATCH"} {
		if !strings.Contains(trace.String(), field) {
			t.Errorf("missing diagnostic field %s", field)
		}
	}
	if strings.Contains(trace.String(), "Authorization") || strings.Contains(trace.String(), "Bearer") {
		t.Fatal("diagnostics leaked authentication")
	}
}

func TestOlderDailySnapshotCannotUndoNewerAcrossTimezones(t *testing.T) {
	setupRetreatBackfillDB(t)
	newer := time.Now().UTC().Add(-time.Minute)
	older := newer.Add(-time.Hour).In(leaderboardLocation)
	day := SectDayKey(older.AddDate(0, 0, -1))
	if err := recordPreparedDailyListeningStats(1, "abs1", map[string]float64{day: 7200}, nil, newer); err != nil {
		t.Fatal(err)
	}
	if err := recordPreparedDailyListeningStats(1, "abs1", map[string]float64{day: 3600}, nil, older); err == nil {
		t.Fatal("stale daily snapshot accepted")
	}
	var stat model.DailyListeningStat
	if err := storage.DB.Where("user_id = ? AND day_key = ?", 1, day).First(&stat).Error; err != nil || stat.RawSeconds != 7200 || !stat.LastFetchedAt.Equal(newer) {
		t.Fatalf("newer snapshot overwritten: %+v err=%v", stat, err)
	}
	// A genuine later ABS correction may reduce totals. Do not use max(seconds).
	if err := recordPreparedDailyListeningStats(1, "abs1", map[string]float64{day: 1800}, nil, newer.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&stat, stat.ID).Error; err != nil || stat.RawSeconds != 1800 {
		t.Fatalf("legitimate downward correction blocked: %+v err=%v", stat, err)
	}
}

func TestOlderSessionObservationCannotUndoNewerAcrossTimezones(t *testing.T) {
	setupRetreatBackfillDB(t)
	end, _ := time.Parse(time.RFC3339, "2026-11-06T09:00:00Z")
	observation := model.ABSCrossDaySessionAdjustment{
		UserID: 1, AbsUserID: "abs1", SessionKey: "session", SessionUpdatedAt: end,
		ObservedAt: end.Add(time.Minute), ListeningSeconds: 7200,
	}
	write := func(row model.ABSCrossDaySessionAdjustment) {
		t.Helper()
		if err := storage.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockCurrentListeningBindingInTx(tx, 1, "abs1"); err != nil {
				return err
			}
			return upsertABSCrossDaySessionObservations(tx, []model.ABSCrossDaySessionAdjustment{row})
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(observation)
	older := observation
	older.SessionUpdatedAt = end.Add(-time.Hour).In(leaderboardLocation)
	older.ListeningSeconds = 3600
	write(older)
	older.SessionUpdatedAt = end.In(leaderboardLocation)
	older.ObservedAt = observation.ObservedAt.Add(-time.Second).In(leaderboardLocation)
	write(older)
	var stored model.ABSCrossDaySessionAdjustment
	if err := storage.DB.First(&stored).Error; err != nil || stored.ListeningSeconds != 7200 || !stored.SessionUpdatedAt.Equal(end) {
		t.Fatalf("newer session overwritten: %+v err=%v", stored, err)
	}
	older.ObservedAt = observation.ObservedAt.Add(time.Second).In(leaderboardLocation)
	write(older)
	if err := storage.DB.First(&stored, stored.ID).Error; err != nil || stored.ListeningSeconds != 3600 {
		t.Fatalf("newer observation rejected: %+v err=%v", stored, err)
	}
}

func TestListeningRequestCrossingMidnightKeepsNewDay(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-11-05T23:59:59+08:00")
	end := start.Add(2 * time.Second)
	records := buildOfficialDailyListeningRecords(1, "abs1", map[string]float64{"2026-11-05": 7200, "2026-11-06": 1}, start, end)
	if len(records) != 2 {
		t.Fatalf("midnight request dropped day: %+v", records)
	}
	for _, record := range records {
		if !record.LastFetchedAt.Equal(start) || (record.DayKey == "2026-11-06" && record.RawSeconds != 1) {
			t.Fatalf("midnight request lost ordering or seconds: %+v", record)
		}
	}
}

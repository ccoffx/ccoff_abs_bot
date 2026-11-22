package game

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

// Exercise migration, rewards, repair and rebinding within an open day using
// historical fixtures. Archive/rollover tests use an explicit clock separately.
func recordOpenOfficialTestDay(userID int64, absID string, days map[string]float64, fetchedAt time.Time) (map[string]float64, error) {
	realNow := time.Now()
	latest := ""
	for day := range days {
		if day > latest {
			latest = day
		}
	}
	logicalNow := realNow
	if latest != "" && latest <= SectDayKey(realNow) {
		date, err := time.ParseInLocation("2006-01-02", latest, storage.DailyOperationsLocation)
		if err == nil {
			logicalNow = date.Add(23*time.Hour + time.Duration(realNow.Minute())*time.Minute + time.Duration(realNow.Second())*time.Second + time.Duration(realNow.Nanosecond()))
		}
	}
	var previous model.ListeningReportSnapshot
	if storage.DB.First(&previous, "user_id = ?", userID).Error == nil && previous.StatsFetchedAt.After(logicalNow) {
		logicalNow = previous.StatsFetchedAt.Add(time.Millisecond)
	}
	return recordOfficialDailyListeningStatsAt(userID, absID, days, fetchedAt.Add(logicalNow.Sub(realNow)), logicalNow)
}

func officialStat(t *testing.T, day string) model.DailyListeningStat {
	t.Helper()
	var s model.DailyListeningStat
	contributionMust(t, storage.DB.Where("user_id = 1 AND day_key = ?", day).First(&s).Error)
	return s
}
func officialTotal(t *testing.T) float64 {
	t.Helper()
	var c model.Cultivation
	contributionMust(t, storage.DB.Where("user_id = 1").First(&c).Error)
	return c.TotalAudioTime
}
func writeOfficial(t *testing.T, days map[string]float64) {
	t.Helper()
	_, err := recordOpenOfficialTestDay(1, "abs1", days, time.Now())
	contributionMust(t, err)
}
func TestOfficialDailyRefreshDoesNotRequestSessions(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	var stats, sessions atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			stats.Add(1)
			fmt.Fprintf(w, `{"days":{%q:26590}}`, day)
			return
		}
		sessions.Add(1)
		w.WriteHeader(503)
	})
	for i := 0; i < 3; i++ {
		days, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
		if !ok || days[day] != 26590 {
			t.Fatalf("refresh=%v %v", days, ok)
		}
	}
	if sessions.Load() != 0 || stats.Load() != 3 {
		t.Fatalf("stats=%d sessions=%d", stats.Load(), sessions.Load())
	}
	if got, want := officialTotal(t), calculateSectEffectiveHoursFromSeconds(26590); math.Abs(got-want) > 1e-9 {
		t.Fatalf("%g != %g", got, want)
	}
}
func TestOfficialDailyRetainsDecreasesAndMissingHistoryAndSupplementsGrowth(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -3))
	today := SectDayKey(time.Now())
	writeOfficial(t, map[string]float64{day: 5 * 3600, today: 60})
	before := officialTotal(t)
	writeOfficial(t, map[string]float64{day: 4 * 3600, today: 30})
	s := officialStat(t, day)
	if s.CappedSeconds != 5*3600 || s.OfficialRawSeconds != 5*3600 || !s.Archived || officialTotal(t) != before {
		t.Fatalf("decrease changed credit: %+v", s)
	}
	writeOfficial(t, map[string]float64{})
	if officialTotal(t) != before {
		t.Fatal("missing day erased history")
	}
	writeOfficial(t, map[string]float64{day: 8 * 3600, today: 120})
	want := calculateSectEffectiveHoursFromSeconds(5*3600) + 120.0/3600
	if math.Abs(officialTotal(t)-want) > 1e-9 {
		t.Fatalf("tier delta=%g want=%g", officialTotal(t), want)
	}
}
func TestOfficialDailyMigrationPreservesBalanceAndRepairIsIdempotent(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	old := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, RawSeconds: 26590, OfficialRawSeconds: 26590, CappedSeconds: 9012.6, EffectiveHours: 2.5035, ValidationJSON: `{"version":2}`}
	contributionMust(t, storage.DB.Create(&old).Error)
	assertListeningSync(t, time.Now(), 2.5035)
	writeOfficial(t, map[string]float64{day: 26590})
	if math.Abs(officialTotal(t)-2.5035) > 1e-9 {
		t.Fatal("migration paid historical difference")
	}
	s := officialStat(t, day)
	if !s.OfficialPolicy || s.OfficialBaselineSeconds != 26590 || s.OfficialBaselineHours != 2.5035 {
		t.Fatal(s)
	}
	preview, err := RepairOfficialListeningDay(1, day, false, 0, 0, "")
	contributionMust(t, err)
	if preview.BaseDeltaHours <= 3.5 || officialTotal(t) != 2.5035 {
		t.Fatalf("preview=%+v", preview)
	}
	_, err = RepairOfficialListeningDay(1, day, true, preview.BaseDeltaHours+1, 99, "reviewed correction")
	if err == nil {
		t.Fatal("accepted stale preview")
	}
	_, err = RepairOfficialListeningDay(1, day, true, preview.BaseDeltaHours, 99, "reviewed ABS daily baseline")
	contributionMust(t, err)
	want := calculateSectEffectiveHoursFromSeconds(26590)
	if math.Abs(officialTotal(t)-want) > 1e-9 {
		t.Fatal(officialTotal(t), want)
	}
	_, err = RepairOfficialListeningDay(1, day, true, preview.BaseDeltaHours, 99, "retry")
	if err == nil {
		t.Fatal("duplicate repair accepted")
	}
	writeOfficial(t, map[string]float64{day: 26690})
	if math.Abs(officialTotal(t)-calculateSectEffectiveHoursFromSeconds(26690)) > 1e-9 {
		t.Fatal("growth after repair incorrect")
	}
	var count int64
	contributionMust(t, storage.DB.Model(&model.AuditLog{}).Where("action = ?", "REPAIR_OFFICIAL_LISTENING_DAY").Count(&count).Error)
	if count != 1 {
		t.Fatal("audit count", count)
	}
}
func TestOfficialDailySourceAndSettlementRollbackTogether(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 3600})
	before := officialStat(t, day)
	total := officialTotal(t)
	for _, table := range []string{"cultivations", "listening_report_snapshots"} {
		contributionMust(t, storage.DB.Exec("CREATE TRIGGER reject_official_update BEFORE UPDATE ON "+table+" BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error)
		_, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{day: 7200}, time.Now())
		if err == nil {
			t.Fatal("expected rollback", table)
		}
		after := officialStat(t, day)
		if after.CappedSeconds != before.CappedSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) || officialTotal(t) != total {
			t.Fatal("partial commit", table)
		}
		contributionMust(t, storage.DB.Exec("DROP TRIGGER reject_official_update").Error)
	}
	writeOfficial(t, map[string]float64{day: 7200})
	if officialTotal(t) != 2 {
		t.Fatal("retry failed")
	}
}
func TestOfficialDailyConcurrentAndStaleResponses(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	older := time.Now().Add(-time.Minute)
	writeOfficial(t, map[string]float64{day: 3600})
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{day: 7200}, time.Now())
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil && !strings.Contains(err.Error(), "STALE_SNAPSHOT") {
			t.Fatal(err)
		}
	}
	if officialTotal(t) != 2 {
		t.Fatal("duplicate credit", officialTotal(t))
	}
	if _, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{day: 10800}, older); err == nil {
		t.Fatal("accepted stale larger response")
	}
	if officialStat(t, day).OfficialRawSeconds != 7200 {
		t.Fatal("stale overwrote raw")
	}
}
func TestOfficialDailyCalendarLimitsAndResumeMissingDays(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-18T01:00:00+08:00")
	records := buildOfficialDailyListeningRecords(1, "abs1", map[string]float64{"2026-11-17": 36000, "2026-11-18": 36000, "2026-11-19": 3600}, now.Add(-2*time.Hour), now)
	for _, r := range records {
		want := 36000.0
		if r.DayKey == "2026-11-18" {
			want = 3600
		}
		if r.CappedSeconds != want {
			t.Fatal(r)
		}
		if r.DayKey == "2026-11-19" {
			t.Fatal("future credit")
		}
	}
	setupRetreatBackfillDB(t)
	days := map[string]float64{}
	for i := 1; i <= 4; i++ {
		days[SectDayKey(time.Now().AddDate(0, 0, -i))] = 3600
	}
	writeOfficial(t, days)
	writeOfficial(t, days)
	if officialTotal(t) != 4 {
		t.Fatal("offline days not paid exactly once", officialTotal(t))
	}
}
func TestOfficialDailyInvalidValuesAndRebinding(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now())
	for _, seconds := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{day: seconds}, time.Now()); err == nil {
			t.Fatal("accepted invalid seconds")
		}
	}
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "replacement").Error)
	if _, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{day: 60}, time.Now()); err == nil {
		t.Fatal("accepted old binding")
	}
	var count int64
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("obsolete binding wrote data")
	}
}

func TestOfficialDailyMigrationDoesNotImportUnseenHistoricalSourceTwice(t *testing.T) {
	setupRetreatBackfillDB(t)
	yesterday := SectDayKey(time.Now().AddDate(0, 0, -1))
	source := SectDayKey(time.Now().AddDate(0, 0, -2))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: yesterday, CappedSeconds: 3600, EffectiveHours: 1}).Error)
	assertListeningSync(t, time.Now(), 1)
	writeOfficial(t, map[string]float64{source: 3600})
	if officialTotal(t) != 1 {
		t.Fatal("legacy allocated playback imported twice", officialTotal(t))
	}
	writeOfficial(t, map[string]float64{source: 7200})
	if officialTotal(t) != 2 {
		t.Fatal("historical growth lost", officialTotal(t))
	}
}

func TestOfficialDailyMigrationPreservesFireAndRetreatBonusBasis(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"abs_user_id": "abs1", "official_raw_seconds": 36000, "raw_seconds": 36000, "validation_json": `{"version":2}`}).Error)
	assertListeningSync(t, now, 15.84)
	before := officialTotal(t)
	writeOfficial(t, map[string]float64{stat.DayKey: 36000})
	if math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatalf("migration changed fire balance: %g -> %g", before, officialTotal(t))
	}
	s := officialStat(t, stat.DayKey)
	if s.BonusListeningSeconds() != 8*3600 {
		t.Fatal("legacy bonus basis changed", s)
	}
	preview, err := RepairOfficialListeningDay(1, stat.DayKey, false, 0, 0, "")
	contributionMust(t, err)
	_, err = RepairOfficialListeningDay(1, stat.DayKey, true, preview.BaseDeltaHours, 99, "reviewed daily repair")
	contributionMust(t, err)
	s = officialStat(t, stat.DayKey)
	if s.BonusListeningSeconds() != 36000 {
		t.Fatal("repair did not restore bonus basis")
	}
	want := calculateSectEffectiveHoursFromSeconds(36000) + (10-calculateSectEffectiveHoursFromSeconds(36000))*5.9
	if math.Abs(officialTotal(t)-want) > 1e-9 {
		t.Fatal(officialTotal(t), want)
	}
}

func TestOfficialDailyRebindingBaselinesNewAccountHistory(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	other := SectDayKey(time.Now().AddDate(0, 0, -2))
	writeOfficial(t, map[string]float64{day: 3600})
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "abs2").Error)
	_, err := recordOpenOfficialTestDay(1, "abs2", map[string]float64{day: 36000, other: 36000}, time.Now())
	contributionMust(t, err)
	if officialTotal(t) != 1 {
		t.Fatal("rebinding imported history again", officialTotal(t))
	}
	_, err = recordOpenOfficialTestDay(1, "abs2", map[string]float64{day: 39600, other: 39600}, time.Now())
	contributionMust(t, err)
	want := 1 + (calculateSectEffectiveHoursFromSeconds(39600) - calculateSectEffectiveHoursFromSeconds(36000))
	if math.Abs(officialTotal(t)-want) > 1e-9 {
		t.Fatal("growth after rebind incorrect", officialTotal(t), want)
	}
}

func TestOfficialDailyRepairBonusOnlyAtSuppressionCap(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"abs_user_id": "abs1", "official_raw_seconds": 86400, "raw_seconds": 86400, "capped_seconds": 72000, "effective_hours": 8.5, "validation_json": `{"version":2}`}).Error)
	_, ok := syncCultivationFromDailyListeningStatsAt(1, now)
	if !ok {
		t.Fatal("settlement failed")
	}
	before := officialTotal(t)
	_, migrationErr := recordOfficialDailyListeningStats(1, "abs1", map[string]float64{stat.DayKey: 86400}, time.Now())
	contributionMust(t, migrationErr)
	if math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatal("migration paid fire gap")
	}
	preview, err := RepairOfficialListeningDay(1, stat.DayKey, false, 0, 0, "")
	contributionMust(t, err)
	if preview.BaseDeltaHours != 0 || preview.TotalDeltaHours <= 0 {
		t.Fatal(preview)
	}
	_, err = RepairOfficialListeningDay(1, stat.DayKey, true, 0, 99, "reviewed fire-only deficit")
	contributionMust(t, err)
	_, err = RepairOfficialListeningDay(1, stat.DayKey, true, 0, 99, "retry")
	if err == nil {
		t.Fatal("duplicate fire repair accepted")
	}
}

func TestOfficialDailyRepairAuditFailureRollsBack(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, RawSeconds: 7200, OfficialRawSeconds: 7200, CappedSeconds: 3600, EffectiveHours: 1, ValidationJSON: `{"version":2}`}).Error)
	writeOfficial(t, map[string]float64{day: 7200})
	preview, err := RepairOfficialListeningDay(1, day, false, 0, 0, "")
	contributionMust(t, err)
	contributionMust(t, storage.DB.Exec("CREATE TRIGGER reject_repair_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END").Error)
	_, err = RepairOfficialListeningDay(1, day, true, preview.BaseDeltaHours, 99, "test rollback")
	if err == nil {
		t.Fatal("expected audit failure")
	}
	if officialTotal(t) != 1 || officialStat(t, day).OfficialBaselineHours != 1 {
		t.Fatal("repair committed without audit")
	}
}

func TestOfficialDailyNullPayloadDoesNotRefreshSuccessfulSnapshot(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 3600})
	before := officialStat(t, day)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"days":{%q:null}}`, day) })
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("null payload accepted")
	}
	after := officialStat(t, day)
	if !after.LastFetchedAt.Equal(before.LastFetchedAt) || after.CappedSeconds != 3600 || officialTotal(t) != 1 {
		t.Fatal("null overwrote successful snapshot")
	}
}

func TestOfficialDailyMigrationBaselinesHistoricalDayMissingFromFirstResponse(t *testing.T) {
	setupRetreatBackfillDB(t)
	allocated := SectDayKey(time.Now().AddDate(0, 0, -1))
	source := SectDayKey(time.Now().AddDate(0, 0, -2))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: allocated, CappedSeconds: 3600, EffectiveHours: 1}).Error)
	assertListeningSync(t, time.Now(), 1)
	// Migration succeeds without the old source day. Subsequent refreshes must
	// retain the migration boundary rather than treating it as a fresh import.
	writeOfficial(t, map[string]float64{})
	writeOfficial(t, map[string]float64{})
	writeOfficial(t, map[string]float64{source: 3600})
	if officialTotal(t) != 1 {
		t.Fatalf("late source duplicated legacy credit: got %g want 1", officialTotal(t))
	}
	s := officialStat(t, source)
	if s.OfficialBaselineSeconds != 3600 || s.EffectiveHours != 0 || s.BonusListeningSeconds() != 0 {
		t.Fatal("late source not baselined", s)
	}
	writeOfficial(t, map[string]float64{source: 7200})
	if officialTotal(t) != 1 {
		t.Fatal("archived history was reopened", officialTotal(t))
	}
}

func TestOfficialDailyMigrationBoundaryDoesNotSuppressNewPolicyDates(t *testing.T) {
	setupRetreatBackfillDB(t)
	allocated := SectDayKey(time.Now().AddDate(0, 0, -4))
	cutoff := SectDayKey(time.Now().AddDate(0, 0, -2))
	delayed := SectDayKey(time.Now().AddDate(0, 0, -1))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: allocated, CappedSeconds: 3600, EffectiveHours: 1}).Error)
	assertListeningSync(t, time.Now(), 1)
	// Seed the persisted state of a migration that committed two days ago.
	contributionMust(t, storage.DB.Model(&model.Cultivation{}).Where("user_id=1").Updates(map[string]any{"official_daily_initialized": true, "official_daily_abs_user_id": "abs1", "official_daily_baseline_before": cutoff}).Error)
	writeOfficial(t, map[string]float64{cutoff: 3600, delayed: 3600})
	if officialTotal(t) != 3 {
		t.Fatal("post-cutover dates were baselined", officialTotal(t))
	}
	var profile model.Cultivation
	contributionMust(t, storage.DB.Where("user_id=1").First(&profile).Error)
	if profile.OfficialDailyBaselineBefore != cutoff {
		t.Fatal("migration boundary advanced", profile.OfficialDailyBaselineBefore)
	}
}

func TestOfficialDailyNativeAccountAcceptsLateHistoricalDate(t *testing.T) {
	setupRetreatBackfillDB(t)
	writeOfficial(t, map[string]float64{})
	source := SectDayKey(time.Now().AddDate(0, 0, -2))
	writeOfficial(t, map[string]float64{source: 3600})
	if officialTotal(t) != 1 {
		t.Fatal("native official account lost late history", officialTotal(t))
	}
	var profile model.Cultivation
	contributionMust(t, storage.DB.Where("user_id=1").First(&profile).Error)
	if profile.OfficialDailyBaselineBefore != "" {
		t.Fatal("native account acquired legacy boundary", profile.OfficialDailyBaselineBefore)
	}
}

func TestOfficialDailyRebindingProtectsHistoricalSourceMissingFromFirstResponse(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	source := SectDayKey(time.Now().AddDate(0, 0, -2))
	writeOfficial(t, map[string]float64{day: 3600})
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "abs2").Error)
	for _, days := range []map[string]float64{{}, {source: 36000}, {source: 36000}} {
		_, err := recordOpenOfficialTestDay(1, "abs2", days, time.Now())
		contributionMust(t, err)
		if officialTotal(t) != 1 {
			t.Fatal("rebound account imported late source twice", officialTotal(t))
		}
	}
}

func TestOfficialDailyMigrationBoundaryRollsBackWithSnapshotFailure(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 3600, EffectiveHours: 1}).Error)
	assertListeningSync(t, time.Now(), 1)
	contributionMust(t, storage.DB.Exec("CREATE TRIGGER reject_cutover_snapshot BEFORE INSERT ON listening_report_snapshots BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error)
	if _, err := recordOpenOfficialTestDay(1, "abs1", map[string]float64{}, time.Now()); err == nil {
		t.Fatal("expected rollback")
	}
	var profile model.Cultivation
	contributionMust(t, storage.DB.Where("user_id=1").First(&profile).Error)
	if profile.OfficialDailyInitialized || profile.OfficialDailyBaselineBefore != "" {
		t.Fatal("failed migration committed boundary", profile)
	}
	contributionMust(t, storage.DB.Exec("DROP TRIGGER reject_cutover_snapshot").Error)
	writeOfficial(t, map[string]float64{})
	contributionMust(t, storage.DB.Where("user_id=1").First(&profile).Error)
	if profile.OfficialDailyBaselineBefore != SectDayKey(time.Now()) {
		t.Fatal("successful retry lost boundary", profile)
	}
}

func TestOfficialDailyRebindingCreditsGrowthBelowPreviousAccountHighWater(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 36000})
	before := officialTotal(t)
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "abs2").Error)
	for _, seconds := range []float64{3600, 7200, 7200, 1800} {
		_, err := recordOpenOfficialTestDay(1, "abs2", map[string]float64{day: seconds}, time.Now())
		contributionMust(t, err)
		want := before
		if seconds != 3600 {
			want++
		}
		if math.Abs(officialTotal(t)-want) > 1e-9 {
			t.Fatalf("new account seconds=%g total=%.9f want=%.9f", seconds, officialTotal(t), want)
		}
	}
	s := officialStat(t, day)
	if s.OfficialBaselineSeconds != 3600 || s.CappedSeconds != 7200 {
		t.Fatal("new account inherited old seconds", s)
	}
}

func TestOfficialDailyRebindingPreservesMigratedBonusBasis(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"abs_user_id": "abs1", "official_raw_seconds": 36000, "raw_seconds": 36000, "validation_json": `{"version":2}`}).Error)
	assertListeningSync(t, now, 15.84)
	writeOfficial(t, map[string]float64{stat.DayKey: 36000})
	before := officialTotal(t)
	contributionMust(t, db.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "abs2").Error)
	_, err := recordOpenOfficialTestDay(1, "abs2", map[string]float64{stat.DayKey: 3600}, time.Now())
	contributionMust(t, err)
	if math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatal("rebinding changed earned bonus", officialTotal(t), before)
	}
	s := officialStat(t, stat.DayKey)
	if s.CappedSeconds != 3600 || s.BonusListeningSeconds() != 28800 {
		t.Fatal("rebinding confused account time and bonus basis", s)
	}
	_, err = recordOpenOfficialTestDay(1, "abs2", map[string]float64{stat.DayKey: 7200}, time.Now())
	contributionMust(t, err)
	if math.Abs(officialTotal(t)-(before+1)) > 1e-9 {
		t.Fatal("new low-tier growth or legacy fire basis incorrect", officialTotal(t), before+1)
	}
}

func TestOfficialDailyRepeatedRebindingResetsOnlyAccountTime(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 36000})
	before := officialTotal(t)
	for _, account := range []string{"abs2", "abs3", "abs1"} {
		contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", account).Error)
		// An empty account must not inherit an earlier account's playback threshold.
		_, err := recordOpenOfficialTestDay(1, account, map[string]float64{day: 0}, time.Now())
		contributionMust(t, err)
		if math.Abs(officialTotal(t)-before) > 1e-9 {
			t.Fatal("rebind changed accumulated cultivation")
		}
		_, err = recordOpenOfficialTestDay(1, account, map[string]float64{day: 3600}, time.Now())
		contributionMust(t, err)
		before = math.Min(sectDailyCultivationHourCap, before+1)
		if math.Abs(officialTotal(t)-before) > 1e-9 {
			t.Fatal("growth lost after repeated rebind", account, officialTotal(t), before)
		}
	}
}

func TestOfficialDailyCorruptRetreatEvidenceIsolation(t *testing.T) {
	for _, evidence := range []string{`{`, `{"session":{"invalid-date":3600}}`} {
		t.Run(evidence, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			day := SectDayKey(time.Now().AddDate(0, 0, -2))
			other := SectDayKey(time.Now().AddDate(0, 0, -1))
			r := addContributionRetreat(t, storage.DB, day)
			writeOfficial(t, map[string]float64{day: 8 * 3600})
			before := officialStat(t, day)
			bonus := before.AppliedBaseHours - before.AppliedEffectiveHours
			if bonus <= 0 {
				t.Fatal("expected settled retreat bonus")
			}
			contributionMust(t, storage.DB.Model(&r).UpdateColumn("listening_evidence_json", evidence).Error)
			writeOfficial(t, map[string]float64{day: 9 * 3600, other: 3600})
			after := officialStat(t, day)
			if math.Abs(after.AppliedBaseHours-after.AppliedEffectiveHours-bonus) > 1e-9 {
				t.Fatal("lost settled retreat bonus")
			}
			if after.AppliedEffectiveHours <= before.AppliedEffectiveHours {
				t.Fatal("ordinary growth blocked")
			}
			if officialStat(t, other).AppliedEffectiveHours != 1 {
				t.Fatal("unrelated day blocked")
			}
			contributionMust(t, RefreshSpiritFireDailyStat(&after, time.Now()))
			contributionMust(t, storage.DB.Model(&r).UpdateColumn("listening_evidence_json", r.ListeningEvidenceJSON).Error)
			writeOfficial(t, map[string]float64{day: 9 * 3600, other: 3600})
			fixed := officialStat(t, day)
			want := fixed.EffectiveHours + dailySectCaveRetreatBonusHours(fixed, []model.SectCaveRetreat{r})
			if math.Abs(fixed.AppliedBaseHours-want) > 1e-9 {
				t.Fatal("repaired evidence not reconsidered")
			}
		})
	}
}

func TestOfficialDailyRebindingCannotExceedUserDayCap(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 18 * 3600})
	for _, account := range []string{"abs2", "abs3", "abs1"} {
		contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", account).Error)
		for _, seconds := range []float64{0, 4 * 3600, 18 * 3600} {
			_, err := recordOpenOfficialTestDay(1, account, map[string]float64{day: seconds}, time.Now())
			contributionMust(t, err)
			s := officialStat(t, day)
			if math.Abs(s.EffectiveHours-sectDailyCultivationHourCap) > 1e-9 || math.Abs(officialTotal(t)-sectDailyCultivationHourCap) > 1e-9 {
				t.Fatalf("account=%s seconds=%g daily=%g total=%g", account, seconds, s.EffectiveHours, officialTotal(t))
			}
		}
	}
}

func TestOfficialDailyNewRetreatUsesMigratedBonusBasis(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
	contributionMust(t, err)
	legacy := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 4 * 3600, RawSeconds: 8 * 3600, OfficialRawSeconds: 8 * 3600, EffectiveHours: 4, ValidationJSON: `{"version":2}`}
	contributionMust(t, storage.DB.Create(&legacy).Error)
	writeOfficial(t, map[string]float64{day: 8 * 3600})
	base, err := getUserTodayBonusListeningSeconds(1, "", start.Add(4*time.Hour))
	contributionMust(t, err)
	if base != 4*3600 {
		t.Fatalf("retreat baseline=%g want 14400", base)
	}
	r := model.SectCaveRetreat{UserID: 1, StartAt: start.Add(4 * time.Hour), EndAt: start.Add(5 * time.Hour), BaseRawSeconds: base, Status: "expired"}
	r.ListeningEvidenceJSON = retreatTestEvidence(r.StartAt, r.EndAt, 3600)
	contributionMust(t, storage.DB.Create(&r).Error)
	writeOfficial(t, map[string]float64{day: 9 * 3600})
	s := officialStat(t, day)
	if got := s.AppliedBaseHours - s.AppliedEffectiveHours; math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("retreat bonus=%g want .2", got)
	}
}

func TestOfficialDailyRepairRejectsRebindingHistory(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 3600})
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id=1").Update("abs_user_id", "abs2").Error)
	_, err := recordOpenOfficialTestDay(1, "abs2", map[string]float64{day: 10 * 3600}, time.Now())
	contributionMust(t, err)
	before := officialStat(t, day)
	for _, apply := range []bool{false, true} {
		_, err = RepairOfficialListeningDay(1, day, apply, calculateSectEffectiveHoursFromSeconds(10*3600)-1, 99, "regression")
		if err == nil {
			t.Fatalf("repair accepted rebind baseline apply=%v", apply)
		}
	}
	after := officialStat(t, day)
	if officialTotal(t) != 1 || after.EffectiveHours != before.EffectiveHours || after.BonusListeningSeconds() != before.BonusListeningSeconds() {
		t.Fatal("rebind history was imported")
	}
}

func TestOfficialDailyRepairRejectsUnverifiedBaselines(t *testing.T) {
	for _, kind := range []string{"", "native", "historical_excluded"} {
		t.Run("kind="+kind, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			day := SectDayKey(time.Now().AddDate(0, 0, -1))
			contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 3600, OfficialRawSeconds: 7200, EffectiveHours: 1, ValidationJSON: `{"version":2}`}).Error)
			writeOfficial(t, map[string]float64{day: 7200})
			contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id=1 AND day_key=?", day).UpdateColumn("official_baseline_kind", kind).Error)
			// Repeated refresh must not guess missing provenance from the numeric gap.
			writeOfficial(t, map[string]float64{day: 7200})
			for _, apply := range []bool{false, true} {
				_, err := RepairOfficialListeningDay(1, day, apply, 1, 99, "regression")
				if err == nil {
					t.Fatalf("accepted baseline kind %q apply=%v", kind, apply)
				}
			}
			if officialTotal(t) != 1 || officialStat(t, day).BonusListeningSeconds() != 3600 {
				t.Fatal("rejected repair changed balance")
			}
			var count int64
			contributionMust(t, storage.DB.Model(&model.AuditLog{}).Where("action=?", "REPAIR_OFFICIAL_LISTENING_DAY").Count(&count).Error)
			if count != 0 {
				t.Fatal("rejected repair wrote audit")
			}
		})
	}
}

package game

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyListeningSuppression(t *testing.T) {
	for _, tc := range []struct{ raw, want float64 }{
		{-1, 0}, {0, 0}, {2, 2}, {4, 4}, {4.5, 4.3},
		{6, 5.2}, {8, 6.4}, {8.5, 6.4 + 0.8/7}, {10, 6.4 + 3.2/7},
		{12, 6.4 + 6.4/7}, {15, 8}, {16.5, 8.25},
		{18, 8.5}, {19.5, 8.5}, {24, 8.5}, {30, 8.5},
	} {
		got := calculateSectEffectiveHoursFromSeconds(tc.raw * 3600)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("raw %g hours: got %.12f, want %g", tc.raw, got, tc.want)
		}
		// A one-second retreat must stay within one second of the normal
		// calculation, including fractional boundaries and the daily cap.
		if tc.raw > 0 {
			got = calculateSectEffectiveHoursFromSecondsWithRetreat(tc.raw*3600, 1)
			if got < tc.want-1e-9 || got > tc.want+1.0/3600 {
				t.Errorf("raw %g hours: unexpected one-second retreat result %g", tc.raw, got)
			}
		}
	}
	got := calculateEffectiveCultivationHoursFromABSDays(map[string]float64{
		"2026-10-27": 24 * 3600,
		"2026-10-28": 4 * 3600,
	})
	if math.Abs(got-12.5) > 1e-9 {
		t.Fatalf("daily thresholds must reset across days: got %g, want 12.5", got)
	}
}

func TestRetreatSuppressionBoundaries(t *testing.T) {
	for _, tc := range []struct{ raw, base, duration, wantBonus float64 }{
		{4, 2, 2, 0},           // No bonus before four hours.
		{6, 4, 2, 0.4},         // 60% -> 80%.
		{10, 8, 2, 1.0 / 7},    // 1.6/7 -> 30%.
		{9, 7, 2, 0.2 + 0.5/7}, // Cross the eight-hour boundary.
		{16, 14, 2, 0.5 / 7},   // Bonus stops at fifteen hours.
		{24, 15, 9, 0},         // No late-hour bypass through retreats.
		{30, 15, 15, 0},        // Raw listening remains capped at 24h.
		{12, 4, 2, 0.4},        // Only the retreat's own duration counts.
		{15.5, 14.5, 1, 0.25 / 7},
		{19, 17, 2, 0}, // Crossing the cap cannot generate retreat bonuses.
	} {
		got := calculateSectRetreatBonusHoursForRecord(tc.raw*3600, tc.base*3600, tc.duration*3600)
		if math.Abs(got-tc.wantBonus) > 1e-9 {
			t.Errorf("raw/base/duration %g/%g/%g: bonus %g, want %g", tc.raw, tc.base, tc.duration, got, tc.wantBonus)
		}
	}
}

func TestListeningStopsEarningAtEighteenHours(t *testing.T) {
	for _, retreatHours := range []float64{0, 2, 4, 18, 24} {
		atCap := calculateSectEffectiveHoursFromSecondsWithRetreat(18*3600, retreatHours*3600)
		if math.Abs(atCap-8.5) > 1e-9 {
			t.Errorf("retreat %gh at cap: got %g, want 8.5", retreatHours, atCap)
		}
		// Keep the retreat start fixed as playback continues past the cap.
		atEnd := calculateSectEffectiveHoursFromSecondsWithRetreat(24*3600, (retreatHours+6)*3600)
		if math.Abs(atCap-atEnd) > 1e-9 {
			t.Errorf("retreat %gh at cap: cultivation grew from %g to %g after 18h", retreatHours, atCap, atEnd)
		}
	}
}

func TestCombinedDailyRetreatCap(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "listening.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	previousDB := storage.DB
	storage.DB = db
	t.Cleanup(func() {
		storage.DB = previousDB
		_ = connection.Close()
	})
	if err := db.AutoMigrate(&model.DailyListeningStat{}, &model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	start, _ := sectDayRange(time.Now())
	now := start.Add(22 * time.Hour)
	for _, startHour := range []int{4, 8} {
		retreat := model.SectCaveRetreat{
			UserID: 1, StartAt: start.Add(time.Duration(startHour) * time.Hour),
			EndAt:          start.Add(time.Duration(startHour+4) * time.Hour),
			BaseRawSeconds: float64(startHour) * 3600, Status: sectRetreatStatusActive,
		}
		retreat.ListeningEvidenceJSON = retreatTestEvidence(retreat.StartAt, retreat.EndAt, retreat.EndAt.Sub(retreat.StartAt).Seconds())
		if err := db.Create(&retreat).Error; err != nil {
			t.Fatal(err)
		}
	}
	yesterday := model.DailyListeningStat{
		UserID: 1, DayKey: SectDayKey(start.AddDate(0, 0, -1)), EffectiveHours: 8.5,
	}
	if err := db.Create(&yesterday).Error; err != nil {
		t.Fatal(err)
	}
	stat := model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now)}
	if err := db.Create(&stat).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want float64 }{
		{8, 7.2}, {12, 8.4}, {13, 8.5}, {15, 8.5}, {17, 8.5}, {18, 8.5}, {24, 8.5},
	} {
		stat.RawSeconds = tc.raw * 3600
		stat.CappedSeconds = stat.RawSeconds
		stat.EffectiveHours = calculateSectEffectiveHoursFromSeconds(stat.RawSeconds)
		if err := db.Save(&stat).Error; err != nil {
			t.Fatal(err)
		}
		bonus := ActiveSectCaveRetreatBonusHours(1, now)
		got, ok := GetTodaySectListeningHoursFromCache(1, now)
		if !ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("raw %gh, combined daily value %g (ok=%t), want %g", tc.raw, got, ok, tc.want)
		}
		if bonus < 0 || got > 8.5+1e-9 {
			t.Errorf("raw %gh: invalid bonus %g or daily total %g", tc.raw, bonus, got)
		}
		total, ok := totalCultivationEffectiveHoursFromDailyStats(1, now)
		if !ok || math.Abs(total-(8.5+tc.want)) > 1e-9 {
			t.Errorf("raw %gh: cumulative value %g (ok=%t), want %g", tc.raw, total, ok, 8.5+tc.want)
		}
		report := formatDailyListeningStat("test", stat, total)
		if tc.want == 8.5 && !strings.Contains(report, "今日净修为：`8.50` 小时") {
			t.Errorf("report does not show combined daily cap: %s", report)
		}
	}
}

func TestRetreatBonusSurvivesMidnightAndReload(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T22:00:00+08:00")
	stat := model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now), RawSeconds: 8 * 3600, CappedSeconds: 8 * 3600, EffectiveHours: 6.4}
	if err := storage.DB.Create(&stat).Error; err != nil {
		t.Fatal(err)
	}
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: now.Add(-18 * time.Hour), EndAt: now.Add(-14 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	retreat.ListeningEvidenceJSON = retreatTestEvidence(retreat.StartAt, retreat.EndAt, 4*3600)
	if err := storage.DB.Create(&retreat).Error; err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, now.Add(3 * time.Hour), now.AddDate(0, 0, 7)} {
		for repeat := 0; repeat < 2; repeat++ {
			total, ok := totalCultivationEffectiveHoursFromDailyStats(1, at)
			if !ok || math.Abs(total-7.2) > 1e-9 {
				t.Fatalf("at %s: got %g, %t; want 7.2", at, total, ok)
			}
		}
	}
	// Historical ABS corrections recompute the per-day cap rather than freezing
	// a stale bonus or adding it twice on refresh.
	if err := storage.DB.Model(&stat).Updates(map[string]interface{}{"raw_seconds": 18 * 3600, "capped_seconds": 18 * 3600, "effective_hours": 8.5}).Error; err != nil {
		t.Fatal(err)
	}
	if total, ok := totalCultivationEffectiveHoursFromDailyStats(1, now.AddDate(0, 0, 1)); !ok || math.Abs(total-8.5) > 1e-9 {
		t.Fatalf("corrected historical total=%g, %t; want 8.5", total, ok)
	}
	// A retreat read failure must not overwrite cultivation with a lower total.
	if err := storage.DB.Migrator().DropTable(&model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := totalCultivationEffectiveHoursFromDailyStats(1, now); ok {
		t.Fatal("missing retreat records silently discarded historical bonuses")
	}
}

func TestRetreatBonusSplitsAcrossBeijingDays(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-10-28T22:00:00+08:00")
	retreats := []model.SectCaveRetreat{{StartAt: start, EndAt: start.Add(4 * time.Hour), BaseRawSeconds: 4 * 3600,
		ListeningEvidenceJSON: retreatTestEvidence(start, start.Add(4*time.Hour), 4*3600)}}
	for _, tc := range []struct {
		day  string
		want float64
	}{{"2026-10-28", 0.4}, {"2026-10-29", 0}, {"2026-10-30", 0}} {
		stat := model.DailyListeningStat{DayKey: tc.day, CappedSeconds: 8 * 3600, EffectiveHours: 6.4}
		if got := dailySectCaveRetreatBonusHours(stat, retreats); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("day %s: bonus %g, want %g", tc.day, got, tc.want)
		}
	}
}

func retreatTestEvidence(start, end time.Time, seconds float64) string {
	session := absListeningSession{SessionKey: "session", StartedAt: start, UpdatedAt: end, ListeningSeconds: seconds, HasListeningTime: true}
	days := provenSectRetreatSessionDays(model.SectCaveRetreat{StartAt: start, EndAt: end}, session)
	data, _ := json.Marshal(sectRetreatListeningEvidence{"session": days})
	return string(data)
}

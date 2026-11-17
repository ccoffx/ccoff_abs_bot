package game

import (
	"fmt"
	"math"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestCultivationHistoryUsesBoundedQueriesAndRetainsCorrections(t *testing.T) {
	db, now, original := setupListeningContributionTest(t)
	contributionMust(t, db.Delete(&original).Error)
	start := now.AddDate(0, 0, -365)
	first := SectDayKey(start)
	contributionMust(t, db.Model(&model.SpiritFireState{}).Where("id = 1").Update("launch_day", first).Error)
	contributionMust(t, db.Create(&model.SectOwnerBonusRevision{UserID: 1, DayKey: first, Enabled: true}).Error)
	last := SectDayKey(now.AddDate(0, 0, -1))
	contributionMust(t, db.Create(&model.SectOwnerBonusRevision{UserID: 1, DayKey: last, Enabled: false}).Error)
	stats := make([]model.DailyListeningStat, 365)
	snaps := make([]model.SpiritFireDaily, 365)
	for i := range stats {
		day := SectDayKey(start.AddDate(0, 0, i))
		stats[i] = model.DailyListeningStat{UserID: 1, DayKey: day, CappedSeconds: 8 * 3600, EffectiveHours: 6.4, BaseCultivationHours: 6.4, SpiritFireHours: .8, SectOwnerBonusHours: .192}
		if day == last {
			stats[i].SectOwnerBonusHours = 0
		}
		snaps[i] = model.SpiritFireDaily{UserID: 1, DayKey: day, FireIDs: "[]", Percent: 50}
	}
	contributionMust(t, db.CreateInBatches(&stats, 100).Error)
	contributionMust(t, db.CreateInBatches(&snaps, 100).Error)
	queries := 0
	contributionMust(t, db.Callback().Query().Before("gorm:query").Register("count_history_queries", func(*gorm.DB) { queries++ }))
	t.Cleanup(func() { db.Callback().Query().Remove("count_history_queries") })
	want := 365*7.2 + 364*.192
	assertListeningSync(t, now, want)
	if queries > 15 {
		t.Fatalf("365 days required %d queries; want at most 15", queries)
	}
	t.Logf("365 historical days: %d read queries", queries)
	loadedDays := int64(0)
	contributionMust(t, db.Callback().Query().After("gorm:query").Register("count_history_rows", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			loadedDays += tx.RowsAffected
		}
	}))
	t.Cleanup(func() { db.Callback().Query().Remove("count_history_rows") })
	assertListeningSync(t, now, want)
	if loadedDays != 0 {
		t.Fatalf("unchanged history reloaded %d days", loadedDays)
	}

	// A late reduction still updates base, historical fire rate and historical
	// owner entitlement, even after ownership has changed.
	contributionMust(t, db.Model(&stats[0]).Updates(map[string]any{"capped_seconds": 4 * 3600, "effective_hours": 3.2}).Error)
	assertListeningSync(t, now, want-3.696)
	if loadedDays != 1 {
		t.Fatalf("one correction loaded %d daily records", loadedDays)
	}
	var corrected model.DailyListeningStat
	contributionMust(t, storage.DB.First(&corrected, stats[0].ID).Error)
	if math.Abs(corrected.SpiritFireHours-.4) > 1e-9 || math.Abs(corrected.SectOwnerBonusHours-.096) > 1e-9 || corrected.BaseCultivationHours != 3.2 {
		t.Fatalf("historical correction lost components: %+v", corrected)
	}
}

func BenchmarkDailyListeningValidationHistory(b *testing.B) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, storage.DailyOperationsLocation)
	days := make(map[string]float64, 730)
	sessions := make([]absListeningSession, 730)
	for i := range sessions {
		start := now.AddDate(0, 0, -i-1)
		day := SectDayKey(start)
		days[day] = 3600
		sessions[i] = absListeningSession{SessionKey: fmt.Sprint(i), DateKey: day, StartedAt: start, UpdatedAt: start.Add(time.Hour), ListeningSeconds: 3600, HasListeningTime: true}
	}
	b.ResetTimer()
	for b.Loop() {
		validateDailyListeningCredit(days, days, sessions, now)
	}
}

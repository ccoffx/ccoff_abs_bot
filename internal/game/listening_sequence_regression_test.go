package game

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

func TestIncrementalListeningMatchesFullTotalsAcrossCorrections(t *testing.T) {
	db, now, original := setupListeningContributionTest(t)
	contributionMust(t, db.Delete(&original).Error)
	stats := make([]model.DailyListeningStat, 24)
	for i := range stats {
		stats[i] = model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now.AddDate(0, 0, i-20)), CappedSeconds: 8 * 3600, EffectiveHours: 6.4}
	}
	contributionMust(t, db.Create(&stats).Error)
	rng := rand.New(rand.NewSource(47291))
	var retreats []model.SectCaveRetreat
	for step := 0; step < 160; step++ {
		index := rng.Intn(len(stats))
		switch step % 6 {
		case 0:
			seconds := float64(rng.Intn(24*3600 + 1))
			contributionMust(t, db.Unscoped().Model(&stats[index]).UpdateColumns(map[string]any{"capped_seconds": seconds, "effective_hours": calculateSectEffectiveHoursFromSeconds(seconds)}).Error)
		case 1:
			contributionMust(t, db.Delete(&stats[index]).Error)
		case 2:
			contributionMust(t, db.Unscoped().Model(&stats[index]).UpdateColumn("deleted_at", nil).Error)
		case 3:
			retreats = append(retreats, addContributionRetreat(t, db, stats[index].DayKey))
		case 4:
			if len(retreats) > 0 {
				contributionMust(t, db.Delete(&retreats[0]).Error)
				retreats = retreats[1:]
			}
		case 5:
			now = now.Add(7 * time.Hour)
		}
		// Run incremental settlement first: full recomputation refreshes caches
		// and could otherwise hide a missing invalidation in the incremental path.
		got, ok := syncCultivationFromDailyListeningStatsAt(1, now)
		if !ok {
			t.Fatalf("step %d: incremental settlement failed", step)
		}
		var cul model.Cultivation
		contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
		var want, wantBase float64
		contributionMust(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			want, wantBase, err = dailyCultivationTotalsInTx(tx, 1, now)
			return err
		}))
		if math.Abs(got-want) > 1e-9 || math.Abs(cul.ListeningBaseHours-wantBase) > 1e-9 {
			t.Fatalf("step %d: incremental total/base %.12f/%.12f, full %.12f/%.12f", step, got, cul.ListeningBaseHours, want, wantBase)
		}
	}
}

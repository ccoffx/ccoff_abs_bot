package sectbonus

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRolloutTransferAndHistoricalCorrections(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bonus.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { pool.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.AutoMigrate(append(Models(), &model.Sect{}, &model.DailyListeningStat{})...))
	now := time.Date(2026, 11, 10, 23, 0, 0, 0, location)
	must(db.Create(&model.Sect{OwnerID: 1, Name: "宗门"}).Error)
	must(Initialize(db, now))
	check := func(user int64, day string, effective, want float64) {
		t.Helper()
		stat := model.DailyListeningStat{UserID: user, DayKey: day, EffectiveHours: effective, SpiritFireHours: 50, BaseCultivationHours: 12}
		must(RefreshDaily(db, &stat))
		if math.Abs(stat.SectOwnerBonusHours-want) > 1e-9 {
			t.Fatalf("user=%d day=%s bonus=%g want=%g", user, day, stat.SectOwnerBonusHours, want)
		}
	}
	check(1, "2026-11-10", 8, 0)
	check(1, "2026-11-11", 8, .24)
	check(2, "2026-11-11", 8, 0)
	transferAt := now.Add(2 * time.Hour)
	must(db.Transaction(func(tx *gorm.DB) error {
		if err := Schedule(tx, 1, false, transferAt); err != nil {
			return err
		}
		return Schedule(tx, 2, true, transferAt)
	}))
	// Repeated startup must not re-enable the old owner or postpone rollout.
	must(Initialize(db, transferAt.Add(48*time.Hour)))
	check(1, "2026-11-11", 8, .24)
	check(2, "2026-11-11", 8, 0)
	check(1, "2026-11-12", 8, 0)
	check(2, "2026-11-12", 8, .24)
	check(1, "2026-11-11", 4, .12)
	// Multiple same-day transfers resolve to a single next-day recipient.
	must(Schedule(db, 2, false, transferAt.Add(time.Hour)))
	must(Schedule(db, 1, true, transferAt.Add(time.Hour)))
	check(1, "2026-11-12", 8, .24)
	check(2, "2026-11-12", 8, 0)
	stat := model.DailyListeningStat{UserID: 1, DayKey: "2026-11-11", EffectiveHours: 8}
	must(db.Create(&stat).Error)
	must(RefreshDaily(db, &stat))
	must(RefreshDaily(db, &stat))
	must(db.First(&stat, stat.ID).Error)
	if stat.SectOwnerBonusHours != .24 {
		t.Fatalf("repeated refresh changed bonus: %+v", stat)
	}
	stat.EffectiveHours = 4
	must(RefreshDaily(db, &stat))
	must(db.First(&stat, stat.ID).Error)
	if stat.SectOwnerBonusHours != .12 {
		t.Fatalf("correction did not update bonus: %+v", stat)
	}
}

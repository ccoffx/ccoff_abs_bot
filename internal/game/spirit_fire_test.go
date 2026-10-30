package game

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSpiritFireCultivationIntegrationAndHistoricalCorrection(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "cultivation.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	old := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = old; conn.Close() })
	for _, m := range append(spiritfire.Models(), &model.DailyListeningStat{}, &model.SectCaveRetreat{}) {
		if err := db.AutoMigrate(m); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	launch := now.AddDate(0, 0, -2)
	if err := spiritfire.Initialize(db, launch); err != nil {
		t.Fatal(err)
	}
	ids := []uint{}
	for id := uint(2); id <= 23; id++ {
		ids = append(ids, id)
	}
	// Durable scheduled loadout was selected on the previous day.
	if err := db.Create(&model.SpiritFireLoadout{UserID: 1, EffectiveDay: SectDayKey(now), FireIDs: spiritfire.Encode(ids)}).Error; err != nil {
		t.Fatal(err)
	}
	records := []model.DailyListeningStat{
		{UserID: 1, DayKey: SectDayKey(launch.AddDate(0, 0, -1)), RawSeconds: 24 * 3600, EffectiveHours: 8.5},
		{UserID: 1, DayKey: SectDayKey(now), RawSeconds: 24 * 3600, EffectiveHours: 8.5},
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, ok := totalCultivationEffectiveHoursFromDailyStats(1, now)
		if !ok || math.Abs(got-(8.5+37.33)) > 1e-9 {
			t.Fatalf("total %v ok=%v", got, ok)
		}
	}
	stat, ok, err := GetTodayDailyListeningStatChecked(1, now)
	if err != nil || !ok {
		t.Fatalf("today: %v %v", ok, err)
	}
	if stat.EffectiveHours != 8.5 || stat.BaseCultivationHours != 8.5 || math.Abs(stat.SpiritFireHours-28.83) > 1e-9 {
		t.Fatal(stat)
	}
	// Sect activities still consume ordinary hours; fire does not inflate them.
	got, ok := GetTodaySectListeningHoursFromCache(1, now)
	if !ok || got != 8.5 {
		t.Fatalf("sect %v %v", got, ok)
	}
	if err := db.Model(&records[1]).Updates(map[string]any{"raw_seconds": 8 * 3600, "effective_hours": 6.4}).Error; err != nil {
		t.Fatal(err)
	}
	got, ok = totalCultivationEffectiveHoursFromDailyStats(1, now.AddDate(0, 0, 1))
	if !ok || math.Abs(got-(8.5+6.4+1.6*1.86)) > 1e-9 {
		t.Fatalf("corrected total %v %v", got, ok)
	}
	// Losing a snapshot table must fail instead of replacing cultivation with zero.
	if err := db.Exec("DROP TABLE spirit_fire_dailies").Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := totalCultivationEffectiveHoursFromDailyStats(1, now); ok {
		t.Fatal("snapshot error ignored")
	}
}

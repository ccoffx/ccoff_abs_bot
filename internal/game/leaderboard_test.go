package game

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLeaderboardCultivationHoursUsesNetDailyStats(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "leaderboard.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.DailyListeningStat{}, &model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	oldDB := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = oldDB })
	stats := []model.DailyListeningStat{
		{UserID: 1, DayKey: "2026-09-30", RawSeconds: 24 * 3600, EffectiveHours: 8.5},
		{UserID: 1, DayKey: "2026-10-25", RawSeconds: 18 * 3600, EffectiveHours: 8.5, SpiritFireHours: 1.25},
		{UserID: 1, DayKey: "2026-10-31", RawSeconds: 8 * 3600, CappedSeconds: 8 * 3600, EffectiveHours: 6.4, SpiritFireHours: 2.1},
		{UserID: 1, DayKey: "2026-11-01", RawSeconds: 24 * 3600, EffectiveHours: 8.5, SpiritFireHours: 3.5},
		{UserID: 2, DayKey: "2026-11-01", RawSeconds: 24 * 3600, EffectiveHours: 8.5},
	}
	if err := db.Create(&stats).Error; err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339, "2026-10-31T04:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(4 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	retreat.ListeningEvidenceJSON = retreatTestEvidence(retreat.StartAt, retreat.EndAt, 4*3600)
	if err := db.Create(&retreat).Error; err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, timeframe, month string
		days                   map[string]bool
		want                   float64
	}{
		{"daily", "daily", "2026-10", map[string]bool{"2026-11-01": true}, 12},
		{"weekly", "weekly", "2026-10", map[string]bool{"2026-10-26": true, "2026-10-31": true, "2026-11-01": true}, 21.3},
		{"monthly", "monthly", "2026-10", nil, 19.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := leaderboardCultivationHours(1, tc.timeframe, tc.days, tc.month)
			if err != nil || math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("got %.2f, want %.2f, err=%v", got, tc.want, err)
			}
		})
	}
}

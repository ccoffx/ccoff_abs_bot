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
	if err := db.AutoMigrate(&model.DailyListeningStat{}, &model.SectCaveRetreat{}, &model.SectOwnerBonusRevision{}); err != nil {
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

func TestLeaderboardOwnerBonusDoesNotOverwriteConcurrentRefresh(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "leaderboard-owner.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.AutoMigrate(&model.DailyListeningStat{}, &model.SectCaveRetreat{}, &model.SectOwnerBonusRevision{}); err != nil {
		t.Fatal(err)
	}
	oldDB := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = oldDB })
	stat := model.DailyListeningStat{UserID: 1, DayKey: "2026-11-11", EffectiveHours: 6.4}
	if err := db.Create(&stat).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SectOwnerBonusRevision{UserID: 1, DayKey: stat.DayKey, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	// Deterministically commit a listening correction after the leaderboard reads
	// its rows, before it calculates the bonus from that older read.
	corrected := false
	if err := db.Callback().Query().After("gorm:query").Register("test:correct-listening", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]model.DailyListeningStat); !ok || corrected || tx.Error != nil {
			return
		}
		corrected = true
		tx.AddError(db.Model(&model.DailyListeningStat{}).Where("id = ?", stat.ID).
			Updates(map[string]any{"effective_hours": 8.5, "sect_owner_bonus_hours": .255}).Error)
	}); err != nil {
		t.Fatal(err)
	}
	days := map[string]bool{stat.DayKey: true}
	got, err := leaderboardCultivationHours(1, "daily", days, "")
	if err != nil || math.Abs(got-6.592) > 1e-9 || !corrected {
		t.Fatalf("older read: got=%g corrected=%v err=%v", got, corrected, err)
	}
	var saved model.DailyListeningStat
	if err := db.First(&saved, stat.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.EffectiveHours != 8.5 || math.Abs(saved.SectOwnerBonusHours-.255) > 1e-9 {
		t.Fatalf("leaderboard overwrote the newer correction: %+v", saved)
	}
	got, err = leaderboardCultivationHours(1, "daily", days, "")
	if err != nil || math.Abs(got-8.755) > 1e-9 {
		t.Fatalf("newer read: got=%g err=%v", got, err)
	}
}

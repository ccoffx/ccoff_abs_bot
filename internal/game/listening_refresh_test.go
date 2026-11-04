package game

import (
	"testing"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRefreshAllDailyListeningStatsReportsUserQueryFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = previous })

	_, _, _, refreshErr := RefreshAllDailyListeningStatsWithOptions(false)
	if refreshErr == nil {
		t.Fatal("missing users table was reported as a successful refresh")
	}
}

func TestRefreshAllDailyListeningStatsReportsPerUserFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.SystemConfig{}, &model.DailyListeningStat{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{TelegramID: 91346, Username: "listener", AbsUserID: "abs-listener"}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousClient := storage.DB, abs.DefaultClient
	storage.DB, abs.DefaultClient = db, nil
	t.Cleanup(func() { storage.DB, abs.DefaultClient = previousDB, previousClient })

	success, total, skipped, refreshErr := RefreshAllDailyListeningStatsWithOptions(false)
	if refreshErr == nil || success != 0 || total != 1 || skipped != 0 {
		t.Fatalf("failed ABS refresh reported success=%d total=%d skipped=%d err=%v", success, total, skipped, refreshErr)
	}
	stored, err := storage.GetSystemConfigStringChecked(storage.DailyListeningRefreshLastErrorKey)
	if err != nil || stored == "" {
		t.Fatalf("failure state was not stored: value=%q err=%v", stored, err)
	}
}

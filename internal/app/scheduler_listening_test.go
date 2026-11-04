package app

import (
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDailyListeningSchedulerKeepsPartialRefreshError(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "refresh.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.SystemConfig{}, &model.DailyListeningStat{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{TelegramID: 91347, Username: "listener", AbsUserID: "abs-listener"}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousClient := storage.DB, abs.DefaultClient
	storage.DB, abs.DefaultClient = db, nil
	t.Cleanup(func() { storage.DB, abs.DefaultClient = previousDB, previousClient })

	runDailyListeningRefreshIfNeeded(nil, time.Now())
	stored, err := storage.GetSystemConfigStringChecked(storage.DailyListeningRefreshLastErrorKey)
	if err != nil || stored == "" {
		t.Fatalf("scheduler cleared a failed refresh: value=%q err=%v", stored, err)
	}
}

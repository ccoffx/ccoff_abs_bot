package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLegacyFixtureUpgradeIsRepeatableAndPreservesState(t *testing.T) {
	if os.Getenv("UPGRADE_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyFixtureUpgradeIsRepeatableAndPreservesState$")
		cmd.Env = append(os.Environ(), "UPGRADE_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("legacy upgrade failed: %v\n%s", err, output)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/legacy-compatible.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(fixture)).Error; err != nil {
		t.Fatal(err)
	}
	raw, _ := db.DB()
	raw.Close()
	config.AppConfig = &config.Config{DbURL: path, DatabaseMaxOpenConns: 1, DatabaseMaxIdleConns: 1, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64)}
	for i := 0; i < 2; i++ {
		InitDB()
		assertCurrentDatabaseIndexes(t)
		var user model.User
		var inventory model.Inventory
		var cfg model.SystemConfig
		var round model.TreasureRound
		if err := storage.DB.Where("telegram_id = ?", 42).First(&user).Error; err != nil || user.Points != 321 || user.AbsUserID != "legacy-abs" {
			t.Fatalf("user changed: %+v %v", user, err)
		}
		if err := storage.DB.Where("user_id = ?", 42).First(&inventory).Error; err != nil || inventory.Quantity != 7 {
			t.Fatalf("inventory changed: %+v %v", inventory, err)
		}
		if err := storage.DB.Where("key = ?", "legacy_custom").First(&cfg).Error; err != nil || cfg.Value != "preserve-me" {
			t.Fatalf("config changed: %+v %v", cfg, err)
		}
		if err := storage.DB.Where("chat_id = ?", -123).First(&round).Error; err != nil || round.Status != "active" || round.RewardsJSON != "[0,0,0,0,2,2,2,5,10]" {
			t.Fatalf("ongoing activity changed: %+v %v", round, err)
		}
		raw, _ := storage.DB.DB()
		raw.Close()
	}
}

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestTreasureExchangeSurvivesRestart(t *testing.T) {
	if os.Getenv("TREASURE_RESTART_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTreasureExchangeSurvivesRestart$")
		cmd.Env = append(os.Environ(), "TREASURE_RESTART_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("treasure exchange restart failed: %v\n%s", err, output)
		}
		return
	}
	config.AppConfig = &config.Config{DbURL: filepath.Join(t.TempDir(), "treasure-restart.db"), DatabaseMaxOpenConns: 1, DatabaseMaxIdleConns: 1, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64)}
	InitDB()
	for _, record := range []interface{}{&model.User{TelegramID: 1, Points: 100}, &model.Cultivation{UserID: 1, MajorRealm: 5}} {
		if err := storage.DB.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := game.GardenGrantInventoryInTx(storage.DB, 1, game.SectSecretRealmTokenItemName, 3); err != nil {
		t.Fatal(err)
	}
	if err := game.GardenGrantInventoryInTx(storage.DB, 1, "地心熔岩髓", 2); err != nil {
		t.Fatal(err)
	}
	exchange := prepareExchangeForTest(t)
	if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); err != nil {
		t.Fatal(err)
	}
	connection, err := storage.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	InitDB()
	assertCurrentDatabaseIndexes(t)
	if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); !errors.Is(err, game.ErrCultivationTreasureCompleted) {
		t.Fatalf("restart allowed duplicate confirmation: %v", err)
	}
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 2 || exchangeTestQuantity(t, "地心熔岩髓") != 3 || treasureTestBalance(t, 1) != 100 {
		t.Fatal("restart changed inventory or points")
	}
}

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

func TestTreasureExchangeAndLegacyAssetsSurviveRestart(t *testing.T) {
	if os.Getenv("SPIRIT_RETIREMENT_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTreasureExchangeAndLegacyAssetsSurviveRestart$")
		cmd.Env = append(os.Environ(), "SPIRIT_RETIREMENT_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("retirement restart failed: %v\n%s", err, output)
		}
		return
	}
	config.AppConfig = &config.Config{DbURL: filepath.Join(t.TempDir(), "retirement.db"), DatabaseMaxOpenConns: 1, DatabaseMaxIdleConns: 1, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64)}
	InitDB()
	legacyTables := []string{"user_lingjing_balances", "user_spirit_servants", "servant_equipments", "servant_manual_studies", "sect_libraries", "sect_manual_unlocks", "sect_fortunes", "sect_beasts"}
	for _, name := range legacyTables {
		if storage.DB.Migrator().HasTable(name) {
			t.Fatalf("fresh startup created removed feature table: %s", name)
		}
	}
	// 旧库可以留有不再使用的表；启动过程不应重建、修改或删除这些数据。
	for _, name := range legacyTables {
		if err := storage.DB.Exec("CREATE TABLE " + name + " (id INTEGER PRIMARY KEY, value TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.DB.Exec("INSERT INTO "+name+" VALUES (1, ?)", "preserved").Error; err != nil {
			t.Fatal(err)
		}
	}
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
	for _, name := range legacyTables {
		var value string
		if err := storage.DB.Raw("SELECT value FROM " + name + " WHERE id = 1").Scan(&value).Error; err != nil || value != "preserved" {
			t.Fatalf("startup modified unused table %s: %s, %v", name, value, err)
		}
	}
	if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); !errors.Is(err, game.ErrCultivationTreasureCompleted) {
		t.Fatalf("restart allowed duplicate confirmation: %v", err)
	}
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 2 || exchangeTestQuantity(t, "地心熔岩髓") != 3 || treasureTestBalance(t, 1) != 100 {
		t.Fatal("restart changed inventory or points")
	}
}

package game

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

func gardenBatchPlots(t *testing.T, count int) {
	t.Helper()
	for no := count; no > 0; no-- {
		if err := storage.DB.Create(&model.GardenPlot{UserID: 1, PlotNo: no}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func gardenBatchStock(t *testing.T, want int) {
	t.Helper()
	var inv model.Inventory
	if err := storage.DB.Where("user_id = ? AND item_name = ?", 1, "青灵叶种子").Take(&inv).Error; err != nil || inv.Quantity != want {
		t.Fatalf("inventory=%d want=%d err=%v", inv.Quantity, want, err)
	}
}

func gardenHasCallback(markup tgbotapi.InlineKeyboardMarkup, callback string) bool {
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != nil && *b.CallbackData == callback {
				return true
			}
		}
	}
	return false
}

func TestGardenPlantAllQuantityAndSnapshots(t *testing.T) {
	for _, stock := range []int{2, 8} {
		t.Run(fmt.Sprint(stock), func(t *testing.T) {
			db := setupGardenProgressDB(t)
			gardenBatchPlots(t, 6)
			gardenTestPlant(t, 2, "huiqicao")
			gardenTestMature(t) // Mature but unharvested is still occupied.
			gardenTestPlant(t, 4, "huiqicao")
			if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
				t.Fatal(err)
			}
			if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", stock); err != nil {
				t.Fatal(err)
			}
			result, err := gardenPlantAll(1, "qingling")
			if err != nil {
				t.Fatal(err)
			}
			wantNos := []int{1, 3, 5, 6}[:min(stock, 4)]
			if fmt.Sprint(result.PlotNos) != fmt.Sprint(wantNos) {
				t.Fatal(result.PlotNos, wantNos)
			}
			gardenBatchStock(t, stock-len(wantNos))
			var plants []model.GardenPlanting
			if err := db.Where("seed_key = ?", "qingling").Find(&plants).Error; err != nil {
				t.Fatal(err)
			}
			if len(plants) != len(wantNos) {
				t.Fatal(plants)
			}
			for _, p := range plants {
				if !p.PlantedAt.Equal(result.PlantedAt) || !p.MaturesAt.Equal(result.MaturesAt) || p.MaturesAt.Sub(p.PlantedAt) != 192*time.Minute || p.Experience != 4 || p.GrowthReduction != 20 || p.OverflowChance != 80 || p.OverflowReward != 5 {
					t.Fatal("incorrect snapshot", p)
				}
			}
			if !strings.Contains(result.text(), "青灵叶") || !strings.Contains(result.text(), fmt.Sprintf("×%d", len(wantNos))) {
				t.Fatal(result.text())
			}
		})
	}
}

func TestGardenPlantAllErrorsAndRollback(t *testing.T) {
	for _, scenario := range []string{"no plots", "missing seed", "zero seed", "unknown seed", "second insert", "progress read"} {
		t.Run(scenario, func(t *testing.T) {
			db := setupGardenProgressDB(t)
			if scenario != "no plots" {
				gardenBatchPlots(t, 3)
			}
			if scenario != "missing seed" {
				if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", 3); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "zero seed" {
				if err := db.Model(&model.Inventory{}).Where("user_id = ?", 1).Update("quantity", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "second insert" {
				if err := db.Exec(`CREATE TRIGGER fail_batch BEFORE INSERT ON garden_plantings WHEN NEW.plot_no = 2 BEGIN SELECT RAISE(ABORT,'unavailable'); END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "progress read" {
				if err := db.Migrator().DropTable(&model.GardenProgress{}); err != nil {
					t.Fatal(err)
				}
			}
			key := "qingling"
			if scenario == "unknown seed" {
				key = "invalid"
			}
			result, err := gardenPlantAll(1, key)
			if err == nil || len(result.PlotNos) != 0 {
				t.Fatal("expected empty failed result", result, err)
			}
			switch scenario {
			case "no plots":
				if !errors.Is(err, storage.ErrGardenNoEmptyPlot) {
					t.Fatal(err)
				}
			case "missing seed", "zero seed":
				if !errors.Is(err, storage.ErrGardenSeedNotEnough) {
					t.Fatal(err)
				}
			case "unknown seed":
				if !errors.Is(err, storage.ErrGardenSeedUnknown) {
					t.Fatal(err)
				}
			}
			var count int64
			if err := db.Model(&model.GardenPlanting{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("partial planting", count, err)
			}
			if scenario != "missing seed" {
				want := 3
				if scenario == "zero seed" {
					want = 0
				}
				gardenBatchStock(t, want)
			}
		})
	}
}

func TestGardenPlantAllConcurrentWithSingleAndReplay(t *testing.T) {
	db := setupGardenProgressDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(10)
	gardenBatchPlots(t, 6)
	if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", 20); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_, err := gardenPlantAll(1, "qingling")
				errs <- err
			} else {
				errs <- gardenPlantSeed(1, 1, "qingling")
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, storage.ErrGardenPlotBusy) && !errors.Is(err, storage.ErrGardenNoEmptyPlot) {
			t.Fatal(err)
		}
	}
	_, err = gardenPlantAll(1, "qingling")
	if !errors.Is(err, storage.ErrGardenNoEmptyPlot) {
		t.Fatal(err)
	}
	gardenBatchStock(t, 14)
	var plants []model.GardenPlanting
	if err := db.Find(&plants).Error; err != nil || len(plants) != 6 {
		t.Fatal(len(plants), err)
	}
}

func TestGardenPlantAllConcurrentDifferentUsers(t *testing.T) {
	db := setupGardenProgressDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(10)
	const users = 12
	for id := int64(1); id <= users; id++ {
		for no := 1; no <= 6; no++ {
			if err := db.Create(&model.GardenPlot{UserID: id, PlotNo: no}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := GardenGrantInventoryInTx(db, id, "青灵叶种子", 8); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, users)
	start := make(chan struct{})
	for id := int64(1); id <= users; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			result, err := gardenPlantAll(id, "qingling")
			if err == nil && len(result.PlotNos) != 6 {
				err = fmt.Errorf("user %d planted %v", id, result.PlotNos)
			}
			errs <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := int64(1); id <= users; id++ {
		var inv model.Inventory
		if err := db.Where("user_id = ? AND item_name = ?", id, "青灵叶种子").Take(&inv).Error; err != nil || inv.Quantity != 2 {
			t.Fatalf("user=%d inventory=%d err=%v", id, inv.Quantity, err)
		}
		var count int64
		if err := db.Model(&model.GardenPlanting{}).Where("user_id = ?", id).Count(&count).Error; err != nil || count != 6 {
			t.Fatalf("user=%d plantings=%d err=%v", id, count, err)
		}
	}
}

func TestGardenPlantAllRetryRollsBackPartialAttempt(t *testing.T) {
	for _, scenario := range []string{"busy snapshot", "locked", "persistent busy", "permanent error"} {
		t.Run(scenario, func(t *testing.T) {
			db := setupGardenProgressDB(t)
			gardenBatchPlots(t, 3)
			if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", 8); err != nil {
				t.Fatal(err)
			}
			creates := 0
			if err := db.Callback().Create().Before("gorm:create").Register("test:planting_conflict", func(tx *gorm.DB) {
				if tx.Statement.Table != "garden_plantings" {
					return
				}
				creates++
				// Fail the second insert, after a crop and inventory deduction
				// have been written, to exercise rollback and result reset.
				if creates == 2 || (scenario == "persistent busy" && creates%2 == 0) {
					switch scenario {
					case "locked":
						tx.AddError(sqlite3.Error{Code: sqlite3.ErrLocked})
					case "permanent error":
						tx.AddError(errors.New("planting unavailable"))
					default:
						tx.AddError(sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot})
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			result, err := gardenPlantAll(1, "qingling")
			wantStock, wantPlants, wantCreates := 5, int64(3), 5
			if scenario == "persistent busy" || scenario == "permanent error" {
				wantStock, wantPlants, wantCreates = 8, 0, 2
				if scenario == "persistent busy" {
					wantCreates = 16
				}
				if err == nil || len(result.PlotNos) != 0 {
					t.Fatalf("failed result=%+v err=%v", result, err)
				}
			} else if err != nil || fmt.Sprint(result.PlotNos) != "[1 2 3]" {
				t.Fatalf("retried result=%+v err=%v", result, err)
			}
			gardenBatchStock(t, wantStock)
			var count int64
			if err := db.Model(&model.GardenPlanting{}).Count(&count).Error; err != nil || count != wantPlants || creates != wantCreates {
				t.Fatalf("plants=%d creates=%d err=%v", count, creates, err)
			}
		})
	}
}

func TestGardenPlantAllScreens(t *testing.T) {
	db := setupGardenProgressDB(t)
	gardenBatchPlots(t, 3)
	text, markup := renderGardenPlantAllList(1)
	if !strings.Contains(text, "暂无种子") || !gardenHasCallback(markup, "garden:shop") {
		t.Fatal(text, markup)
	}
	if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", 2); err != nil {
		t.Fatal(err)
	}
	text, markup = renderGardenPlantAllList(1)
	if !strings.Contains(text, "空闲灵田：`3`") || !strings.Contains(text, "基础 4小时0分") || !gardenHasCallback(markup, "garden:plantall:qingling") {
		t.Fatal(text, markup)
	}
	if len(markup.InlineKeyboard) == 0 || markup.InlineKeyboard[0][0].Text != "种青灵叶 ×2" {
		t.Fatal(markup)
	}
	_, fields := renderGardenFields(1)
	if !gardenHasCallback(fields, "garden:plantalllist") {
		t.Fatal(fields)
	}
	// Buying keeps the shop and exposes navigation to planting and individual plots.
	if err := gardenBuySeed(1, "qingling"); err != nil {
		t.Fatal(err)
	}
	_, shop := renderGardenSeedShop(1)
	for _, cb := range []string{"garden:plantalllist", "garden:fields", "garden:home", "garden:buy:qingling"} {
		if !gardenHasCallback(shop, cb) {
			t.Fatal("missing", cb)
		}
	}
	// Execution recalculates a stale preview of two seeds after another purchase.
	result, err := gardenPlantAll(1, "qingling")
	if err != nil || len(result.PlotNos) != 3 {
		t.Fatal(result, err)
	}
	text, markup = renderGardenPlantAllList(1)
	if !strings.Contains(text, "暂无空闲灵田") || gardenHasCallback(markup, "garden:plantall:qingling") {
		t.Fatal(text, markup)
	}
	_, fields = renderGardenFields(1)
	if gardenHasCallback(fields, "garden:plantalllist") {
		t.Fatal(fields)
	}
	if err := db.Migrator().DropTable(&model.GardenProgress{}); err != nil {
		t.Fatal(err)
	}
	text, markup = renderGardenPlantAllList(1)
	if !strings.Contains(text, "读取失败") || gardenHasCallback(markup, "garden:plantall:qingling") {
		t.Fatal(text, markup)
	}
}

package game

import (
	"bytes"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupGardenProgressDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "garden.db")+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	previous := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = previous; _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.Inventory{}, &model.PointTransaction{}, &model.GardenProgress{}, &model.GardenPlot{}, &model.GardenPlanting{}, &model.GardenSeedPurchase{}, &model.GardenHerbMarketSale{}, &model.GardenRecipeUnlock{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	for _, index := range storage.DatabaseIndexes {
		if strings.Contains(index, "idx_garden_") || strings.Contains(index, "idx_inventory_user_item_unique") {
			if err := db.Exec(index).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	contributionMust(t, storage.InitializeGardenProductionTotals(db))
	if err := db.Create(&model.User{TelegramID: 1, Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func gardenTestPlant(t *testing.T, plotNo int, key string) model.GardenPlanting {
	t.Helper()
	cfg, ok := GardenSeedByKey(key)
	if !ok {
		t.Fatal("unknown seed", key)
	}
	plot := model.GardenPlot{UserID: 1, PlotNo: plotNo}
	if err := storage.DB.Where("user_id = ? AND plot_no = ?", 1, plotNo).FirstOrCreate(&plot).Error; err != nil {
		t.Fatal(err)
	}
	if err := GardenGrantInventoryInTx(storage.DB, 1, cfg.SeedName, 1); err != nil {
		t.Fatal(err)
	}
	if err := gardenPlantSeed(1, plotNo, key); err != nil {
		t.Fatal(err)
	}
	var planting model.GardenPlanting
	if err := storage.DB.Where("plot_id = ? AND status = ?", plot.ID, gardenStatusGrowing).Take(&planting).Error; err != nil {
		t.Fatal(err)
	}
	return planting
}

func gardenTestMature(t *testing.T) {
	t.Helper()
	if err := storage.DB.Model(&model.GardenPlanting{}).Where("status = ?", gardenStatusGrowing).Update("matures_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
}

func gardenTestZeroEntropy(t *testing.T) {
	t.Helper()
	previous := rand.Reader
	rand.Reader = &gameFailingEntropy{remaining: -1}
	t.Cleanup(func() { rand.Reader = previous })
}

func TestGardenLevelThresholdsAndRewards(t *testing.T) {
	thresholds := []int64{0, 48, 192, 480, 960, 1800, 3000, 4800, 7600, 12000}
	for i, threshold := range thresholds {
		if got := gardenLevel(threshold); got != i+1 {
			t.Fatalf("experience %d: got level %d", threshold, got)
		}
		if i > 0 && gardenLevel(threshold-1) != i {
			t.Fatal("premature level up", threshold)
		}
	}
	if gardenLevel(1_000_000) != 10 || gardenGrowDuration(12*time.Hour, gardenLevels[9].GrowthReduction) != 9*time.Hour+36*time.Minute {
		t.Fatal("maximum level or duration incorrect")
	}
	wantRewards := map[string]int{"huiqicao": 3, "qingling": 5, "chiyang": 8, "yueyingteng": 12, "xuanyanshen": 18, "zijingzhi": 28, "longxue": 16, "chixinlian": 24}
	for _, cfg := range GardenSeeds {
		reward := gardenOverflowReward(cfg)
		if reward != wantRewards[cfg.Key] {
			t.Fatal("wrong overflow reward", cfg.Key, reward)
		}
		// 80% 的命中率对应种子成本 16% 的额外期望收益。
		if cfg.Purchasable && reward*gardenLevels[9].OverflowChance != cfg.Price*16 {
			t.Fatal("maximum expected return changed", cfg.Key)
		}
	}
}

func TestGardenOverflowProbabilityBoundaries(t *testing.T) {
	previous := rand.Reader
	defer func() { rand.Reader = previous }()
	rand.Reader = &gameFailingEntropy{err: errors.New("entropy unavailable")}
	if hit, err := gardenRollOverflow(0); err != nil || hit {
		t.Fatal("zero probability consumed randomness or granted points", hit, err)
	}
	for _, level := range gardenLevels[1:] {
		for _, roll := range []int{1, level.OverflowChance, level.OverflowChance + 1, 100} {
			// crypto/rand.Int uses a seven-bit sample for the range [0, 100).
			rand.Reader = bytes.NewReader([]byte{byte(roll - 1)})
			hit, err := gardenRollOverflow(level.OverflowChance)
			if err != nil || hit != (roll <= level.OverflowChance) {
				t.Fatalf("chance=%d roll=%d hit=%v err=%v", level.OverflowChance, roll, hit, err)
			}
		}
	}
}

func TestGardenPlantingSnapshotsSurviveLevelAndConfigChanges(t *testing.T) {
	db := setupGardenProgressDB(t)
	gardenTestZeroEntropy(t)
	if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 11990}).Error; err != nil {
		t.Fatal(err)
	}
	first := gardenTestPlant(t, 1, "xuanyanshen")
	second := gardenTestPlant(t, 2, "zijingzhi")
	if first.Experience != 12 || first.GrowthReduction != 18 || first.OverflowChance != 70 || first.OverflowReward != 18 || first.MaturesAt.Sub(first.PlantedAt) != gardenGrowDuration(12*time.Hour, 18) {
		t.Fatalf("wrong planting snapshot: %+v", first)
	}
	if result, err := gardenHarvestPlot(1, 1); !errors.Is(err, storage.ErrGardenNotMature) || result.Plots != 0 {
		t.Fatal("immature planting harvested", result, err)
	}
	gardenTestMature(t)
	result, err := gardenHarvestPlot(1, 1)
	if err != nil || result.LevelBefore != 9 || result.LevelAfter != 10 || result.Experience != 12 || result.SpiritPoints != 18 {
		t.Fatal("wrong harvest or level up", result, err)
	}
	if !strings.Contains(result.text(), "下次播种生效") || !strings.Contains(result.text(), "Lv.10") {
		t.Fatal("missing upgrade message", result.text())
	}
	next := gardenTestPlant(t, 1, "xuanyanshen")
	if next.GrowthReduction != 20 || next.OverflowChance != 80 || next.Experience != 12 || next.MaturesAt.Sub(next.PlantedAt) != 9*time.Hour+36*time.Minute {
		t.Fatalf("new level did not apply: %+v", next)
	}
	original := GardenSeeds
	GardenSeeds = append([]gardenSeedConfig(nil), GardenSeeds...)
	t.Cleanup(func() { GardenSeeds = original })
	for i := range GardenSeeds {
		if GardenSeeds[i].Key == "zijingzhi" {
			GardenSeeds[i].Price = 500
			GardenSeeds[i].GrowDuration = time.Hour
		}
	}
	result, err = gardenHarvestPlot(1, 2)
	if err != nil || result.Experience != 18 || result.SpiritPoints != 28 {
		t.Fatal("harvest did not use planting snapshot", result, err)
	}
	if err := db.First(&second, second.ID).Error; err != nil || second.GrowthReduction != 18 || second.OverflowChance != 70 || second.OverflowPoints != 28 {
		t.Fatal("existing planting changed after upgrade", second, err)
	}
	var user model.User
	if err := db.Where("telegram_id = ?", 1).Take(&user).Error; err != nil || user.Points != 146 {
		t.Fatal("points not granted", user.Points, err)
	}
	var transactions []model.PointTransaction
	if err := db.Order("id").Find(&transactions).Error; err != nil || len(transactions) != 2 {
		t.Fatal("missing overflow audit", transactions, err)
	}
	for _, transaction := range transactions {
		if transaction.Type != "garden_spirit_overflow" || transaction.RefType != "garden_planting" || transaction.RefID == "" {
			t.Fatal("overflow audit lacks planting reference", transaction)
		}
	}
	if result, err := gardenHarvestPlot(1, 2); err == nil || result.Plots != 0 || result.SpiritPoints != 0 {
		t.Fatal("duplicate harvest succeeded", result, err)
	}
}

func TestGardenHarvestModesUseEachPlantingSnapshot(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "single"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			db := setupGardenProgressDB(t)
			if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 47}).Error; err != nil {
				t.Fatal(err)
			}
			for _, no := range []int{1, 2} {
				gardenTestPlant(t, no, "zijingzhi")
			}
			gardenTestMature(t)
			// Both plants have 0% snapshots, even though the first harvest levels up.
			previous := rand.Reader
			rand.Reader = &gameFailingEntropy{err: errors.New("no entropy expected")}
			defer func() { rand.Reader = previous }()
			var result gardenHarvestResult
			var err error
			if all {
				result, err = gardenHarvestAll(1)
			} else {
				result, err = gardenHarvestPlot(1, 1)
				if err != nil {
					t.Fatal(err)
				}
				result, err = gardenHarvestPlot(1, 2)
			}
			if err != nil || result.SpiritPoints != 0 {
				t.Fatal("harvest used a newly unlocked probability", result, err)
			}
			experience, err := gardenExperience(db, 1)
			if err != nil || experience != 83 {
				t.Fatal("incorrect experience", experience, err)
			}
		})
	}
}

func TestGardenHarvestAllRollsIndependentlyAndPersists(t *testing.T) {
	db := setupGardenProgressDB(t)
	if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
		t.Fatal(err)
	}
	for _, no := range []int{1, 2} {
		gardenTestPlant(t, no, "zijingzhi")
	}
	gardenTestMature(t)
	previous := rand.Reader
	rand.Reader = bytes.NewReader([]byte{79, 80}) // roll 80 hits; roll 81 misses.
	defer func() { rand.Reader = previous }()
	result, err := gardenHarvestAll(1)
	if err != nil || result.Plots != 2 || result.Quantity != 2 || len(result.Items) != 1 || result.Items[0].Quantity != 2 || result.Experience != 36 || result.SpiritPoints != 28 || result.LevelAfter != 10 {
		t.Fatal("batch did not roll per plot", result, err)
	}
	// A new database handle has no in-memory progress or result to rely on.
	reopened, err := gorm.Open(sqlite.Open(db.Dialector.(*sqlite.Dialector).DSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := reopened.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	storage.DB = reopened
	experience, err := gardenExperience(reopened, 1)
	if err != nil || experience != 12036 {
		t.Fatal("progress lost on restart", experience, err)
	}
	if retry, err := gardenHarvestAll(1); err == nil || retry.Plots != 0 {
		t.Fatal("restart allowed duplicate harvest", retry, err)
	}
	var records []model.GardenPlanting
	if err := reopened.Order("plot_no").Find(&records).Error; err != nil || len(records) != 2 || records[0].OverflowPoints != 28 || records[1].OverflowPoints != 0 {
		t.Fatal("per-plot rewards not persisted", records, err)
	}
}

func TestGardenHarvestFailureRollsBackAllAssets(t *testing.T) {
	triggers := map[string]string{
		"planting":  `CREATE TRIGGER fail_harvest BEFORE UPDATE ON garden_plantings BEGIN SELECT RAISE(ABORT,'unavailable'); END`,
		"inventory": `CREATE TRIGGER fail_harvest BEFORE INSERT ON inventories WHEN NEW.item_name = '紫晶芝' BEGIN SELECT RAISE(ABORT,'unavailable'); END`,
		"wallet":    `CREATE TRIGGER fail_harvest BEFORE INSERT ON point_transactions BEGIN SELECT RAISE(ABORT,'unavailable'); END`,
		"progress":  `CREATE TRIGGER fail_harvest BEFORE UPDATE ON garden_progresses BEGIN SELECT RAISE(ABORT,'unavailable'); END`,
		"entropy":   "",
	}
	for stage, trigger := range triggers {
		for _, all := range []bool{false, true} {
			name := stage + "/single"
			if all {
				name = stage + "/all"
			}
			t.Run(name, func(t *testing.T) {
				db := setupGardenProgressDB(t)
				gardenTestZeroEntropy(t)
				if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
					t.Fatal(err)
				}
				gardenTestPlant(t, 1, "zijingzhi")
				if all {
					gardenTestPlant(t, 2, "zijingzhi")
				}
				gardenTestMature(t)
				if trigger != "" {
					if err := db.Exec(trigger).Error; err != nil {
						t.Fatal(err)
					}
				} else {
					reads := 0
					if all {
						reads = 1 // First plot settles before the second plot's RNG fails.
					}
					rand.Reader = &gameFailingEntropy{remaining: reads, err: errors.New("entropy unavailable")}
				}
				harvest := func() (gardenHarvestResult, error) {
					if all {
						return gardenHarvestAll(1)
					}
					return gardenHarvestPlot(1, 1)
				}
				if result, err := harvest(); err == nil || result.Plots != 0 || result.Experience != 0 || result.SpiritPoints != 0 {
					t.Fatal("failed harvest returned success", result, err)
				}
				experience, err := gardenExperience(db, 1)
				if err != nil || experience != 12000 {
					t.Fatal("failed harvest granted experience", experience, err)
				}
				var user model.User
				if err := db.Where("telegram_id = ?", 1).Take(&user).Error; err != nil || user.Points != 100 {
					t.Fatal("failed harvest granted points", user.Points, err)
				}
				for _, query := range []*gorm.DB{
					db.Model(&model.GardenPlanting{}).Where("status <> ? OR quantity <> 0 OR overflow_points <> 0 OR harvested_at IS NOT NULL", gardenStatusGrowing),
					db.Model(&model.Inventory{}).Where("item_name = ?", "紫晶芝"),
					db.Model(&model.PointTransaction{}),
				} {
					var count int64
					if err := query.Count(&count).Error; err != nil || count != 0 {
						t.Fatal("partial harvest committed", count, err)
					}
				}
				if trigger != "" {
					if err := db.Exec("DROP TRIGGER fail_harvest").Error; err != nil {
						t.Fatal(err)
					}
				}
				rand.Reader = &gameFailingEntropy{remaining: -1}
				if result, err := harvest(); err != nil || result.SpiritPoints == 0 {
					t.Fatal("rolled-back harvest could not be retried", result, err)
				}
			})
		}
	}
}

func TestGardenConcurrentHarvestCreditsOnce(t *testing.T) {
	db := setupGardenProgressDB(t)
	// Use separate connections so database constraints also guard racing requests.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(4)
	gardenTestZeroEntropy(t)
	if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
		t.Fatal(err)
	}
	gardenTestPlant(t, 1, "zijingzhi")
	gardenTestMature(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan gardenHarvestResult, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(all bool) {
			defer wg.Done()
			<-start
			result, err := gardenHarvest(1, 1, all)
			if err == nil {
				results <- result
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()
	close(results)
	if len(results) != 1 {
		t.Fatalf("expected exactly one committed harvest, got %d", len(results))
	}
	experience, err := gardenExperience(db, 1)
	if err != nil || experience != 12018 {
		t.Fatal("concurrent harvest duplicated experience", experience, err)
	}
	var user model.User
	if err := db.Where("telegram_id = ?", 1).Take(&user).Error; err != nil || user.Points != 128 {
		t.Fatal("concurrent harvest duplicated points", user.Points, err)
	}
	var count int64
	if err := db.Model(&model.PointTransaction{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("concurrent harvest duplicated ledger", count, err)
	}
}

func TestGardenProgressScreensAndReadFailure(t *testing.T) {
	db := setupGardenProgressDB(t)
	if text := gardenProgressText(1); !strings.Contains(text, "Lv.1") || !strings.Contains(text, "0/48") {
		t.Fatal("new garden progress incorrect", text)
	}
	if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
		t.Fatal(err)
	}
	gardenTestPlant(t, 1, "xuanyanshen")
	home, markup := renderGardenHome(1)
	if !strings.Contains(home, "Lv.10") || !strings.Contains(home, "80%") || !strings.Contains(home, "已满级") {
		t.Fatal("home lacks level details", home)
	}
	found := false
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			found = found || button.CallbackData != nil && *button.CallbackData == "garden:level"
		}
	}
	if !found {
		t.Fatal("level explanation button missing")
	}
	shop, _ := renderGardenSeedShop(1)
	if !strings.Contains(shop, "基础 12小时0分 → 当前 9小时36分") {
		t.Fatal("shop duration preview incorrect", shop)
	}
	if err := GardenGrantInventoryInTx(db, 1, "玄岩参种子", 1); err != nil {
		t.Fatal(err)
	}
	list, _ := renderGardenPlantList(1, 2)
	if !strings.Contains(list, "基础 12小时0分 → 当前 9小时36分") {
		t.Fatal("plant list duration preview incorrect", list)
	}
	var planting model.GardenPlanting
	if err := db.Take(&planting).Error; err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{planting.MaturesAt.Add(-time.Nanosecond), planting.MaturesAt, planting.MaturesAt} {
		claimed, err := claimGardenMaturityNotice(planting.ID, at)
		if err != nil || claimed != (i == 1) {
			t.Fatal("maturity notice did not follow saved reduced duration", claimed, err)
		}
	}
	notice := gardenMaturityNoticeText([]model.GardenPlanting{planting})
	if !strings.Contains(notice, "玄岩参") {
		t.Fatal("maturity notice lost herb", notice)
	}
	if err := db.Migrator().DropTable(&model.GardenProgress{}); err != nil {
		t.Fatal(err)
	}
	if _, err := gardenExperience(db, 1); err == nil || !strings.Contains(gardenProgressText(1), "读取失败") {
		t.Fatal("database failure treated as new garden", err)
	}
	shop, _ = renderGardenSeedShop(1)
	if !strings.Contains(shop, "当前读取失败") {
		t.Fatal("failed progress read showed an invented duration", shop)
	}
	if err := db.Create(&model.GardenPlot{UserID: 1, PlotNo: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gardenPlantSeed(1, 2, "xuanyanshen"); err == nil {
		t.Fatal("planting succeeded without reading progress")
	}
	var seed model.Inventory
	if err := db.Where("user_id = ? AND item_name = ?", 1, "玄岩参种子").Take(&seed).Error; err != nil || seed.Quantity != 1 {
		t.Fatal("failed progress read consumed seed", seed.Quantity, err)
	}
}

package game

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestGardenAlchemyRecordsOnlyCommittedProducts(t *testing.T) {
	for _, recipeKey := range []string{"ningyuan", "qinglingyangqi"} {
		t.Run(recipeKey, func(t *testing.T) {
			testGardenAlchemyRecordsOnlyCommittedProducts(t, recipeKey)
		})
	}
}

func testGardenAlchemyRecordsOnlyCommittedProducts(t *testing.T, recipeKey string) {
	t.Helper()
	recipe, ok := gardenRecipeByKey(recipeKey)
	if !ok {
		t.Fatal("recipe missing")
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "alchemy.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	if err := db.AutoMigrate(&model.User{}, &model.Inventory{}, &model.PointTransaction{}, &model.GardenRecipeUnlock{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX inventory_test_unique ON inventories(user_id,item_name) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&model.User{TelegramID: 1, Username: "alchemy", Points: 100},
		&model.GardenRecipeUnlock{UserID: 1, RecipeKey: recipeKey},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, material := range recipe.Materials {
		if err := db.Create(&model.Inventory{UserID: 1, ItemName: material.ItemName, Quantity: material.Quantity * 3}).Error; err != nil {
			t.Fatal(err)
		}
	}
	check := func(wantCount int64, wantPoints, wantProduct int) {
		t.Helper()
		var count int64
		if err := db.Model(&model.GardenAlchemyRecord{}).Where("user_id = ?", 1).Count(&count).Error; err != nil || count != wantCount {
			t.Fatal("alchemy count did not match committed products", count, err)
		}
		var user model.User
		if err := db.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != wantPoints {
			t.Fatal("wrong point balance", user.Points, err)
		}
		quantities := map[string]int{recipe.ProductName: wantProduct}
		for _, material := range recipe.Materials {
			quantities[material.ItemName] = material.Quantity * (3 - wantProduct)
		}
		for name, want := range quantities {
			var quantity int
			if err := db.Model(&model.Inventory{}).Select("COALESCE(SUM(quantity), 0)").Where("user_id = ? AND item_name = ?", 1, name).Scan(&quantity).Error; err != nil || quantity != want {
				t.Fatal("wrong inventory after alchemy", name, quantity, err)
			}
		}
	}
	// 写入成丹记录失败时，刚扣除的材料、积分和刚发放的丹药全部回滚。
	if err := db.Exec(`CREATE TRIGGER fail_alchemy_record BEFORE INSERT ON garden_alchemy_records BEGIN SELECT RAISE(ABORT,'record unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := gardenAlchemy(1, recipeKey); err == nil {
		t.Fatal("record failure did not fail alchemy")
	}
	check(0, 100, 0)
	if err := db.Exec("DROP TRIGGER fail_alchemy_record").Error; err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		product, err := gardenAlchemy(1, recipeKey)
		if err != nil || product != recipe.ProductName {
			t.Fatal("alchemy failed", product, err)
		}
		check(int64(i), 100-i*recipe.AlchemyCost, i)
	}
	if _, err := gardenAlchemy(1, recipeKey); err == nil {
		t.Fatal("alchemy with insufficient materials succeeded")
	}
	check(3, 100-3*recipe.AlchemyCost, 3)
}

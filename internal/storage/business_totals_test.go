package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func businessTotalsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "totals.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	totalsMust(t, err)
	pool, err := db.DB()
	totalsMust(t, err)
	t.Cleanup(func() { pool.Close() })
	totalsMust(t, db.AutoMigrate(&model.GardenPlanting{}, &model.GardenAlchemyRecord{}, &model.SectShopPurchase{}))
	return db
}

func totalsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertGardenTotal(t *testing.T, db *gorm.DB, userID, harvest, alchemy int64) {
	t.Helper()
	var total model.GardenProductionTotal
	err := db.First(&total, "user_id = ?", userID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	if total.HarvestCount != harvest || total.AlchemyCount != alchemy {
		t.Fatalf("user %d: total=%+v, want harvest/alchemy %d/%d", userID, total, harvest, alchemy)
	}
}

func assertShopTotal(t *testing.T, db *gorm.DB, sectID, userID, want int64) {
	t.Helper()
	var total model.SectShopContributionTotal
	err := db.First(&total, "sect_id = ? AND user_id = ?", sectID, userID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	if total.Spent != want {
		t.Fatalf("sect/user %d/%d: spent=%d, want %d", sectID, userID, total.Spent, want)
	}
}

func TestBusinessTotalsBackfillOnceAndFollowSourceCorrections(t *testing.T) {
	db := businessTotalsDB(t)
	now := time.Now()
	harvest := model.GardenPlanting{UserID: 1, Quantity: 4, Status: "harvested", HarvestedAt: &now}
	alchemy := model.GardenAlchemyRecord{UserID: 1, Quantity: 2}
	purchase := model.SectShopPurchase{SectID: 10, UserID: 1, CostContribution: 100}
	deleted := model.GardenPlanting{UserID: 1, Quantity: 99, Status: "harvested", HarvestedAt: &now}
	for _, row := range []any{&harvest, &alchemy, &purchase, &deleted,
		&model.GardenPlanting{UserID: 1, Quantity: 999, Status: "growing"},
		&model.GardenPlanting{UserID: 1, Quantity: 999, Status: "harvested"},
		&model.GardenAlchemyRecord{UserID: 1, Quantity: -1}} {
		totalsMust(t, db.Create(row).Error)
	}
	totalsMust(t, db.Delete(&deleted).Error)
	totalsMust(t, InitializeGardenProductionTotals(db))
	totalsMust(t, InitializeSectShopContributionTotals(db))
	assertGardenTotal(t, db, 1, 4, 2)
	assertShopTotal(t, db, 10, 1, 100)

	totalsMust(t, db.Model(&harvest).Update("quantity", 7).Error)
	totalsMust(t, db.Model(&alchemy).Update("quantity", 3).Error)
	totalsMust(t, db.Model(&purchase).Update("cost_contribution", 80).Error)
	assertGardenTotal(t, db, 1, 7, 3)
	assertShopTotal(t, db, 10, 1, 80)

	// Repeated startup must not rebuild source histories or reset current totals.
	for _, table := range []string{"garden_production_totals", "sect_shop_contribution_totals"} {
		totalsMust(t, db.Exec("CREATE TRIGGER reject_rebuild_"+table+" BEFORE DELETE ON "+table+" BEGIN SELECT RAISE(ABORT, 'unexpected rebuild'); END").Error)
	}
	totalsMust(t, InitializeGardenProductionTotals(db))
	totalsMust(t, InitializeSectShopContributionTotals(db))
	assertGardenTotal(t, db, 1, 7, 3)
	assertShopTotal(t, db, 10, 1, 80)

	// Bulk ownership changes maintain both identities in the source transaction.
	totalsMust(t, db.Model(&harvest).Update("user_id", 2).Error)
	totalsMust(t, db.Model(&alchemy).Update("user_id", 2).Error)
	totalsMust(t, db.Model(&purchase).Updates(map[string]any{"sect_id": 20, "user_id": 2}).Error)
	assertGardenTotal(t, db, 1, 0, 0)
	assertGardenTotal(t, db, 2, 7, 3)
	assertShopTotal(t, db, 10, 1, 0)
	assertShopTotal(t, db, 20, 2, 80)

	totalsMust(t, db.Delete(&harvest).Error)
	totalsMust(t, db.Delete(&purchase).Error)
	assertGardenTotal(t, db, 2, 0, 3)
	assertShopTotal(t, db, 20, 2, 0)
	totalsMust(t, db.Unscoped().Model(&harvest).Update("deleted_at", nil).Error)
	totalsMust(t, db.Unscoped().Model(&purchase).Update("deleted_at", nil).Error)
	assertGardenTotal(t, db, 2, 7, 3)
	assertShopTotal(t, db, 20, 2, 80)
	totalsMust(t, db.Unscoped().Delete(&harvest).Error)
	totalsMust(t, db.Unscoped().Delete(&alchemy).Error)
	totalsMust(t, db.Unscoped().Delete(&purchase).Error)
	assertGardenTotal(t, db, 2, 0, 0)
	assertShopTotal(t, db, 20, 2, 0)
}

func TestBusinessTotalsRollBackWithSourcesAndRejectPartialWrites(t *testing.T) {
	db := businessTotalsDB(t)
	totalsMust(t, InitializeGardenProductionTotals(db))
	totalsMust(t, InitializeSectShopContributionTotals(db))
	now := time.Now()
	rollback := errors.New("rollback business transaction")
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, row := range []any{
			&model.GardenPlanting{UserID: 1, Status: "harvested", HarvestedAt: &now, Quantity: 4},
			&model.GardenAlchemyRecord{UserID: 1, Quantity: 2},
			&model.SectShopPurchase{SectID: 1, UserID: 1, CostContribution: 9},
		} {
			if err := tx.Create(row).Error; err != nil {
				return err
			}
		}
		assertGardenTotal(t, tx, 1, 4, 2)
		assertShopTotal(t, tx, 1, 1, 9)
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	assertGardenTotal(t, db, 1, 0, 0)
	assertShopTotal(t, db, 1, 1, 0)
	for _, table := range []string{"garden_production_totals", "sect_shop_contribution_totals"} {
		totalsMust(t, db.Exec("CREATE TRIGGER reject_total_"+table+" BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT, 'total unavailable'); END").Error)
	}
	for _, row := range []any{&model.GardenAlchemyRecord{UserID: 1, Quantity: 2}, &model.SectShopPurchase{SectID: 1, UserID: 1, CostContribution: 9}} {
		if err := db.Create(row).Error; err == nil {
			t.Fatal("source committed without its counter")
		}
		var count int64
		totalsMust(t, db.Model(row).Count(&count).Error)
		if count != 0 {
			t.Fatal("partial source write", count)
		}
	}
}

func TestMissingBusinessTotalTablesAreRebuiltDespiteExistingMarkers(t *testing.T) {
	db := businessTotalsDB(t)
	totalsMust(t, InitializeGardenProductionTotals(db))
	totalsMust(t, InitializeSectShopContributionTotals(db))
	totalsMust(t, db.Create(&model.GardenAlchemyRecord{UserID: 1, Quantity: 3}).Error)
	totalsMust(t, db.Create(&model.SectShopPurchase{SectID: 1, UserID: 1, CostContribution: 8}).Error)
	totalsMust(t, db.Migrator().DropTable(&model.GardenProductionTotal{}))
	totalsMust(t, db.Migrator().DropTable(&model.SectShopContributionTotal{}))
	totalsMust(t, InitializeGardenProductionTotals(db))
	totalsMust(t, InitializeSectShopContributionTotals(db))
	assertGardenTotal(t, db, 1, 0, 3)
	assertShopTotal(t, db, 1, 1, 8)
}

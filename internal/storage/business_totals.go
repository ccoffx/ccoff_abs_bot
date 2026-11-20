package storage

import (
	"fmt"
	"strings"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

// InitializeGardenProductionTotals installs transactional delta maintenance.
// Database triggers also cover bulk account transfers, soft deletion and repairs,
// rather than relying on every caller to remember a separate counter write.
func InitializeGardenProductionTotals(db *gorm.DB) error {
	created := !db.Migrator().HasTable(&model.GardenProductionTotal{})
	if err := db.AutoMigrate(&model.GardenProductionTotal{}, &model.BusinessTotalMigration{}); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if created {
			if err := tx.Where("key = ?", "garden_production_v1").Delete(&model.BusinessTotalMigration{}).Error; err != nil {
				return err
			}
		}
		for _, source := range []struct{ table, column, eligible string }{
			{"garden_plantings", "harvest_count", "deleted_at IS NULL AND status = 'harvested' AND harvested_at IS NOT NULL AND quantity > 0"},
			{"garden_alchemy_records", "alchemy_count", "quantity > 0"},
		} {
			amount := func(prefix string) string {
				condition := source.eligible
				if prefix != "" {
					// All identifiers here are fixed, application-owned SQL.
					condition = qualifyTotalCondition(condition, prefix)
				}
				return "CASE WHEN " + condition + " THEN " + prefix + "quantity ELSE 0 END"
			}
			change := func(prefix, sign string) string {
				return fmt.Sprintf(`INSERT INTO garden_production_totals(user_id, %s) VALUES (%suser_id, %s(%s))
ON CONFLICT(user_id) DO UPDATE SET %s = %s + excluded.%s;`, source.column, prefix, sign, amount(prefix), source.column, source.column, source.column)
			}
			for _, event := range []struct{ name, when, body string }{
				{"INSERT", amount("NEW.") + " <> 0", change("NEW.", "+")},
				{"UPDATE", "OLD.user_id IS NOT NEW.user_id OR (" + amount("OLD.") + ") IS NOT (" + amount("NEW.") + ")", change("OLD.", "-") + change("NEW.", "+")},
				{"DELETE", amount("OLD.") + " <> 0", change("OLD.", "-")},
			} {
				sql := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS total_%s_%s AFTER %s ON %s WHEN %s BEGIN %s END", source.table, event.name, event.name, source.table, event.when, event.body)
				if err := tx.Exec(sql).Error; err != nil {
					return err
				}
			}
		}
		return initializeBusinessTotal(tx, "garden_production_v1", []string{
			`DELETE FROM garden_production_totals`,
			`INSERT INTO garden_production_totals(user_id, harvest_count)
SELECT user_id, SUM(quantity) FROM garden_plantings
WHERE deleted_at IS NULL AND status = 'harvested' AND harvested_at IS NOT NULL AND quantity > 0 GROUP BY user_id`,
			`INSERT INTO garden_production_totals(user_id, alchemy_count)
SELECT user_id, SUM(quantity) FROM garden_alchemy_records WHERE quantity > 0 GROUP BY user_id
ON CONFLICT(user_id) DO UPDATE SET alchemy_count = excluded.alchemy_count`,
		})
	})
}

func qualifyTotalCondition(condition, prefix string) string {
	// The conditions above only contain these column names.
	for _, column := range []string{"deleted_at", "status", "harvested_at", "quantity"} {
		condition = strings.ReplaceAll(condition, column, prefix+column)
	}
	return condition
}

func InitializeSectShopContributionTotals(db *gorm.DB) error {
	created := !db.Migrator().HasTable(&model.SectShopContributionTotal{})
	if err := db.AutoMigrate(&model.SectShopContributionTotal{}, &model.BusinessTotalMigration{}); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if created {
			if err := tx.Where("key = ?", "sect_shop_contribution_v1").Delete(&model.BusinessTotalMigration{}).Error; err != nil {
				return err
			}
		}
		change := func(prefix, sign string) string {
			return fmt.Sprintf(`INSERT INTO sect_shop_contribution_totals(sect_id, user_id, spent)
VALUES (%[1]ssect_id, %[1]suser_id, %[2]s(CASE WHEN %[1]sdeleted_at IS NULL THEN %[1]scost_contribution ELSE 0 END))
ON CONFLICT(sect_id, user_id) DO UPDATE SET spent = spent + excluded.spent;`, prefix, sign)
		}
		for _, event := range []struct{ name, body string }{
			{"INSERT", change("NEW.", "+")},
			{"UPDATE", change("OLD.", "-") + change("NEW.", "+")},
			{"DELETE", change("OLD.", "-")},
		} {
			sql := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS total_sect_shop_%s AFTER %s ON sect_shop_purchases BEGIN %s END", event.name, event.name, event.body)
			if err := tx.Exec(sql).Error; err != nil {
				return err
			}
		}
		return initializeBusinessTotal(tx, "sect_shop_contribution_v1", []string{
			`DELETE FROM sect_shop_contribution_totals`,
			`INSERT INTO sect_shop_contribution_totals(sect_id, user_id, spent)
SELECT sect_id, user_id, SUM(cost_contribution) FROM sect_shop_purchases WHERE deleted_at IS NULL GROUP BY sect_id, user_id`,
		})
	})
}

func initializeBusinessTotal(tx *gorm.DB, key string, statements []string) error {
	var migration model.BusinessTotalMigration
	err := tx.First(&migration, "key = ?", key).Error
	if err == nil {
		return nil
	}
	if err != gorm.ErrRecordNotFound {
		return err
	}
	for _, sql := range statements {
		if err := tx.Exec(sql).Error; err != nil {
			return err
		}
	}
	return tx.Create(&model.BusinessTotalMigration{Key: key}).Error
}

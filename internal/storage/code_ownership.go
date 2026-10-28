package storage

import (
	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

// MigrateTradedCodeOwnership protects previously completed trades too. Legacy
// codes without an exchange owner or a completed trade remain bearer codes.
func MigrateTradedCodeOwnership(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		const key = "migration_traded_code_ownership_v1"
		var count int64
		if err := tx.Model(&model.SystemConfig{}).Where("key = ?", key).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		for _, table := range []string{"invite_codes", "renew_codes"} {
			// Table names are constants above, never user input. The newest sold
			// secret identifies the current buyer after any chain of resales.
			query := `UPDATE ` + table + ` SET owner_user_id = (
 SELECT buyer_id FROM marketplace_secrets s WHERE s.code_hash = ` + table + `.code_hash
 AND s.status = 'sold' AND s.buyer_id <> 0 AND s.deleted_at IS NULL
 ORDER BY s.sold_at DESC, s.id DESC LIMIT 1),
 source = CASE WHEN COALESCE(source, '') = '' THEN 'marketplace' ELSE source END
 WHERE is_used = 0 AND deleted_at IS NULL AND EXISTS (
 SELECT 1 FROM marketplace_secrets s WHERE s.code_hash = ` + table + `.code_hash
 AND s.status = 'sold' AND s.buyer_id <> 0 AND s.deleted_at IS NULL)`
			if err := tx.Exec(query).Error; err != nil {
				return err
			}
		}
		return UpsertSystemConfigValueInTx(tx, key, "complete")
	})
}

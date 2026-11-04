package storage

import "gorm.io/gorm"

func InitializeOrphanAccountDeletionGuards(db *gorm.DB) error {
	for _, sql := range []string{
		`CREATE TRIGGER IF NOT EXISTS orphan_account_binding_insert_guard BEFORE INSERT ON users
		WHEN NEW.abs_user_id <> '' AND NEW.deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM orphan_account_deletions WHERE abs_user_id = NEW.abs_user_id)
		BEGIN SELECT RAISE(ABORT, 'ABS账号正在清理或已删除，请重新验证账号'); END`,
		`CREATE TRIGGER IF NOT EXISTS orphan_account_binding_update_guard BEFORE UPDATE ON users
		WHEN NEW.abs_user_id <> '' AND NEW.deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM orphan_account_deletions WHERE abs_user_id = NEW.abs_user_id)
		BEGIN SELECT RAISE(ABORT, 'ABS账号正在清理或已删除，请重新验证账号'); END`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return nil
}

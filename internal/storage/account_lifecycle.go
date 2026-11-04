package storage

import "gorm.io/gorm"

// SQLite guards cover every writer (cards, compensation, whitelist, rebind),
// including writers outside the scheduler. Comparing values permits unrelated
// wallet changes and the scheduler's no-op write used to acquire a write lock.
func InitializeAccountLifecycleGuards(db *gorm.DB) error {
	for _, sql := range []string{
		`CREATE TRIGGER IF NOT EXISTS account_lifecycle_update_guard BEFORE UPDATE ON users
		WHEN EXISTS (SELECT 1 FROM account_lifecycle_claims WHERE user_row_id = OLD.id)
		AND (NEW.id IS NOT OLD.id OR NEW.telegram_id IS NOT OLD.telegram_id OR NEW.abs_user_id IS NOT OLD.abs_user_id
		 OR NEW.expire_at IS NOT OLD.expire_at OR NEW.is_whitelist IS NOT OLD.is_whitelist
		 OR NEW.is_suspended IS NOT OLD.is_suspended OR NEW.admin_suspended IS NOT OLD.admin_suspended OR NEW.role IS NOT OLD.role
		 OR NEW.status IS NOT OLD.status OR NEW.account_type IS NOT OLD.account_type
		 OR NEW.deleted_at IS NOT OLD.deleted_at)
		BEGIN SELECT RAISE(ABORT, '账号生命周期操作处理中，请稍后重试'); END`,
		`CREATE TRIGGER IF NOT EXISTS account_lifecycle_delete_guard BEFORE DELETE ON users
		WHEN EXISTS (SELECT 1 FROM account_lifecycle_claims WHERE user_row_id = OLD.id)
		BEGIN SELECT RAISE(ABORT, '账号生命周期操作处理中，请稍后重试'); END`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			return err
		}
	}
	return nil
}

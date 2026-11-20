package storage

import (
	"fmt"

	"gorm.io/gorm"
)

// InitializeListeningInputCache keeps the previous-input optimization safe when
// source days are repaired, deleted, restored or transferred outside ABS sync.
func InitializeListeningInputCache(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		clear := func(prefix string) string {
			return fmt.Sprintf(`UPDATE listening_report_snapshots SET official_input_json = ''
WHERE user_id = %suser_id AND official_input_json <> '';`, prefix)
		}
		for _, event := range []struct{ name, clause, body string }{
			{"insert", "INSERT", clear("NEW.")},
			{"delete", "DELETE", clear("OLD.")},
			{"update", `UPDATE OF user_id, abs_user_id, day_key, deleted_at, raw_seconds,
official_raw_seconds, capped_seconds, effective_hours, official_policy,
official_baseline_kind, official_baseline_seconds, official_baseline_hours,
official_baseline_credited_seconds, validation_json, source, fetch_status,
refresh_reason, last_fetched_at`, clear("OLD.") + clear("NEW.")},
		} {
			sql := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS listening_input_%s AFTER %s ON daily_listening_stats BEGIN %s END", event.name, event.clause, event.body)
			if err := tx.Exec(sql).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

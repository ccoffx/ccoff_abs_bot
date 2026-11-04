package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

// Bound memory while preventing two retries from racing an intervening ban.
var userLocks [64]sync.Mutex

// IsSuspended remains true until ABS and the local transaction both succeed.
// It is also the durable retry marker, so a crash or API failure cannot lose
// an expiry-related reactivation. Administrative bans and abuse freezes stay.
func ReactivateExpiredAccount(userID int64) error {
	lock := &userLocks[uint64(userID)%uint64(len(userLocks))]
	lock.Lock()
	defer lock.Unlock()
	var u model.User
	needed := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", userID).Error; err != nil {
			return err
		}
		if err := tx.First(&u, "telegram_id = ?", userID).Error; err != nil {
			return err
		}
		if u.AdminSuspended {
			return fmt.Errorf("管理员封禁须由管理员解除")
		}
		if !u.IsSuspended {
			return nil
		}
		var pending model.AccountLifecycleClaim
		claimErr := tx.First(&pending, "user_row_id = ?", u.ID).Error
		if claimErr != nil && !errors.Is(claimErr, gorm.ErrRecordNotFound) {
			return claimErr
		}
		if claimErr == nil && pending.Kind != "reactivate" {
			return fmt.Errorf("账号生命周期操作处理中，请稍后重试")
		}
		if u.AbsUserID == "" || u.Status != "active" || (!u.IsWhitelist && storage.IsUserExpiredAt(u, time.Now())) {
			// A long ABS outage may outlast the extension. Hand the account back
			// to expiry handling without enabling an already-expired account.
			if claimErr == nil {
				return tx.Delete(&pending).Error
			}
			return nil
		}
		var freezes int64
		if err := tx.Model(&model.ListeningAbuseRecord{}).Where("user_id = ? AND abs_user_id = ? AND action = ? AND status = ?",
			userID, u.AbsUserID, storage.ListeningAbuseActionFreeze, storage.ListeningAbuseStatusActive).Count(&freezes).Error; err != nil {
			return err
		}
		if freezes > 0 {
			return fmt.Errorf("播放异常冻结期间不能恢复账号")
		}
		if errors.Is(claimErr, gorm.ErrRecordNotFound) {
			if err := tx.Create(&model.AccountLifecycleClaim{UserRowID: u.ID, Kind: "reactivate", CreatedAt: time.Now()}).Error; err != nil {
				return err
			}
		}
		needed = true
		return nil
	})
	if err != nil || !needed {
		return err
	}
	if abs.DefaultClient == nil {
		return fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
	}
	// The durable claim guards account changes without holding SQLite's writer
	// lock across network calls. Failures/restarts leave it available for retry.
	if err := abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, true); err != nil {
		return err
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ?", u.ID, "reactivate")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		} // another idempotent retry finalized it
		res = tx.Model(&model.User{}).Where("id = ? AND abs_user_id = ? AND admin_suspended = ?", u.ID, u.AbsUserID, false).
			Updates(map[string]interface{}{"is_suspended": false, "status": "active"})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("REACTIVATION_USER_CHANGED")
		}
		return storage.WriteAuditLogInTx(tx, userID, "REACTIVATE_EXPIRED_USER", fmt.Sprint(userID), 0, "restore expiry suspension after account extension")
	})
}

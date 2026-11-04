package lifecycle

import (
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

const ListeningAbuseReleaseOperation = "abuse_release"

// ReleaseListeningAbuseFreeze reserves the same account guard as expiry
// restoration. An ABS failure leaves both the claim and active record for retry.
func ReleaseListeningAbuseFreeze(recordID uint, now time.Time) (bool, error) {
	var snapshot model.ListeningAbuseRecord
	if err := storage.DB.First(&snapshot, recordID).Error; err != nil {
		return false, err
	}
	lock := &userLocks[uint64(snapshot.UserID)%uint64(len(userLocks))]
	lock.Lock()
	defer lock.Unlock()
	var u model.User
	var record model.ListeningAbuseRecord
	needed := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", snapshot.UserID).Error; err != nil {
			return err
		}
		if err := tx.First(&record, recordID).Error; err != nil {
			return err
		}
		if record.Action != storage.ListeningAbuseActionFreeze || record.Status != storage.ListeningAbuseStatusActive || record.FreezeEndAt == nil || record.FreezeEndAt.After(now) {
			return nil
		}
		userErr := tx.First(&u, "telegram_id = ?", record.UserID).Error
		if userErr != nil && !errors.Is(userErr, gorm.ErrRecordNotFound) {
			return userErr
		}
		if userErr != nil || u.AbsUserID != record.AbsUserID || u.AbsUserID == "" || u.Status != "active" || u.AdminSuspended || u.IsSuspended || (!u.IsWhitelist && storage.IsUserExpiredAt(u, now)) {
			// The expiry time can pass during a prolonged ABS outage. Release our
			// claim together with the blocked record so expiry handling can resume.
			if userErr == nil {
				if err := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ? AND reference_id = ?", u.ID, ListeningAbuseReleaseOperation, record.ID).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&record).Updates(map[string]interface{}{
				"status": storage.ListeningAbuseStatusReleaseBlocked, "released_at": now, "release_error": "account missing, unbound, changed, suspended or expired",
			}).Error; err != nil {
				return err
			}
			return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_RELEASE_BLOCKED", fmt.Sprint(record.UserID), 0, fmt.Sprintf("account cannot be restored; record_id=%d", record.ID))
		}
		var pending model.AccountLifecycleClaim
		claimErr := tx.First(&pending, "user_row_id = ?", u.ID).Error
		if claimErr != nil && !errors.Is(claimErr, gorm.ErrRecordNotFound) {
			return claimErr
		}
		if claimErr == nil && (pending.Kind != ListeningAbuseReleaseOperation || pending.ReferenceID != record.ID) {
			return fmt.Errorf("账号生命周期操作处理中，请稍后重试")
		}
		if errors.Is(claimErr, gorm.ErrRecordNotFound) {
			if err := tx.Create(&model.AccountLifecycleClaim{UserRowID: u.ID, Kind: ListeningAbuseReleaseOperation, ReferenceID: record.ID, CreatedAt: now}).Error; err != nil {
				return err
			}
		}
		needed = true
		return nil
	})
	if err != nil || !needed {
		return false, err
	}
	if abs.DefaultClient == nil {
		return false, fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
	}
	if err := abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, true); err != nil {
		return false, err
	}
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ? AND reference_id = ?", u.ID, ListeningAbuseReleaseOperation, record.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("LISTENING_ABUSE_RELEASE_CLAIM_CHANGED")
		}
		res = tx.Model(&model.ListeningAbuseRecord{}).Where("id = ? AND status = ?", record.ID, storage.ListeningAbuseStatusActive).
			Updates(map[string]interface{}{"status": storage.ListeningAbuseStatusReleased, "released_at": now, "release_error": ""})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("LISTENING_ABUSE_RELEASE_RECORD_CHANGED")
		}
		return storage.WriteAuditLogInTx(tx, 0, "LISTENING_ABUSE_RELEASE", fmt.Sprint(record.UserID), 0, fmt.Sprintf("released abnormal listening freeze; record_id=%d", record.ID))
	})
	return err == nil, err
}

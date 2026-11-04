package app

import (
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Persist intent before the remote call so renewal, rebind and restart cannot
// turn an uncertain ABS deletion into an ordinary usable local account.
func deleteAccountWithAudit(actorID, targetID int64, expectedAbsID, action string, detailBuilder func(model.User) string) error {
	if action != "SELF_DELETE_USER" && action != "FORCE_DELETE_USER" {
		return fmt.Errorf("INVALID_ACCOUNT_DELETION_ACTION")
	}
	var u model.User
	var claim model.AccountLifecycleClaim
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", targetID).Error; err != nil {
			return err
		}
		if err := tx.First(&u, "telegram_id = ?", targetID).Error; err != nil {
			return err
		}
		if storage.IsSuperAdminUser(u) {
			return storage.ErrTargetIsSuperAdmin
		}
		if u.AbsUserID != expectedAbsID {
			return fmt.Errorf("target_abs_user_changed")
		}
		err := tx.First(&claim, "user_row_id = ?", u.ID).Error
		if err == nil {
			if claim.Kind != "delete" || claim.ActorID != actorID || claim.AuditAction != action {
				return fmt.Errorf("账号生命周期操作处理中，请稍后重试")
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		detail := fmt.Sprintf("deleted local user record: username=%s tg=%d abs_user_id=%s", textfmt.FormatPlainValue(u.Username), targetID, textfmt.FormatPlainValue(u.AbsUserID))
		if detailBuilder != nil {
			detail = detailBuilder(u)
		}
		claim = model.AccountLifecycleClaim{UserRowID: u.ID, Kind: "delete", ActorID: actorID, AuditAction: action, AuditDetail: detail, CreatedAt: time.Now()}
		return tx.Create(&claim).Error
	})
	if err != nil {
		return err
	}
	return completeAccountDeletion(u, claim)
}

func completeAccountDeletion(u model.User, claim model.AccountLifecycleClaim) error {
	if storage.IsSuperAdminUser(u) {
		return storage.ErrTargetIsSuperAdmin
	}
	if u.AbsUserID != "" {
		if abs.DefaultClient == nil {
			return fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
		}
		if err := abs.DefaultClient.DeleteUser(u.AbsUserID); err != nil && !abs.IsAbsNotFoundError(err) {
			return err
		}
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ?", u.ID, "delete")
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		action := claim.AuditAction
		var detailBuilder func(model.User) string
		if action == "" {
			action = "AUTO_DELETE_EXPIRED_USER"
		} else {
			detailBuilder = func(model.User) string { return claim.AuditDetail }
		}
		if err := deleteLocalUserWithAuditDB(tx, claim.ActorID, u.TelegramID, u.AbsUserID, action, detailBuilder); err != nil {
			return err
		}
		if u.AbsUserID == "" {
			return nil
		}
		now := time.Now()
		// Keep a tombstone too: a login verified before deletion cannot commit
		// a new binding after this user row has been removed.
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.OrphanAccountDeletion{
			AbsUserID: u.AbsUserID, ActorID: claim.ActorID, CreatedAt: now, CompletedAt: &now,
		}).Error
	})
}

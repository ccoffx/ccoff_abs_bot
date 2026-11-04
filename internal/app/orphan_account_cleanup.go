package app

import (
	"fmt"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Claim the ABS identity under SQLite's writer lock before touching ABS. The
// persistent claim blocks all binding writers, including verified stale logins.
func deleteOrphanAccount(actorID int64, absID string) (bool, error) {
	if absID == "" {
		return false, fmt.Errorf("ABS_USER_ID_EMPTY")
	}
	if config.AppConfig != nil && absID == config.AppConfig.AbsTemplateID {
		return false, nil
	}
	var claim model.OrphanAccountDeletion
	bound := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		claim = model.OrphanAccountDeletion{AbsUserID: absID, ActorID: actorID, CreatedAt: time.Now()}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.User{}).Where("abs_user_id = ?", absID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			bound = true
			return tx.Delete(&model.OrphanAccountDeletion{}, "abs_user_id = ?", absID).Error
		}
		return tx.First(&claim, "abs_user_id = ?", absID).Error
	})
	if err != nil || bound {
		return false, err
	}
	if claim.CompletedAt != nil {
		return true, nil
	}
	if abs.DefaultClient == nil {
		return false, fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
	}
	// Preview may be stale: protect accounts promoted to admin before confirmation.
	user, err := abs.DefaultClient.GetUserSummary(absID)
	if err != nil && !abs.IsAbsNotFoundError(err) {
		return false, err
	}
	if err == nil {
		if user.Type == "root" || user.Type == "admin" {
			return false, storage.DB.Delete(&model.OrphanAccountDeletion{}, "abs_user_id = ? AND completed_at IS NULL", absID).Error
		}
		if err := abs.DefaultClient.DeleteUser(absID); err != nil && !abs.IsAbsNotFoundError(err) {
			return false, err
		}
	}
	res := storage.DB.Model(&model.OrphanAccountDeletion{}).Where("abs_user_id = ?", absID).Update("completed_at", time.Now())
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected != 1 {
		return false, fmt.Errorf("ORPHAN_DELETION_CLAIM_MISSING")
	}
	return true, nil
}

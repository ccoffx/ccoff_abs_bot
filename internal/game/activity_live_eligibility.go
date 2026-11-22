package game

import (
	"errors"
	"strings"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

// Recheck in the publication write transaction: network sampling can outlive a
// binding change or sect departure. Even a failed sample must not retain an
// ineligible account's old damage or guardian bonus.
func activityLiveParticipantEligible(tx *gorm.DB, userID int64, absID string, sectID int64) (bool, error) {
	var user model.User
	err := tx.Select("status", "abs_user_id").Where("telegram_id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.Status != "active" || strings.TrimSpace(absID) == "" || user.AbsUserID != absID {
		return false, nil
	}
	if sectID != 0 {
		var count int64
		if err := tx.Model(&model.SectMember{}).Where("user_id = ? AND sect_id = ?", userID, sectID).Count(&count).Error; err != nil {
			return false, err
		}
		return count == 1, nil
	}
	return true, nil
}

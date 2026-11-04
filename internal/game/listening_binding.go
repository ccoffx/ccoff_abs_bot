package game

import (
	"strings"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

// Acquire the SQLite write lock and validate the identity before persisting
// an ABS response. Rebinding/deletion may finish while the HTTP request runs.
func lockCurrentListeningBindingInTx(tx *gorm.DB, userID int64, absUserID string) error {
	absUserID = strings.TrimSpace(absUserID)
	if userID == 0 || absUserID == "" {
		return ErrListeningBindingChanged
	}
	res := tx.Model(&model.User{}).Where("telegram_id = ? AND abs_user_id = ?", userID, absUserID).
		UpdateColumn("id", gorm.Expr("id"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrListeningBindingChanged
	}
	return nil
}

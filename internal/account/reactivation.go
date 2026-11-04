package account

import (
	"errors"
	"log"
	"time"

	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func ReactivateExpiredAccount(userID int64) error {
	return lifecycle.ReactivateExpiredAccount(userID)
}

func tryReactivateExpiredAccount(userID int64) {
	if err := ReactivateExpiredAccount(userID); err != nil {
		log.Printf("账号延期已到账，ABS 恢复待重试: user=%d err=%v", userID, err)
	}
}

func RetryExpiredAccountReactivations() error {
	var users []model.User
	if err := storage.DB.Where("is_suspended = ? AND admin_suspended = ? AND abs_user_id <> '' AND status = ? AND (is_whitelist = ? OR expire_at IS NULL OR expire_at > ?)",
		true, false, "active", true, time.Now()).Find(&users).Error; err != nil {
		return err
	}
	var failures []error
	for _, u := range users {
		if err := ReactivateExpiredAccount(u.TelegramID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

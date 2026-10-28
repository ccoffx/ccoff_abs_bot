package account

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func BindLocalUserWithAudit(actorID int64, username string, absUserID string, securityCodeHash string, expireAt *time.Time) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username_empty")
	}
	if absUserID == "" {
		return storage.ErrAbsUserIDEmpty
	}
	if securityCodeHash == "" {
		return storage.ErrSecurityPepperNotConfigured
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		err := tx.Where("telegram_id = ?", actorID).First(&target).Error
		switch {
		case err == nil:
			oldUsername := target.Username
			oldAbsUserID := target.AbsUserID
			expireInitialized := target.ExpireAt == nil

			updates := map[string]interface{}{
				"username":      username,
				"abs_user_id":   absUserID,
				"security_code": securityCodeHash,
				"status":        "active",
				"account_type":  storage.AccountTypeFormal,
			}
			if expireInitialized {
				updates["expire_at"] = expireAt
			}

			res := tx.Model(&model.User{}).
				Where("id = ? AND telegram_id = ?", target.ID, actorID).
				Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("target_binding_changed")
			}

			return storage.WriteAuditLogInTx(
				tx,
				actorID,
				"BIND_USER",
				fmt.Sprintf("%d", actorID),
				0,
				fmt.Sprintf("updated local bind record old_username=%s old_abs_user_id=%s new_username=%s new_abs_user_id=%s expire_initialized=%t",
					textfmt.FormatPlainValue(oldUsername), textfmt.FormatPlainValue(oldAbsUserID), textfmt.FormatPlainValue(username), textfmt.FormatPlainValue(absUserID), expireInitialized),
			)
		case errors.Is(err, gorm.ErrRecordNotFound):
			user := model.User{
				TelegramID:   actorID,
				Username:     username,
				AbsUserID:    absUserID,
				SecurityCode: securityCodeHash,
				ExpireAt:     expireAt,
				Status:       "active",
				AccountType:  storage.AccountTypeFormal,
			}
			if err := createBoundLocalUserInTx(tx, &user); err != nil {
				return err
			}

			return storage.WriteAuditLogInTx(
				tx,
				actorID,
				"BIND_USER",
				fmt.Sprintf("%d", actorID),
				0,
				fmt.Sprintf("created local bind record username=%s abs_user_id=%s expire_initialized=%t",
					textfmt.FormatPlainValue(username), textfmt.FormatPlainValue(absUserID), expireAt != nil),
			)
		default:
			return err
		}
	})
}

// renameLocalUsernameWithAudit 在本地档案中把当前用户的用户名改为 newUsername。
// 调用前必须已完成安全码与密码校验，且已在 ABS 服务端成功改名。
// 事务内二次校验所有权与唯一性，避免并发场景下写坏数据。
func RenameLocalUsernameWithAudit(actorID int64, oldUsername string, newUsername string, absUserID string) error {
	newUsername = strings.TrimSpace(newUsername)
	if newUsername == "" {
		return fmt.Errorf("username_empty")
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", actorID).First(&target).Error; err != nil {
			return err
		}
		if strings.TrimSpace(target.AbsUserID) == "" {
			return storage.ErrAbsUserIDEmpty
		}
		if target.Username == newUsername {
			return storage.ErrUsernameUnchanged
		}

		// 唯一性二次校验：用户名是登录凭证且带唯一索引，需排除自身后判断是否被他人占用。
		var conflictCount int64
		if err := tx.Model(&model.User{}).
			Where("username = ? AND telegram_id <> ?", newUsername, actorID).
			Count(&conflictCount).Error; err != nil {
			return err
		}
		if conflictCount > 0 {
			return storage.ErrUsernameTaken
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND username = ?", target.ID, actorID, target.Username).
			Update("username", newUsername)
		if res.Error != nil {
			if storage.IsUniqueConstraintError(res.Error) {
				return storage.ErrUsernameTaken
			}
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("target_username_changed")
		}

		return storage.WriteAuditLogInTx(
			tx,
			actorID,
			"RENAME_USERNAME",
			fmt.Sprintf("%d", actorID),
			0,
			fmt.Sprintf("renamed local username old_username=%s new_username=%s abs_user_id=%s",
				textfmt.FormatPlainValue(oldUsername), textfmt.FormatPlainValue(newUsername), textfmt.FormatPlainValue(absUserID)),
		)
	})
}

func createBoundLocalUserInTx(tx *gorm.DB, user *model.User) error {
	if tx == nil || user == nil {
		return fmt.Errorf("BOUND_USER_INVALID")
	}
	entry := *user
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("BOUND_USER_CREATE_MISSED")
	}
	*user = entry
	return nil
}

func RebindLocalUserWithAudit(actorID int64, targetID uint, expectedAbsUserID string) error {
	if targetID == 0 {
		return storage.ErrUserNotFound
	}
	if expectedAbsUserID == "" {
		return storage.ErrAbsUserIDEmpty
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if target.AbsUserID != expectedAbsUserID {
			return fmt.Errorf("target_abs_user_changed")
		}

		oldTelegramID := target.TelegramID
		res := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND abs_user_id = ?", target.ID, oldTelegramID, expectedAbsUserID).
			Updates(map[string]interface{}{
				"telegram_id":  actorID,
				"status":       "active",
				"account_type": storage.AccountTypeFormal,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("target_binding_changed")
		}

		return storage.WriteAuditLogInTx(
			tx,
			actorID,
			"REBIND_USER",
			fmt.Sprintf("%d", actorID),
			0,
			fmt.Sprintf("rebound local user record username=%s abs_user_id=%s old_telegram_id=%d new_telegram_id=%d",
				textfmt.FormatPlainValue(target.Username), textfmt.FormatPlainValue(target.AbsUserID), oldTelegramID, actorID),
		)
	})
}

func UnbindLocalUserWithAudit(actorID int64, expectedAbsUserID string) error {
	if expectedAbsUserID == "" {
		return storage.ErrAbsUserIDEmpty
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", actorID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if target.AbsUserID != expectedAbsUserID {
			return fmt.Errorf("target_abs_user_changed")
		}

		unboundTelegramID := -actorID
		res := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND abs_user_id = ?", target.ID, actorID, expectedAbsUserID).
			Updates(map[string]interface{}{
				"telegram_id": unboundTelegramID,
				"status":      "unbound",
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("target_binding_changed")
		}

		return storage.WriteAuditLogInTx(
			tx,
			actorID,
			"UNBIND_USER",
			fmt.Sprintf("%d", actorID),
			0,
			fmt.Sprintf("unbound local user record username=%s abs_user_id=%s old_telegram_id=%d new_telegram_id=%d",
				textfmt.FormatPlainValue(target.Username), textfmt.FormatPlainValue(target.AbsUserID), actorID, unboundTelegramID),
		)
	})
}

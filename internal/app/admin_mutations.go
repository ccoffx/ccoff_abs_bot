package app

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

type adminMutationStatus string

const (
	adminMutationOK                 adminMutationStatus = "ok"
	adminMutationSelf               adminMutationStatus = "self"
	adminMutationNotFound           adminMutationStatus = "not_found"
	adminMutationTargetSuperAdmin   adminMutationStatus = "target_super_admin"
	adminMutationAlreadyAdmin       adminMutationStatus = "already_admin"
	adminMutationAlreadyWhitelisted adminMutationStatus = "already_whitelisted"
	adminMutationTargetStateChanged adminMutationStatus = "target_state_changed"
)

func setConfigIntWithAudit(actorID int64, key string, value int, defaultVal int, action string, label string, reason string) (int, error) {
	oldValue := 0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txOldValue, err := storage.GetConfigIntFromDBChecked(tx, key, defaultVal)
		if err != nil {
			return err
		}
		if err := storage.UpsertSystemConfigValueInTx(tx, key, strconv.Itoa(value)); err != nil {
			return err
		}
		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			action,
			key,
			0,
			fmt.Sprintf("%s changed from %d to %d; reason: %s", label, txOldValue, value, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		oldValue = txOldValue
		return nil
	})
	if err != nil {
		return 0, err
	}
	return oldValue, nil
}

func setServerLinesWithAudit(actorID int64, lines string, reason string) (int, int, error) {
	oldLen := 0
	newLen := len([]rune(lines))
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var cfg model.SystemConfig
		err := tx.Where("key = ?", "server_lines").First(&cfg).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		txOldLen := 0
		if err == nil {
			txOldLen = len([]rune(cfg.Value))
		}

		if err := storage.UpsertSystemConfigValueInTx(tx, "server_lines", lines); err != nil {
			return err
		}
		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"SET_SERVER_LINES",
			"server_lines",
			0,
			fmt.Sprintf("server_lines length changed from %d to %d chars; reason: %s", txOldLen, newLen, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		oldLen = txOldLen
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return oldLen, newLen, nil
}

func generateInviteCodesWithAudit(actorID int64, count int, reason string) ([]string, error) {
	if count <= 0 || count > 100 {
		return nil, storage.ErrCreateInviteCodeFailed
	}

	var codes []string
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txCodes := make([]string, 0, count)
		for len(txCodes) < count {
			code, err := createAdminInviteCodeInTx(tx)
			if err != nil {
				return err
			}
			txCodes = append(txCodes, code)
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"GENERATE_INVITE_CODES",
			"invite_codes",
			0,
			fmt.Sprintf("generated invite codes count=%d; reason: %s", count, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		codes = txCodes
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func generateRenewCodesWithAudit(actorID int64, days int, count int, reason string) ([]string, error) {
	if days <= 0 || days > 365 || count <= 0 || count > 100 {
		return nil, storage.ErrCreateRenewCodeFailed
	}

	var codes []string
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txCodes := make([]string, 0, count)
		for len(txCodes) < count {
			code, err := createAdminRenewCodeInTx(tx, days)
			if err != nil {
				return err
			}
			txCodes = append(txCodes, code)
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"GENERATE_RENEW_CODES",
			"renew_codes",
			0,
			fmt.Sprintf("generated renew codes days=%d count=%d; reason: %s", days, count, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		codes = txCodes
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func createAdminInviteCodeInTx(tx *gorm.DB) (string, error) {
	for i := 0; i < 5; i++ {
		code := security.GenerateRandomCode(16)
		if err := storage.CreateInviteCodeRecord(tx, code); err == nil {
			return code, nil
		} else if !storage.IsUniqueConstraintError(err) {
			return "", err
		}
	}
	return "", storage.ErrCreateInviteCodeFailed
}

func createAdminRenewCodeInTx(tx *gorm.DB, days int) (string, error) {
	for i := 0; i < 5; i++ {
		code := fmt.Sprintf("R%d-%s", days, security.GenerateRandomCode(16))
		if err := storage.CreateRenewCodeRecord(tx, code, days); err == nil {
			return code, nil
		} else if !storage.IsUniqueConstraintError(err) {
			return "", err
		}
	}
	return "", storage.ErrCreateRenewCodeFailed
}

func applySuspendLocalStatusWithAudit(actorID int64, targetID int64, expectedAbsUserID string, suspended bool, auditAction string, reason string) error {
	if expectedAbsUserID == "" {
		return storage.ErrAbsUserIDEmpty
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if target.Role == "super_admin" {
			return storage.ErrTargetIsSuperAdmin
		}
		if target.AbsUserID != expectedAbsUserID {
			return fmt.Errorf("target_abs_user_changed")
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND abs_user_id = ? AND role <> ?", target.ID, expectedAbsUserID, "super_admin").
			Update("is_suspended", suspended)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("target_state_changed")
		}

		return storage.WriteAuditLogInTx(
			tx,
			actorID,
			auditAction,
			fmt.Sprintf("%d", targetID),
			0,
			fmt.Sprintf("set user %s(%d) suspended=%t; reason: %s", textfmt.FormatPlainValue(target.Username), targetID, suspended, textfmt.FormatPlainValue(reason)),
		)
	})
}

func deleteLocalUserWithAudit(actorID int64, targetID int64, expectedAbsUserID string, action string, detailBuilder func(model.User) string) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if target.Role == "super_admin" {
			return storage.ErrTargetIsSuperAdmin
		}
		if target.AbsUserID != expectedAbsUserID {
			return fmt.Errorf("target_abs_user_changed")
		}

		deleteRes := tx.Unscoped().
			Where("id = ? AND telegram_id = ? AND abs_user_id = ? AND role <> ?", target.ID, targetID, expectedAbsUserID, "super_admin").
			Delete(&model.User{})
		if deleteRes.Error != nil {
			return deleteRes.Error
		}
		if deleteRes.RowsAffected == 0 {
			return fmt.Errorf("target_state_changed")
		}

		detail := fmt.Sprintf("deleted local user record: username=%s tg=%d abs_user_id=%s", textfmt.FormatPlainValue(target.Username), target.TelegramID, textfmt.FormatPlainValue(target.AbsUserID))
		if detailBuilder != nil {
			detail = detailBuilder(target)
		}
		return storage.WriteAuditLogInTx(tx, actorID, action, fmt.Sprintf("%d", targetID), 0, detail)
	})
}

func promoteAdminWithAudit(actorID int64, targetID int64, reason string) (adminMutationStatus, error) {
	if actorID == targetID {
		return adminMutationSelf, nil
	}

	txStatus := adminMutationOK
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				txStatus = adminMutationNotFound
				return nil
			}
			return err
		}
		switch target.Role {
		case "super_admin":
			txStatus = adminMutationTargetSuperAdmin
			return nil
		case "admin":
			txStatus = adminMutationAlreadyAdmin
			return nil
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND role <> ? AND role <> ?", target.ID, "super_admin", "admin").
			Update("role", "admin")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			txStatus = adminMutationTargetStateChanged
			return nil
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"PROMOTE_ADMIN",
			fmt.Sprintf("%d", targetID),
			0,
			fmt.Sprintf("promoted user %s(%d) to admin; reason: %s", textfmt.FormatPlainValue(target.Username), targetID, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		txStatus = adminMutationOK
		return nil
	})
	if err != nil {
		return adminMutationOK, err
	}
	return txStatus, nil
}

func setWhitelistWithAudit(actorID int64, targetID int64, reason string) (adminMutationStatus, error) {
	if actorID == targetID {
		return adminMutationSelf, nil
	}

	txStatus := adminMutationOK
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				txStatus = adminMutationNotFound
				return nil
			}
			return err
		}
		if target.Role == "super_admin" {
			txStatus = adminMutationTargetSuperAdmin
			return nil
		}
		if target.IsWhitelist {
			txStatus = adminMutationAlreadyWhitelisted
			return nil
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND is_whitelist = ? AND role <> ?", target.ID, false, "super_admin").
			Update("is_whitelist", true)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			txStatus = adminMutationTargetStateChanged
			return nil
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"SET_WHITELIST",
			fmt.Sprintf("%d", targetID),
			0,
			fmt.Sprintf("added user %s(%d) to whitelist; reason: %s", textfmt.FormatPlainValue(target.Username), targetID, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		txStatus = adminMutationOK
		return nil
	})
	if err != nil {
		return adminMutationOK, err
	}
	return txStatus, nil
}

func simulateExpireWithAudit(actorID int64, targetID int64, reason string) (adminMutationStatus, time.Time, error) {
	expireAt := time.Now().AddDate(0, 0, -1)
	if actorID == targetID {
		return adminMutationSelf, expireAt, nil
	}

	txStatus := adminMutationOK
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", targetID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				txStatus = adminMutationNotFound
				return nil
			}
			return err
		}
		if target.Role == "super_admin" {
			txStatus = adminMutationTargetSuperAdmin
			return nil
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND role <> ?", target.ID, "super_admin").
			Updates(map[string]interface{}{"expire_at": expireAt, "is_suspended": false})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			txStatus = adminMutationTargetStateChanged
			return nil
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"SIMULATE_EXPIRE",
			fmt.Sprintf("%d", targetID),
			0,
			fmt.Sprintf("forced user %s(%d) expired; expire_at=%s; reason: %s", textfmt.FormatPlainValue(target.Username), targetID, expireAt.Format(time.RFC3339), textfmt.FormatPlainValue(reason)),
		); err != nil {
			return err
		}
		txStatus = adminMutationOK
		return nil
	})
	if err != nil {
		return adminMutationOK, time.Time{}, err
	}
	return txStatus, expireAt, nil
}

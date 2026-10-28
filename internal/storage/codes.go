package storage

import (
	"fmt"
	"strings"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
)

const (
	RenewCodeSourcePointExchange = "point_exchange"
	CodeSourceMarketplace        = "marketplace"
)

func CheckCodeOwnership(source string, ownerUserID, userID int64) error {
	if ownerUserID != 0 || source == RenewCodeSourcePointExchange || source == CodeSourceMarketplace {
		if ownerUserID == 0 || ownerUserID != userID {
			return ErrCodeOwnerMismatch
		}
	}
	return nil
}

func CreateRenewCodeRecord(tx *gorm.DB, rawCode string, days int) error {
	return CreateRenewCodeRecordWithMeta(tx, rawCode, days, "", 0)
}

func CreateRenewCodeRecordWithMeta(tx *gorm.DB, rawCode string, days int, source string, ownerUserID int64) error {
	if tx == nil {
		tx = DB
	}

	codeHash := security.HashSensitiveToken(rawCode)
	if codeHash == "" {
		return ErrSecurityPepperNotConfigured
	}

	res := tx.Create(&model.RenewCode{
		CodeHash:    codeHash,
		CodePreview: security.MaskSecret(rawCode),
		Days:        days,
		Source:      strings.TrimSpace(source),
		OwnerUserID: ownerUserID,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("RENEW_CODE_CREATE_MISSED")
	}
	return nil
}

func CreateInviteCodeRecord(tx *gorm.DB, rawCode string) error {
	return CreateInviteCodeRecordWithMeta(tx, rawCode, "", 0)
}

func CreateInviteCodeRecordWithMeta(tx *gorm.DB, rawCode, source string, ownerUserID int64) error {
	if tx == nil {
		tx = DB
	}

	codeHash := security.HashSensitiveToken(rawCode)
	if codeHash == "" {
		return ErrSecurityPepperNotConfigured
	}

	res := tx.Create(&model.InviteCode{
		CodeHash:    codeHash,
		CodePreview: security.MaskSecret(rawCode),
		Source:      strings.TrimSpace(source),
		OwnerUserID: ownerUserID,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("INVITE_CODE_CREATE_MISSED")
	}
	return nil
}

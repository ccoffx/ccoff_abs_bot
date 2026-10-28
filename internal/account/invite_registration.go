package account

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func ReserveInviteCodeForRegistrationWithAudit(userID int64, inviteHash string) (model.InviteCode, error) {
	if inviteHash == "" {
		return model.InviteCode{}, storage.ErrInvalidInviteCode
	}

	var invite model.InviteCode
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var txInvite model.InviteCode
		if err := tx.Where("code_hash = ? AND is_used = ?", inviteHash, false).First(&txInvite).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrInvalidInviteCode
			}
			return err
		}

		if err := storage.CheckCodeOwnership(txInvite.Source, txInvite.OwnerUserID, userID); err != nil {
			return err
		}
		res := tx.Model(&model.InviteCode{}).
			Where("id = ? AND is_used = ?", txInvite.ID, false).
			Updates(map[string]interface{}{
				"is_used":    true,
				"used_by_id": userID,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrInvalidInviteCode
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"RESERVE_INVITE_CODE",
			fmt.Sprintf("invite_code_id=%d", txInvite.ID),
			0,
			fmt.Sprintf("user %d reserved invite code %s for registration", userID, textfmt.FormatPlainValue(txInvite.CodePreview)),
		); err != nil {
			return err
		}
		invite = txInvite
		return nil
	})
	if err != nil {
		return model.InviteCode{}, err
	}
	return invite, nil
}

func ReleaseInviteCodeReservationWithAudit(userID int64, inviteHash string, reason string) error {
	if inviteHash == "" {
		return storage.ErrInvalidInviteCode
	}

	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var invite model.InviteCode
		if err := tx.Where("code_hash = ? AND is_used = ? AND used_by_id = ?", inviteHash, true, userID).First(&invite).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrInvalidInviteCode
			}
			return err
		}

		res := tx.Model(&model.InviteCode{}).
			Where("id = ? AND is_used = ? AND used_by_id = ?", invite.ID, true, userID).
			Updates(map[string]interface{}{
				"is_used":    false,
				"used_by_id": 0,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrInvalidInviteCode
		}

		return storage.WriteAuditLogInTx(
			tx,
			userID,
			"RELEASE_INVITE_CODE",
			fmt.Sprintf("invite_code_id=%d", invite.ID),
			0,
			fmt.Sprintf("released invite code %s after registration compensation; reason: %s", textfmt.FormatPlainValue(invite.CodePreview), textfmt.FormatPlainValue(reason)),
		)
	})
}

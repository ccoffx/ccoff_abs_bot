package market

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const codeCashoutPercent = 60

type CodeCashoutQuote struct {
	Kind     string
	RecordID uint
	Hash     string
	Preview  string
	Name     string
	Points   int
}

func InspectCodeCashout(rawCode string, userID int64) (CodeCashoutQuote, error) {
	rawCode = strings.TrimSpace(rawCode)
	codeHash := security.HashSensitiveToken(rawCode)
	if codeHash == "" {
		return CodeCashoutQuote{}, storage.ErrSecurityPepperNotConfigured
	}

	var quote CodeCashoutQuote
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var invite model.InviteCode
		if err := tx.Where("code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", codeHash, false).First(&invite).Error; err == nil {
			if err := storage.CheckCodeOwnership(invite.Source, invite.OwnerUserID, userID); err != nil {
				return err
			}
			points, err := codeCashoutPointsInTx(tx, "invite", 0)
			if err != nil {
				return err
			}
			quote = CodeCashoutQuote{
				Kind:     "invite",
				RecordID: invite.ID,
				Hash:     codeHash,
				Preview:  invite.CodePreview,
				Name:     "邀请码",
				Points:   points,
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var renew model.RenewCode
		if err := tx.Where("code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", codeHash, false).First(&renew).Error; err == nil {
			if err := storage.CheckCodeOwnership(renew.Source, renew.OwnerUserID, userID); err != nil {
				return err
			}
			points, err := codeCashoutPointsInTx(tx, "renew", renew.Days)
			if err != nil {
				return err
			}
			quote = CodeCashoutQuote{
				Kind:     "renew",
				RecordID: renew.ID,
				Hash:     codeHash,
				Preview:  renew.CodePreview,
				Name:     fmt.Sprintf("%d 天续期卡", renew.Days),
				Points:   points,
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return ErrCodeCashoutInvalid
	})
	if err != nil {
		return CodeCashoutQuote{}, err
	}
	if quote.Preview == "" {
		quote.Preview = security.MaskSecret(rawCode)
	}
	return quote, nil
}

func codeCashoutPointsInTx(tx *gorm.DB, kind string, days int) (int, error) {
	var originalValue int
	switch kind {
	case "invite":
		price, err := storage.GetConfigIntFromDBChecked(tx, "invite_price", 300)
		if err != nil {
			return 0, err
		}
		originalValue = price
	case "renew":
		if days <= 0 {
			return 0, ErrCodeCashoutInvalid
		}
		price, err := storage.GetConfigIntFromDBChecked(tx, "renew_price", 150)
		if err != nil {
			return 0, err
		}
		originalValue = (price*days + 29) / 30
	default:
		return 0, ErrCodeCashoutInvalid
	}
	points := calculateCodeCashoutPoints(originalValue)
	if originalValue <= 0 || points <= 0 {
		return 0, ErrCodeCashoutPriceInvalid
	}
	return points, nil
}

func calculateCodeCashoutPoints(originalValue int) int {
	if originalValue <= 0 {
		return 0
	}
	return originalValue * codeCashoutPercent / 100
}

func ExecuteCodeCashout(userID int64, quote CodeCashoutQuote) (int, error) {
	if userID == 0 || quote.RecordID == 0 || strings.TrimSpace(quote.Hash) == "" || quote.Points <= 0 {
		return 0, ErrCodeCashoutInvalid
	}

	awarded := 0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		currentPoints := 0
		now := time.Now()
		switch quote.Kind {
		case "invite":
			var invite model.InviteCode
			if err := tx.Where("id = ? AND code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", quote.RecordID, quote.Hash, false).First(&invite).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrCodeCashoutInvalid
				}
				return err
			}
			if err := storage.CheckCodeOwnership(invite.Source, invite.OwnerUserID, userID); err != nil {
				return err
			}
			points, err := codeCashoutPointsInTx(tx, "invite", 0)
			if err != nil {
				return err
			}
			currentPoints = points
			if currentPoints != quote.Points {
				return ErrCodeCashoutPriceChanged
			}
			res := tx.Model(&model.InviteCode{}).
				Where("id = ? AND code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", invite.ID, quote.Hash, false).
				Updates(map[string]interface{}{
					"is_used":          true,
					"used_by_id":       userID,
					"cashed_out_at":    &now,
					"cashed_out_by_id": userID,
					"cashout_points":   currentPoints,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrCodeCashoutInvalid
			}
		case "renew":
			var renew model.RenewCode
			if err := tx.Where("id = ? AND code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", quote.RecordID, quote.Hash, false).First(&renew).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrCodeCashoutInvalid
				}
				return err
			}
			if err := storage.CheckCodeOwnership(renew.Source, renew.OwnerUserID, userID); err != nil {
				return err
			}
			points, err := codeCashoutPointsInTx(tx, "renew", renew.Days)
			if err != nil {
				return err
			}
			currentPoints = points
			if currentPoints != quote.Points {
				return ErrCodeCashoutPriceChanged
			}
			res := tx.Model(&model.RenewCode{}).
				Where("id = ? AND code_hash = ? AND is_used = ? AND cashed_out_at IS NULL", renew.ID, quote.Hash, false).
				Updates(map[string]interface{}{
					"is_used":          true,
					"used_by_id":       userID,
					"cashed_out_at":    &now,
					"cashed_out_by_id": userID,
					"cashout_points":   currentPoints,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrCodeCashoutInvalid
			}
		default:
			return ErrCodeCashoutInvalid
		}

		if err := closeCashedOutMarketplaceUnitsInTx(tx, quote.Hash); err != nil {
			return err
		}
		refID := fmt.Sprintf("%s:%d", quote.Kind, quote.RecordID)
		if err := storage.ApplyPointDeltaInTx(
			tx,
			userID,
			currentPoints,
			"code_cashout",
			fmt.Sprintf("回收本服%s %s", quote.Name, textfmt.FormatPlainValue(quote.Preview)),
			"code_cashout",
			refID,
		); err != nil {
			return err
		}
		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"CASHOUT_CODE",
			refID,
			currentPoints,
			fmt.Sprintf("回收本服%s，preview=%s", quote.Name, textfmt.FormatPlainValue(quote.Preview)),
		); err != nil {
			return err
		}
		awarded = currentPoints
		return nil
	})
	if err != nil {
		return 0, err
	}
	return awarded, nil
}

func closeCashedOutMarketplaceUnitsInTx(tx *gorm.DB, codeHash string) error {
	var units []model.MarketplaceSecret
	if err := tx.Select("id", "listing_id").
		Where("code_hash = ? AND status = ?", codeHash, marketplaceSecretAvailable).
		Find(&units).Error; err != nil {
		return err
	}
	if len(units) == 0 {
		return nil
	}

	listingIDs := make(map[uint]struct{}, len(units))
	for _, unit := range units {
		res := tx.Model(&model.MarketplaceSecret{}).
			Where("id = ? AND code_hash = ? AND status = ?", unit.ID, codeHash, marketplaceSecretAvailable).
			Update("status", marketplaceSecretClosed)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrCodeCashoutInvalid
		}
		listingIDs[unit.ListingID] = struct{}{}
	}
	for listingID := range listingIDs {
		var remaining int64
		if err := tx.Model(&model.MarketplaceSecret{}).
			Where("listing_id = ? AND status = ?", listingID, marketplaceSecretAvailable).
			Count(&remaining).Error; err != nil {
			return err
		}
		if remaining == 0 {
			if err := tx.Model(&model.MarketplaceListing{}).
				Where("id = ? AND status = ?", listingID, marketplaceStatusActive).
				Update("status", marketplaceStatusClosed).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

package app

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
)

// Progress keyed only by Telegram ID must not become a new account's record.
// Keep event summaries and title history; soft-delete source rows where supported.
func clearDeletedAccountRankSourcesInTx(tx *gorm.DB, userID int64) error {
	if err := removeDeletedAccountSectMembershipInTx(tx, userID); err != nil {
		return err
	}
	for _, source := range []any{
		&model.Cultivation{},
		&model.DailyListeningStat{},
		&model.ABSCrossDaySessionScanCursor{},
		&model.ABSCrossDaySessionAdjustment{},
		&model.GardenPlanting{},
		&model.GardenAlchemyRecord{},
		&model.GardenPlot{},
		&model.GardenRecipeUnlock{},
		&model.Inventory{},
		&model.GardenSeedPurchase{},
		&model.GardenHerbMarketSale{},
		&model.SectCaveRetreat{},
		&model.SectOwnerBonusRevision{},
		&model.WorldBossParticipant{},
	} {
		if err := tx.Where("user_id = ?", userID).Delete(source).Error; err != nil {
			return fmt.Errorf("clear deleted account ranking source %T: %w", source, err)
		}
	}
	return nil
}

func removeDeletedAccountSectMembershipInTx(tx *gorm.DB, userID int64) error {
	var sects []model.Sect
	if err := tx.Where("owner_id = ? OR id IN (?)", userID,
		tx.Model(&model.SectMember{}).Select("sect_id").Where("user_id = ?", userID)).Find(&sects).Error; err != nil {
		return err
	}
	if err := tx.Unscoped().Where("user_id = ?", userID).Delete(&model.SectMember{}).Error; err != nil {
		return err
	}
	for _, sect := range sects {
		updates := map[string]any{}
		if sect.OwnerID == userID {
			var successor model.SectMember
			err := tx.Model(&model.SectMember{}).
				Joins("JOIN users ON users.telegram_id = sect_members.user_id AND users.deleted_at IS NULL").
				Where("sect_members.sect_id = ?", sect.ID).
				Order("CASE WHEN sect_members.role = 'elder' THEN 0 ELSE 1 END").
				Order("sect_members.contribution DESC, sect_members.joined_at ASC, sect_members.id ASC").
				Take(&successor).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := tx.Unscoped().Where("sect_id = ?", sect.ID).Delete(&model.SectMember{}).Error; err != nil {
					return err
				}
				if err := tx.Delete(&sect).Error; err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if err := tx.Model(&successor).Update("role", "owner").Error; err != nil {
				return err
			}
			updates["owner_id"] = successor.UserID
			updates["owner_name"] = successor.UserName
			if err := sectbonus.Schedule(tx, successor.UserID, true, time.Now()); err != nil {
				return err
			}
		}
		var count int64
		if err := tx.Model(&model.SectMember{}).Where("sect_id = ?", sect.ID).Count(&count).Error; err != nil {
			return err
		}
		updates["member_count"] = count
		if err := tx.Model(&sect).Updates(updates).Error; err != nil {
			return err
		}
	}
	return nil
}

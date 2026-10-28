package game

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

const (
	cultivationTreasureTokenCost         = 1
	cultivationTreasureExchangeTTL       = 15 * time.Minute
	CultivationTreasureExchangePending   = "pending"
	CultivationTreasureExchangeCompleted = "completed"
)

func cultivationTreasureExchangeOffer(database *gorm.DB, userID int64) (*model.Cultivation, string, int, error) {
	var cul model.Cultivation
	if err := database.Where("user_id = ?", userID).First(&cul).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", 0, ErrCultivationTreasureNoProfile
		}
		return nil, "", 0, err
	}
	name := CultivationTreasureNameForRealm(cul.MajorRealm)
	if name == "" {
		return &cul, "", 0, ErrCultivationTreasureUnavailable
	}
	var token model.Inventory
	err := database.Where("user_id = ? AND item_name = ?", userID, SectSecretRealmTokenItemName).First(&token).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", 0, err
	}
	return &cul, name, token.Quantity, nil
}

func PrepareCultivationTreasureExchange(database *gorm.DB, userID int64) (*model.CultivationTreasureExchange, error) {
	cul, name, tokens, err := cultivationTreasureExchangeOffer(database, userID)
	if err != nil {
		return nil, err
	}
	if tokens < cultivationTreasureTokenCost {
		return nil, ErrCultivationTreasureTokens
	}
	exchange := model.CultivationTreasureExchange{UserID: userID, FromMajorRealm: cul.MajorRealm,
		TreasureName: name, TokenCost: cultivationTreasureTokenCost, Status: CultivationTreasureExchangePending}
	if err := database.Create(&exchange).Error; err != nil {
		return nil, err
	}
	return &exchange, nil
}

// 确认单认领、境界校验、扣信物、发至宝、写审计在同一事务内完成。
// 重复按钮、并发回调或失败重试均不会重复扣款或重复发奖。
func ConfirmCultivationTreasureExchange(database *gorm.DB, userID int64, exchangeID uint) (*model.CultivationTreasureExchange, error) {
	var exchange model.CultivationTreasureExchange
	err := database.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.CultivationTreasureExchange{}).
			Where("id = ? AND user_id = ? AND status = ? AND created_at >= ?", exchangeID, userID,
				CultivationTreasureExchangePending, time.Now().Add(-cultivationTreasureExchangeTTL)).
			Update("status", CultivationTreasureExchangeCompleted)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var old model.CultivationTreasureExchange
			err := tx.Where("id = ? AND user_id = ?", exchangeID, userID).First(&old).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil && old.Status == CultivationTreasureExchangeCompleted {
				return ErrCultivationTreasureCompleted
			}
			return ErrCultivationTreasureExpired
		}
		if err := tx.First(&exchange, exchangeID).Error; err != nil {
			return err
		}
		cul, name, _, err := cultivationTreasureExchangeOffer(tx, userID)
		if err != nil {
			return err
		}
		if cul.MajorRealm != exchange.FromMajorRealm || name != exchange.TreasureName {
			return ErrCultivationTreasureRealmChanged
		}
		if exchange.TokenCost != cultivationTreasureTokenCost {
			return ErrCultivationTreasureExpired
		}
		res = tx.Model(&model.Inventory{}).
			Where("user_id = ? AND item_name = ? AND quantity >= ?", userID, SectSecretRealmTokenItemName, exchange.TokenCost).
			UpdateColumn("quantity", gorm.Expr("quantity - ?", exchange.TokenCost))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrCultivationTreasureTokens
		}
		if err := GardenGrantInventoryInTx(tx, userID, exchange.TreasureName, 1); err != nil {
			return err
		}
		return storage.WriteAuditLogInTx(tx, userID, "cultivation_treasure_exchange", fmt.Sprintf("exchange:%d", exchange.ID), 0,
			fmt.Sprintf("秘境信物 -%d，%s +1，突破前大境界 %d", exchange.TokenCost, exchange.TreasureName, exchange.FromMajorRealm))
	})
	if err != nil {
		return nil, err
	}
	return &exchange, nil
}

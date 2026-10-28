package storage

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/textfmt"
)

// 返回值：currentPool(当前进度), isBurst(是否触发了300分爆包)
func AddPointsToFusionPool(pointsToAdd int) (int, bool) {
	currentPool, isBurst, err := AddPointsToFusionPoolWithError(pointsToAdd)
	if err != nil {
		log.Printf("⚠️ 天道奖池注水失败: points=%d err=%s", pointsToAdd, textfmt.FormatPlainError(err))
		return 0, false
	}

	return currentPool, isBurst
}

func AddPointsToFusionPoolWithError(pointsToAdd int) (int, bool, error) {
	currentPool := 0
	isBurst := false
	err := RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		if pointsToAdd <= 0 {
			var poolCfg model.SystemConfig
			if err := tx.Where("key = ?", "fusion_pool_points").First(&poolCfg).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			parsedPool, err := strconv.Atoi(strings.TrimSpace(poolCfg.Value))
			if err != nil {
				return err
			}
			currentPool = parsedPool
			return nil
		}

		var err error
		currentPool, isBurst, err = AddPointsToFusionPoolInTx(tx, pointsToAdd)
		return err
	})
	if err != nil {
		return 0, false, err
	}

	return currentPool, isBurst, nil
}

func RunFusionPoolLockedTransaction(fn func(tx *gorm.DB) error) error {
	if fn == nil {
		return fmt.Errorf("FUSION_POOL_TX_EMPTY")
	}

	fusionPoolMutex.Lock()
	defer fusionPoolMutex.Unlock()

	return DB.Transaction(fn)
}

func createFusionPoolConfigIfMissingInTx(tx *gorm.DB) error {
	if tx == nil {
		return fmt.Errorf("FUSION_POOL_CONFIG_INVALID")
	}
	cfg := model.SystemConfig{Key: "fusion_pool_points", Value: "0"}
	res := tx.Clauses(SystemConfigKeyDoNothingClause()).Create(&cfg)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	return nil
}

// addPointsToFusionPoolInTx assumes the caller already holds fusionPoolMutex.
func AddPointsToFusionPoolInTx(tx *gorm.DB, pointsToAdd int) (int, bool, error) {
	if tx == nil {
		return 0, false, fmt.Errorf("DB_TX_EMPTY")
	}
	if pointsToAdd <= 0 {
		return 0, false, nil
	}

	var poolCfg model.SystemConfig
	err := tx.Where("key = ?", "fusion_pool_points").First(&poolCfg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := createFusionPoolConfigIfMissingInTx(tx); err != nil {
				return 0, false, err
			}
			if err := tx.Where("key = ?", "fusion_pool_points").First(&poolCfg).Error; err != nil {
				return 0, false, err
			}
		} else {
			return 0, false, err
		}
	}

	currentPool, err := strconv.Atoi(strings.TrimSpace(poolCfg.Value))
	if err != nil {
		return 0, false, fmt.Errorf("invalid fusion pool points value=%s: %w", textfmt.FormatPlainValue(poolCfg.Value), err)
	}

	currentPool += pointsToAdd
	isBurst := false
	for currentPool >= 300 {
		currentPool -= 300
		isBurst = true

		redID := "FS-" + security.GenerateRandomCode(6)
		packet := model.RedPacket{
			ID:          redID,
			SenderID:    0,
			SenderName:  "✨ 天道灵气",
			TotalPoints: 300,
			Count:       30,
			LeftCount:   30,
			LeftPoints:  300,
			CreatedAt:   time.Now(),
		}
		if err := CreateRedPacketInTx(tx, &packet); err != nil {
			return 0, false, err
		}
	}

	poolRes := tx.Model(&model.SystemConfig{}).
		Where("id = ? AND key = ?", poolCfg.ID, "fusion_pool_points").
		Update("value", fmt.Sprintf("%d", currentPool))
	if poolRes.Error != nil {
		return 0, false, poolRes.Error
	}
	if poolRes.RowsAffected == 0 {
		return 0, false, fmt.Errorf("FUSION_POOL_STATE_CHANGED")
	}

	return currentPool, isBurst, nil
}

var fusionPoolMutex sync.Mutex

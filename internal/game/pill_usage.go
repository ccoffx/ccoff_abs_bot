package game

import (
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func GetManualPillUsageConfig(itemName string, t time.Time) (periodStart time.Time, periodKey string, maxCount int, cycleName string, addHours float64, ok bool) {
	loc := time.FixedZone("CST", 8*3600)
	now := t.In(loc)

	switch itemName {
	case "回气丹":
		maxCount = 3
		cycleName = "本周"
		addHours = 1.0
	case "青灵养气丹":
		maxCount = 2
		cycleName = "本周"
		addHours = 3.0
	case "万载灵髓":
		maxCount = 1
		cycleName = "本月"
		addHours = 10.0
	default:
		return time.Time{}, "", 0, "", 0, false
	}

	if itemName == "万载灵髓" {
		periodStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		periodKey = fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month()))
	} else {
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		todayZero := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		periodStart = todayZero.AddDate(0, 0, -(weekday - 1))
		isoYear, isoWeek := now.ISOWeek()
		periodKey = fmt.Sprintf("%04d-W%02d", isoYear, isoWeek)
	}

	return periodStart, periodKey, maxCount, cycleName, addHours, true
}

func CountManualPillUsage(userID int64, itemName string, periodStart time.Time) (int64, error) {
	var usedCount int64
	err := storage.DB.Model(&model.ItemUsageLog{}).
		Where("user_id = ? AND item_name = ? AND used_at >= ?", userID, itemName, periodStart).
		Count(&usedCount).Error
	return usedCount, err
}

func ManualPillUsageCountText(usedCount int64, maxCount int, available bool) string {
	if !available {
		return fmt.Sprintf("读取失败/%d", maxCount)
	}
	return fmt.Sprintf("%d/%d", usedCount, maxCount)
}

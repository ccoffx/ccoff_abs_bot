package activity

import (
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func lotteryUserActivityLabel(activityID uint) string {
	var activity model.LotteryActivity
	if err := storage.DB.Select("title").First(&activity, activityID).Error; err != nil {
		return "抽奖"
	}
	return "抽奖《" + textfmt.LotteryDisplayText(activity.Title, 80, "-") + "》"
}

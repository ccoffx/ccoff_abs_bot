package game

import (
	"fmt"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
)

var SectOwnerCultivationPerkText = fmt.Sprintf("👑 **宗主专享 · 每日基础净修为 +%d%%**（身份变更次日生效）", sectbonus.Percent)

func SectOwnerDailyBonusLine(stat model.DailyListeningStat) string {
	if stat.SectOwnerBonusHours <= 0 {
		return ""
	}
	return fmt.Sprintf("\n👑 **宗主专享 +%d%%**：额外修为 `+%.3f` 小时", sectbonus.Percent, stat.SectOwnerBonusHours)
}

package titles

import (
	"errors"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/textfmt"
)

func rankTitlePeriodText(key, period string) string {
	if key == rankings.BossTitle {
		return "统计范围：最近两场已结算 Boss\n"
	}
	if period == "" {
		return ""
	}
	switch key {
	case rankings.ListeningTitle:
		return "统计月份：" + period + "（北京时间）\n"
	default:
		return ""
	}
}

func renderRankTitlesInTx(tx *gorm.DB, b *strings.Builder, rows *[][]tgbotapi.InlineKeyboardButton, chatID, userID int64, ranking bool) error {
	b.WriteString("🏆 榜首荣誉 · 全服唯一\n随榜单和宗主身份转移；并列保留原第一，Boss 称号仅在每次结算后更新。\n\n")
	for _, d := range RankDefinitions {
		fmt.Fprintf(b, "%s %s｜%s\n", d.Emoji, d.Name, d.Description)
		var holder model.RankTitleHolder
		if err := tx.Where("title_key = ?", d.Key).Take(&holder).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if holder.UserID > 0 {
			fmt.Fprintf(b, "现任：%s\n", textfmt.LotteryDisplayText(holder.UserName, 24, "道友"))
			switch d.Key {
			case rankings.ListeningTitle:
				fmt.Fprintf(b, "当前纪录：%.2f 小时（不含丹药、闭关和异火加成）\n", holder.Score)
			case rankings.BossTitle:
				fmt.Fprintf(b, "当前纪录：%.2f 有效伤害（仅已结算场次）\n", holder.Score)
			case rankings.HarvestTitle:
				fmt.Fprintf(b, "当前纪录：%.0f 株药草\n", holder.Score)
			case rankings.AlchemyTitle:
				fmt.Fprintf(b, "当前纪录：%.0f 颗成丹\n", holder.Score)
			}
		} else {
			b.WriteString("当前空缺\n")
		}
		b.WriteString(rankTitlePeriodText(d.Key, holder.PeriodKey))
		if !ranking {
			if holder.UserID == userID {
				b.WriteString("我的荣誉：当前持有，可自主佩戴\n")
				if rows != nil && chatID < 0 {
					*rows = append(*rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("佩戴 · "+d.Name, fmt.Sprintf("titles:equip:%d:%s", chatID, d.Key))))
				}
			} else {
				var count int64
				if err := tx.Model(&model.RankTitleHistory{}).Where("title_key = ? AND user_id = ?", d.Key, userID).Count(&count).Error; err != nil {
					return err
				}
				if count > 0 {
					b.WriteString("我的荣誉：曾经获得（当前不可佩戴）\n")
				} else {
					b.WriteString("我的荣誉：尚未获得\n")
				}
			}
		}
		b.WriteByte('\n')
	}
	return nil
}

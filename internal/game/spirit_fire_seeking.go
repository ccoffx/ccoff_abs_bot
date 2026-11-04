package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func spiritFireSeekingPanel(user int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	var remaining int64
	var wallet model.SpiritFireWallet
	var u model.User
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		if err := tx.Model(&model.SpiritFire{}).Where("owner_id = 0 AND listing_id = 0 AND rank > 1").Count(&remaining).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", user).First(&wallet).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Select("telegram_id", "points").Where("telegram_id = ?", user).First(&u).Error
	})
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	text := fmt.Sprintf("🔥 异火寻缘\n\n当前积分：%d\n未认主异火：%d/22\n焚诀古卷：%d本｜残页：%d张\n累计寻缘及古卷购买消耗：%d积分\n\n每次%d积分，十次%d积分，不限次数。\n每次必得残页×1；独立有%d%%概率额外获得古卷×1、%d%%概率获得一朵未认主异火，两者可同时获得。异火从未认主池等概率选取，每种全服唯一。\n每%d张残页可合成古卷×1，残页和古卷均绑定，不可交易或回收。\n\n确认前需备足本次总额；十连中途奖池耗尽会停止，只扣实际次数费用。无异火保底，不自动续抽。\n需有效且未暂停的斗师及以上正式听书账号。\n\n发送：异火寻缘 1 / 异火寻缘 10\n购买焚诀 1 / 购买焚诀 10（每本%d积分）\n合成焚诀（每次合成一本）\n操作均先展示价格，确认后执行。", u.Points, remaining, wallet.Scrolls, wallet.Fragments, wallet.SeekingSpent, spiritfire.SeekingCost, 10*spiritfire.SeekingCost, spiritfire.SeekingScrollChance, spiritfire.SeekingFireChance, spiritfire.FragmentsPerScroll, spiritfire.ScrollBuyCost)
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if remaining > 0 {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("寻缘一次 · 100积分", "menu:cmd:fire_seek_one"), tgbotapi.NewInlineKeyboardButtonData("寻缘十次 · 1000积分", "menu:cmd:fire_seek_ten")))
	} else {
		text += "\n\n⏸ 未认主异火已耗尽，寻缘暂停；可购买古卷，异火回归天地后恢复寻缘。"
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("购买古卷 · 500积分", "menu:cmd:fire_scroll_buy"), tgbotapi.NewInlineKeyboardButtonData("10残页合卷", "menu:cmd:fire_scroll_merge")))
	return text, tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

func seekingQuoteText(q model.SpiritFireQuote) string {
	detail := fmt.Sprintf("寻缘%d次，最多消耗%d积分（需备足总额）。\n每次必得残页×1；独立10%%获得古卷×1、1%%获得未认主异火，无异火保底。\n奖池耗尽会停止，仅扣实际次数费用。", q.Quantity, q.Points)
	if q.Action == "scroll_buy" {
		detail = fmt.Sprintf("购买焚诀古卷%d本，消耗%d积分。古卷绑定，不可交易或回收。", q.Quantity, q.Points)
	}
	if q.Action == "scroll_merge" {
		detail = "消耗焚诀残页10张，合成绑定古卷1本，不消耗积分。"
	}
	return fmt.Sprintf("请确认：\n%s\n确认单15分钟有效，重复确认不会重复扣费。\n回复：确认异火 %s", detail, q.ID)
}

func seekingReceipt(user int64, key string) (string, error) {
	var q model.SpiritFireQuote
	if err := storage.DB.Where("id = ? AND user_id = ?", key, user).First(&q).Error; err != nil {
		return "", err
	}
	if q.Action != "seek" && q.Action != "scroll_buy" && q.Action != "scroll_merge" {
		return "", nil
	}
	var result spiritfire.SeekingResult
	if err := json.Unmarshal([]byte(q.ResultJSON), &result); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ 操作已完成（重复确认仅展示原结果）\n实际次数：%d｜消耗积分：%d\n焚诀残页：%+d张｜古卷：+%d本\n", result.Count, result.Points, result.Fragments, result.Scrolls)
	for _, f := range result.Fires {
		fmt.Fprintf(&b, "🔥 获得异火 #%d %s\n", f.ID, f.Name)
	}
	if q.Action == "seek" && result.Count < q.Quantity {
		b.WriteString("未认主异火已耗尽，其余次数未执行、未扣费。\n")
	}
	b.WriteString("发送“异火寻缘”查看余额与库存；发送“我的异火”选择启用。")
	return b.String(), nil
}

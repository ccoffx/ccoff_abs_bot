package telegram

import (
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/textfmt"
)

func TreasureShopBackMarkup() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回聚宝斋", "shop:home"),
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "menu:home"),
		),
	)
}

func EditMenuPanel(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string, markup tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &markup
	if _, err := bot.Send(PrepareCopyable(edit)); err != nil {
		log.Printf("编辑二级菜单失败: chat=%d message=%d err=%s", chatID, messageID, textfmt.FormatTelegramSendError(err))
	}
}

package game

import (
	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func replyBackgroundSubmission(bot *tgbotapi.BotAPI, chatID int64, result background.Result, name string) {
	switch result {
	case background.Accepted:
		// Accepted tasks report their final result from the worker.
		return
	case background.Pending:
		telegram.ReplyText(bot, chatID, "⏳ "+name+"已在处理中，请勿重复提交。")
	default:
		telegram.ReplyText(bot, chatID, "⏳ 后台任务较多，请稍后重新提交。")
	}
}

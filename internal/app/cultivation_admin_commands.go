package app

import (
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func HandleCultivationAdminReadOnlyCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, text string) bool {
	if text != "查看斗气配置" && text != "查看突破配置" && text != "查看境界配置" {
		return false
	}

	if message == nil || message.From == nil {
		return true
	}

	telegram.RegisterIncomingGroupCommandForAutoDelete(message)

	if !isCultivationConfigAdmin(message.From.ID) {
		telegram.SendPlainText(bot, message.Chat.ID, "你没有权限查看斗气配置。")
		return true
	}

	var reply string
	switch text {
	case "查看斗气配置":
		reply = game.FormatCultivationConfigSummary()
	case "查看突破配置":
		reply = game.FormatBreakthroughConfigs()
	case "查看境界配置":
		reply = game.FormatCultivationRealmConfigs()
	default:
		return false
	}

	telegram.SendLongPlainText(bot, message.Chat.ID, reply)
	return true
}

func isCultivationConfigAdmin(userID int64) bool {
	return storage.IsSuperAdmin(userID)
}

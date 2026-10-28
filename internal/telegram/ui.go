package telegram

import (
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/textfmt"
)

const (
	UserMenuGardenText = "🌱 药园"
)

// 用户主菜单
var UserMainMenu = tgbotapi.NewReplyKeyboard(
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("📋 我的档案"),
		tgbotapi.NewKeyboardButton("📆 每日修行"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("🏪 聚宝斋"),
		tgbotapi.NewKeyboardButton(UserMenuGardenText),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("📿 斗气"),
		tgbotapi.NewKeyboardButton("🏯 宗门"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("🎮 活动"),
		tgbotapi.NewKeyboardButton("📚 求书中心"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("🧾 资产交易"),
		tgbotapi.NewKeyboardButton("⚙️ 账号服务"),
	),
)

func userMainMenuReplyMarkup() interface{} {
	return UserMainMenu
}

// 超级管理员菜单
var SuperAdminMenu = tgbotapi.NewReplyKeyboard(
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("📊 系统总览"),
		tgbotapi.NewKeyboardButton("👤 用户管理"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("💰 资产账务"),
		tgbotapi.NewKeyboardButton("🎫 卡密续期"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("🎲 活动运营"),
		tgbotapi.NewKeyboardButton("📚 求书工单"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("📿 斗气配置"),
		tgbotapi.NewKeyboardButton("🛡 安全运维"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("⚙️ 系统配置"),
		tgbotapi.NewKeyboardButton("🔙 返回用户菜单"),
	),
)

// 普通管理员菜单：只保留只读和低风险能力。
// 高危操作统一收回超级管理员。
var NormalAdminMenu = tgbotapi.NewReplyKeyboard(
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("📊 管理总览"),
		tgbotapi.NewKeyboardButton("📚 求书工单"),
	),
	tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton("🧾 流水查询"),
		tgbotapi.NewKeyboardButton("🔙 返回用户菜单"),
	),
)

func SendMenu(bot *tgbotapi.BotAPI, chatID int64, text string, isMenu interface{}) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = isMenu

	if _, err := SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送 Telegram 菜单失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(err))
		return err
	}
	return nil
}

func SendUserMainMenu(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	return SendMenu(bot, chatID, text, userMainMenuReplyMarkup())
}

func ReplyText(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"

	if _, err := SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送 Telegram 消息失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(err))
	}
}

// replyTextToMessage 在群聊中引用用户原始消息回复，方便多人并发操作时确认反馈归属；
// 私聊保持普通回复，引用发送失败时退回普通发送，避免反馈丢失。
func ReplyTextToMessage(bot *tgbotapi.BotAPI, sourceMsg *tgbotapi.Message, text string) {
	if bot == nil || sourceMsg == nil || sourceMsg.Chat == nil {
		return
	}
	if sourceMsg.Chat.IsPrivate() {
		ReplyText(bot, sourceMsg.Chat.ID, text)
		return
	}
	msg := tgbotapi.NewMessage(sourceMsg.Chat.ID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyToMessageID = sourceMsg.MessageID

	if _, err := SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送 Telegram 引用回复失败: chat=%d message=%d err=%s", sourceMsg.Chat.ID, sourceMsg.MessageID, textfmt.FormatTelegramSendError(err))
		ReplyText(bot, sourceMsg.Chat.ID, text)
	}
}

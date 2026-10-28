package telegram

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const botMessageAutoDeleteDelay = 10 * time.Minute

func createAutoDeleteMessageRecord(chatID int64, messageID int, deleteAt time.Time) error {
	if storage.DB == nil || chatID == 0 || messageID == 0 {
		return nil
	}

	res := storage.DB.Create(&model.AutoDeleteMsg{
		ChatID:    chatID,
		MessageID: messageID,
		DeleteAt:  deleteAt,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("AUTO_DELETE_MSG_CREATE_MISSED")
	}
	return nil
}

func registerAutoDeleteMessage(chatID int64, messageID int) {
	if chatID == 0 || messageID == 0 {
		return
	}

	if err := createAutoDeleteMessageRecord(chatID, messageID, time.Now().Add(botMessageAutoDeleteDelay)); err != nil {
		log.Printf("⚠️ 登记自动删除消息失败: chat=%d message=%d err=%s", chatID, messageID, textfmt.FormatPlainError(err))
	}
}

func RegisterIncomingGroupCommandForAutoDelete(msg *tgbotapi.Message) {
	if msg == nil || msg.Chat == nil {
		return
	}

	// 私聊消息不删除
	if msg.Chat.IsPrivate() {
		return
	}

	registerAutoDeleteMessage(msg.Chat.ID, msg.MessageID)
}

func SendAutoDelete(bot *tgbotapi.BotAPI, chattable tgbotapi.Chattable) (tgbotapi.Message, error) {
	sentMsg, err := bot.Send(chattable)
	if err == nil && sentMsg.Chat.ID < 0 {
		registerAutoDeleteMessage(sentMsg.Chat.ID, sentMsg.MessageID)
	}
	return sentMsg, err
}

func SendNoAutoDelete(bot *tgbotapi.BotAPI, chattable tgbotapi.Chattable) (tgbotapi.Message, error) {
	return bot.Send(chattable)
}

func EnqueueAutoDelete(bot *tgbotapi.BotAPI, chattable tgbotapi.Chattable, kind string, priority telegramAsyncPriority, dedupeKey string) bool {
	return EnqueueTelegramAsync(TelegramAsyncJob{
		Kind:      kind,
		DedupeKey: dedupeKey,
		Priority:  priority,
		Send: func() error {
			_, err := SendAutoDelete(bot, chattable)
			return err
		},
	})
}

func EnqueueNoAutoDelete(bot *tgbotapi.BotAPI, chattable tgbotapi.Chattable, kind string, priority telegramAsyncPriority, dedupeKey string) bool {
	return EnqueueTelegramAsync(TelegramAsyncJob{
		Kind:      kind,
		DedupeKey: dedupeKey,
		Priority:  priority,
		Send: func() error {
			_, err := SendNoAutoDelete(bot, chattable)
			return err
		},
	})
}

func SendPlainText(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := SendAutoDelete(bot, msg); err != nil {
		log.Printf("发送 Telegram 文本消息失败: %s", textfmt.FormatTelegramSendError(err))
	}
}

func IsTelegramMessageNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}

func isTerminalTelegramDeleteError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "message to delete not found") ||
		strings.Contains(errText, "message can't be deleted") ||
		strings.Contains(errText, "not enough rights") ||
		strings.Contains(errText, "not enough rights to delete messages")
}

func IsTerminalTelegramUnpinError(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "message to unpin not found") ||
		strings.Contains(errText, "message not found") ||
		strings.Contains(errText, "message can't be unpinned") ||
		strings.Contains(errText, "message is not pinned") ||
		strings.Contains(errText, "not enough rights") ||
		strings.Contains(errText, "not enough rights to manage pinned messages")
}

func SendLongPlainText(bot *tgbotapi.BotAPI, chatID int64, text string) {
	const telegramMessageLimit = 3800

	runes := []rune(text)
	if len(runes) <= telegramMessageLimit {
		msg := tgbotapi.NewMessage(chatID, text)
		if _, err := SendAutoDelete(bot, msg); err != nil {
			log.Printf("发送 Telegram 消息失败: err=%s", textfmt.FormatTelegramSendError(err))
		}
		return
	}

	for start := 0; start < len(runes); start += telegramMessageLimit {
		end := start + telegramMessageLimit
		if end > len(runes) {
			end = len(runes)
		}

		msg := tgbotapi.NewMessage(chatID, string(runes[start:end]))
		if _, err := SendAutoDelete(bot, msg); err != nil {
			log.Printf("发送 Telegram 长消息失败: err=%s", textfmt.FormatTelegramSendError(err))
			return
		}
	}
}

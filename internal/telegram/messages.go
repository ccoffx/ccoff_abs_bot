package telegram

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf16"

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
	sentMsg, err := bot.Send(PrepareCopyable(chattable))
	if err == nil && sentMsg.Chat.ID < 0 {
		registerAutoDeleteMessage(sentMsg.Chat.ID, sentMsg.MessageID)
	}
	return sentMsg, err
}

func SendNoAutoDelete(bot *tgbotapi.BotAPI, chattable tgbotapi.Chattable) (tgbotapi.Message, error) {
	return bot.Send(PrepareCopyable(chattable))
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

// Only replace an existing panel when Telegram confirms it cannot be edited.
// A timeout, rate limit or invalid replacement text does not invalidate it.
func IsTelegramMessageUnavailableForEdit(err error) bool {
	var apiErr *tgbotapi.Error
	var apiValue tgbotapi.Error
	code := 0
	if errors.As(err, &apiErr) {
		code = apiErr.Code
	} else if errors.As(err, &apiValue) {
		code = apiValue.Code
	}
	if code != 400 {
		return false
	}
	description := strings.ToLower(err.Error())
	return strings.Contains(description, "message to edit not found") ||
		strings.Contains(description, "message can't be edited") ||
		strings.Contains(description, "message can not be edited")
}

// A Telegram 4xx rejection confirms that the send was not accepted. Transport
// errors and server errors can happen after publication, so resending is unsafe.
func IsDefiniteTelegramSendFailure(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 400 && apiErr.Code < 500
	}
	var apiValue tgbotapi.Error
	return errors.As(err, &apiValue) && apiValue.Code >= 400 && apiValue.Code < 500
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
	for _, part := range SplitMessageText(text) {
		msg := tgbotapi.NewMessage(chatID, part)
		if _, err := SendAutoDelete(bot, msg); err != nil {
			log.Printf("发送 Telegram 长消息失败: err=%s", textfmt.FormatTelegramSendError(err))
			return
		}
	}
}

// Prefer line boundaries so card codes and commands remain copyable in one
// piece. Reopen explicit copyable spans when a long block crosses messages.
// Telegram entity offsets and limits use UTF-16 units; emoji can use two.
func SplitMessageText(text string) []string {
	const limit = 3798 // Leave room for a reopened marker and a closing marker.
	parts := []string{}
	copyable := false
	for len(text) > 0 {
		cut, newline, units := len(text), 0, 0
		for offset, r := range text {
			size := utf16.RuneLen(r)
			if units+size > limit {
				cut = offset
				if newline > 0 {
					cut = newline
				}
				break
			}
			units += size
			if r == '\n' {
				newline = offset + 1
			}
		}
		part := text[:cut]
		var out strings.Builder
		if copyable {
			out.WriteByte('\x02')
		}
		out.WriteString(part)
		for _, r := range part {
			if r == '\x02' {
				copyable = true
			} else if r == '\x03' {
				copyable = false
			}
		}
		if copyable {
			out.WriteByte('\x03')
		}
		parts = append(parts, out.String())
		text = text[cut:]
	}
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

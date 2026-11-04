package telegram

import (
	"log"
	"strings"
	"sync/atomic"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/textfmt"
)

const callbackAlertTextMaxRunes = 200

func FormatCallbackAlertText(text string) string {
	formatted := textfmt.FormatDiagnosticTextForDisplay(text)
	if formatted == "" {
		return "操作已处理"
	}
	runes := []rune(formatted)
	if len(runes) <= callbackAlertTextMaxRunes {
		return formatted
	}
	if callbackAlertTextMaxRunes <= 3 {
		return string(runes[:callbackAlertTextMaxRunes])
	}
	return string(runes[:callbackAlertTextMaxRunes-3]) + "..."
}

func AnswerCallback(bot *tgbotapi.BotAPI, callbackID string, text string) {
	markCallbackAnswered(callbackID)
	cb := tgbotapi.NewCallback(callbackID, FormatCallbackAlertText(text))
	if _, err := bot.Request(cb); err != nil && !IsOldTelegramCallbackError(err) {
		log.Printf("⚠️ 回答 callback 失败: err=%s", textfmt.FormatTelegramSendError(err))
	}
}

func markCallbackAnswered(callbackID string) {
	if strings.TrimSpace(callbackID) == "" {
		return
	}
	stateValue, ok := CallbackAckStates.Load(callbackID)
	if !ok {
		return
	}
	if state, ok := stateValue.(*atomic.Bool); ok {
		state.Store(true)
	}
}

func IsOldTelegramCallbackError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "query is too old") ||
		strings.Contains(text, "response timeout expired") ||
		strings.Contains(text, "query id is invalid")
}

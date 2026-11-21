package telegram

import (
	"encoding/json"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestEmptyInlineKeyboardEncodesAsArray(t *testing.T) {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, markup := range []tgbotapi.InlineKeyboardMarkup{NewInlineKeyboardMarkup(), NewInlineKeyboardMarkup(rows...)} {
		data, err := json.Marshal(markup)
		if err != nil || string(data) != `{"inline_keyboard":[]}` {
			t.Fatalf("Telegram button removal requires an empty array: %s %v", data, err)
		}
	}
}

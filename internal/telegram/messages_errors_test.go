package telegram

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestOnlyConfirmedUnavailableMessagesCanBeReplaced(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		unavailable bool
	}{
		{"no error", nil, false},
		{"timeout", context.DeadlineExceeded, false},
		{"untyped error", errors.New("message to edit not found"), false},
		{"rate limit", &tgbotapi.Error{Code: 429, Message: "Too Many Requests"}, false},
		{"server error", &tgbotapi.Error{Code: 502, Message: "Bad Gateway"}, false},
		{"invalid text", &tgbotapi.Error{Code: 400, Message: "can't parse entities"}, false},
		{"unchanged", &tgbotapi.Error{Code: 400, Message: "message is not modified"}, false},
		{"missing pointer", &tgbotapi.Error{Code: 400, Message: "message to edit not found"}, true},
		{"uneditable value", tgbotapi.Error{Code: 400, Message: "message can't be edited"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			if err != nil {
				err = fmt.Errorf("update panel: %w", err)
			}
			if got := IsTelegramMessageUnavailableForEdit(err); got != tc.unavailable {
				t.Fatalf("unavailable=%t want=%t error=%v", got, tc.unavailable, err)
			}
		})
	}
}

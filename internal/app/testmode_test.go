package app

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSandboxFiltersBeforeStartFastPathAndCallbacks(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{AppEnv: "test", AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previous })
	message := func(user, chat int64, kind string) *tgbotapi.Message {
		return &tgbotapi.Message{From: &tgbotapi.User{ID: user}, Chat: &tgbotapi.Chat{ID: chat, Type: kind}, Text: "/start"}
	}
	for _, msg := range []*tgbotapi.Message{message(2, 2, "private"), message(1, -100, "supergroup"), message(1, 2, "private")} {
		update := tgbotapi.Update{Message: msg}
		if testUpdateAllowed(update) {
			t.Fatal("unauthorized message accepted")
		}
		dispatchTelegramUpdate(nil, nil, update)
	}
	if !testUpdateAllowed(tgbotapi.Update{Message: message(1, 1, "private")}) {
		t.Fatal("admin rejected")
	}
	for _, msg := range []*tgbotapi.Message{message(1, -100, "supergroup"), message(1, 2, "private"), nil} {
		if testUpdateAllowed(tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: 1}, Message: msg}}) {
			t.Fatal("unsafe callback accepted")
		}
	}
}

func TestSandboxRejectsChannelMemberEvents(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{AppEnv: "test", AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previous })
	if testUpdateAllowed(tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{Chat: tgbotapi.Chat{ID: -100200, Type: "channel"}}}) {
		t.Fatal("test accepted channel event")
	}
}

func TestSandboxOutboundFilterPreservesBody(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{AppEnv: "test", AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previous })
	for _, id := range []string{"1", "2", "-100", "@channel"} {
		body := url.Values{"chat_id": {id}, "text": {"test"}}.Encode()
		req, _ := http.NewRequest("POST", "https://api.telegram.org/bottest/sendMessage", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		err := guardTestTelegramRequest(req)
		if (id == "1") != (err == nil) {
			t.Fatalf("destination %s: %v", id, err)
		}
		got, _ := io.ReadAll(req.Body)
		if string(got) != body {
			t.Fatal("body consumed")
		}
	}
}

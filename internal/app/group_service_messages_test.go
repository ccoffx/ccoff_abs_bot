package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccoff-abs-bot/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func configureGroupServiceTest(t *testing.T) {
	t.Helper()
	previous := config.AppConfig
	config.AppConfig = &config.Config{NoticeGroupID: -100}
	t.Cleanup(func() { config.AppConfig = previous })
}

func TestGroupServiceDeleteUsesOuterMessageAndPropagatesFailure(t *testing.T) {
	configureGroupServiceTest(t)
	deletions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/bottest/getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if r.URL.Path != "/bottest/deleteMessage" || r.Form.Get("chat_id") != "-100" || r.Form.Get("message_id") != "42" {
			t.Errorf("wrong deletion target: %s %v", r.URL.Path, r.Form)
		}
		deletions++
		if deletions > 1 {
			fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: not enough rights"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, PinnedMessage: &tgbotapi.Message{MessageID: 7}}
	job, ok := groupServiceDeleteJob(bot, msg)
	if !ok {
		t.Fatal("pin service message was ignored")
	}
	if err := job.Send(); err != nil {
		t.Fatal(err)
	}
	if err := job.Send(); err == nil {
		t.Fatal("permission failure hidden from the dispatcher")
	}
}

func TestGroupServiceDispatchBypassesPersonAndBusinessGates(t *testing.T) {
	configureGroupServiceTest(t)
	for _, kind := range []string{"join", "leave", "pin"} {
		t.Run(kind, func(t *testing.T) {
			msg := &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}}
			switch kind {
			case "join":
				msg.From = &tgbotapi.User{ID: 99, IsBot: true}
				msg.NewChatMembers = []tgbotapi.User{{ID: 1}}
			case "leave":
				msg.LeftChatMember = &tgbotapi.User{ID: 1}
			case "pin":
				msg.SenderChat = msg.Chat
				msg.PinnedMessage = &tgbotapi.Message{MessageID: 7}
			}
			if _, ok := groupServiceDeleteJob(nil, msg); !ok {
				t.Fatal("non-person service message was ignored")
			}
			jobs := make(chan telegramMessageJob, 1)
			dispatchTelegramUpdate(nil, jobs, tgbotapi.Update{Message: msg})
			if len(jobs) != 0 {
				t.Fatal("service message entered business queue")
			}
		})
	}
}

func TestGroupServiceCleanupPreservesOtherMessagesAndSandbox(t *testing.T) {
	configureGroupServiceTest(t)
	for _, msg := range []*tgbotapi.Message{
		nil,
		{MessageID: 42},
		{MessageID: 42, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "正常聊天或置顶原文"},
		{MessageID: 42, Chat: &tgbotapi.Chat{ID: -200, Type: "supergroup"}, LeftChatMember: &tgbotapi.User{ID: 1}},
		{MessageID: 42, Chat: &tgbotapi.Chat{ID: -100, Type: "channel"}, PinnedMessage: &tgbotapi.Message{MessageID: 7}},
		{MessageID: 42, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, PinnedMessage: &tgbotapi.Message{MessageID: 7}},
	} {
		if _, ok := groupServiceDeleteJob(nil, msg); ok {
			t.Fatalf("unrelated message selected for deletion: %+v", msg)
		}
	}
	config.AppConfig.AppEnv = "test"
	// The sandbox must reject the update before cleanup can issue any API call.
	dispatchTelegramUpdate(nil, nil, tgbotapi.Update{Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, LeftChatMember: &tgbotapi.User{ID: 1}}})
}

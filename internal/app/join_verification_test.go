package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/joinverify"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestChannelVerificationGatesMessagesAndHistoricCallbacks(t *testing.T) {
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.JoinVerification{}, &model.JoinVerificationEvent{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	previous := joinVerification
	t.Cleanup(func() { joinVerification = previous })
	config.AppConfig.NoticeGroupID = -100
	var mu sync.Mutex
	muted, following := false, false
	var privateVerificationEntries []string
	acks := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		defer mu.Unlock()
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 999, "is_bot": true, "first_name": "test", "username": "testbot"}
		case "getChatMember":
			if r.Form.Get("chat_id") == "-200" {
				status := "left"
				if following {
					status = "member"
				}
				result = map[string]any{"status": status}
			} else {
				status := "member"
				if muted {
					status = "restricted"
				}
				result = map[string]any{"status": status, "is_member": true}
			}
		case "getChat":
			result = map[string]any{"type": "supergroup", "permissions": map[string]bool{"can_send_messages": true}}
		case "restrictChatMember":
			muted = strings.Contains(r.Form.Get("permissions"), `"can_send_messages":false`)
		case "sendMessage":
			if r.Form.Get("chat_id") == "1" && strings.Contains(r.Form.Get("reply_markup"), "?start=jv_") {
				privateVerificationEntries = append(privateVerificationEntries, r.Form.Get("reply_markup"))
			}
			result = map[string]any{"message_id": 123, "chat": map[string]any{"id": 1, "type": "private"}}
		case "answerCallbackQuery":
			acks <- r.Form.Get("text")
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	joinVerification = joinverify.New(storage.DB, bot, -100, -200, 999, "https://t.me/example", nil, "testbot")
	// Discussion forwards and chat identities must never become verification
	// subjects or business commands, even when Telegram supplies a fake From.
	for _, msg := range []*tgbotapi.Message{
		{From: &tgbotapi.User{ID: 777000, IsBot: true}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "积分抽奖"},
		{From: &tgbotapi.User{ID: 1087968824}, SenderChat: &tgbotapi.Chat{ID: -200, Type: "channel"}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "积分抽奖"},
		{From: &tgbotapi.User{ID: 777000}, IsAutomaticForward: true, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "积分抽奖"},
		{From: &tgbotapi.User{ID: 1087968824}, SenderChat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "积分抽奖"},
	} {
		jobs := make(chan telegramMessageJob, 1)
		dispatchTelegramUpdate(bot, jobs, tgbotapi.Update{Message: msg})
		if len(jobs) != 0 {
			t.Fatal("non-user message entered business queue")
		}
		handleInteractiveMessage(bot, msg)
	}
	var nonUserRecords int64
	if err := storage.DB.Model(&model.JoinVerification{}).Count(&nonUserRecords).Error; err != nil || nonUserRecords != 0 {
		t.Fatalf("non-user message triggered verification: count=%d err=%v", nonUserRecords, err)
	}
	telegram.ClearSession(1)
	defer telegram.ClearSession(1)
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "/start"})
	var count int64
	storage.DB.Model(&model.JoinVerification{}).Count(&count)
	if count != 0 {
		t.Fatal("plain /start should keep onboarding available")
	}
	// Non-text messages cannot bypass a business session's verification gate.
	telegram.GetSession(1).SetStep("WAITING_REG_USER")
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}})
	if telegram.GetSession(1).GetStep() != "WAITING_REG_USER" {
		t.Fatal("non-text message entered business handling")
	}
	telegram.ClearSession(1)
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "注册账户"})
	var record model.JoinVerification
	if err = storage.DB.First(&record, "user_id = ?", 1).Error; err != nil || record.State != "pending" {
		t.Fatalf("message bypassed verification: %+v %v", record, err)
	}
	if telegram.GetSession(1).GetStep() != "IDLE" {
		t.Fatal("unverified message entered business session")
	}
	mu.Lock()
	entryCount := len(privateVerificationEntries)
	mu.Unlock()
	if entryCount != 2 {
		t.Fatalf("blocked private messages must include verification buttons, got %d", entryCount)
	}
	send := func(data string, chatID int64) string {
		t.Helper()
		dispatchTelegramUpdate(bot, nil, tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{ID: fmt.Sprintf("cb-%d", time.Now().UnixNano()), From: &tgbotapi.User{ID: 1}, Data: data, Message: &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: chatID, Type: "private"}}}})
		var ack string
		select {
		case ack = <-acks:
		case <-time.After(3 * time.Second):
			t.Fatal("callback was not acknowledged")
		}
		unlock := telegram.LockUser(1)
		unlock() // Wait for callback defers before restoring test globals.
		return ack
	}
	if got := send("menu:account", 1); !strings.Contains(got, "验证") {
		t.Fatalf("historic panel bypassed gate: %s", got)
	}
	if got := send("lottery_enter:1:1", 1); !strings.Contains(got, "验证") {
		t.Fatalf("lottery confirmation bypassed follow gate: %s", got)
	}
	mu.Lock()
	entryCount = len(privateVerificationEntries)
	mu.Unlock()
	if entryCount != 4 {
		t.Fatalf("blocked historic callbacks must send verification buttons, got %d", entryCount)
	}
	mu.Lock()
	following = true
	mu.Unlock()
	if got := send(joinverify.CallbackPrefix+record.Round, -100); !strings.Contains(got, "私聊") {
		t.Fatalf("legacy group callback was accepted: %s", got)
	}
	start := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "/start jv_" + record.Round}
	if handlePrivateStartFastPath(bot, start) {
		t.Fatal("verification start consumed by main menu fast path")
	}
	handleInteractiveMessage(bot, start)
	if got := send(joinverify.CallbackPrefix+record.Round, 1); !strings.Contains(got, "验证通过") {
		t.Fatalf("verification callback could not bypass business gate: %s", got)
	}
}

package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/books"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPendingBookReplyDoesNotConsumeMenusAndCommands(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1) })
	telegram.ClearSession(1)
	if err := storage.DB.AutoMigrate(&model.BookRequest{}, &model.BookRequestLog{}); err != nil {
		t.Fatal(err)
	}
	request := model.BookRequest{UserID: 1, Status: books.BookRequestStatusNeedInfo, UserNote: "原说明", LastActionAt: time.Now()}
	if err := storage.DB.Create(&request).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	for _, command := range []string{"📋 我的档案", "📆 每日修行", "📿 斗气", "🏯 宗门", "🎮 活动", "📚 求书中心", "🧾 资产交易", "⚙️ 账号服务", "天道奖池"} {
		msg.Text = command
		handleInteractiveMessage(bot, msg)
		if err := storage.DB.First(&request, request.ID).Error; err != nil {
			t.Fatal(err)
		}
		if request.Status != books.BookRequestStatusNeedInfo || request.UserNote != "原说明" {
			t.Fatalf("command %q was consumed as a book-request reply: %+v", command, request)
		}
	}
	msg.Text = "补充专辑的完整名称"
	handleInteractiveMessage(bot, msg)
	if err := storage.DB.First(&request, request.ID).Error; err != nil {
		t.Fatal(err)
	}
	if request.Status != "claimed" || !strings.Contains(request.UserNote, msg.Text) {
		t.Fatalf("ordinary reply was not saved: %+v", request)
	}
}

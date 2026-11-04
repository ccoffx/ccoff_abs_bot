package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func TestGroupSignInCommandsShareDailyDeduplication(t *testing.T) {
	setupTreasureTestDB(t)
	config.AppConfig.NoticeGroupID = 0
	previousVerification := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previousVerification })
	if err := storage.DB.AutoMigrate(
		&model.SignInStreak{}, &model.SignInLog{}, &model.SignInRewardClaim{},
		&model.Cultivation{}, &model.Sect{}, &model.SectMember{}, &model.SectContributionLog{},
	); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, r.Form.Get("text"))
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":-100,"type":"supergroup"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{MessageID: 41, From: &tgbotapi.User{ID: 1, FirstName: "Group`Name"}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, Text: "签到"}
	handleInteractiveMessage(bot, msg)
	var signCount int64
	if err := storage.DB.Model(&model.SignInLog{}).Where("user_id = ?", 1).Count(&signCount).Error; err != nil || signCount != 1 {
		t.Fatalf("group sign-in missing: count=%d err=%v", signCount, err)
	}
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points <= 100 {
		t.Fatalf("group sign-in did not award points: %+v %v", user, err)
	}
	firstBalance := user.Points
	msg.MessageID = 42
	msg.Text = "每日签到"
	handleInteractiveMessage(bot, msg)
	if err := storage.DB.Model(&model.SignInLog{}).Where("user_id = ?", 1).Count(&signCount).Error; err != nil || signCount != 1 {
		t.Fatalf("second group command repeated sign-in: count=%d err=%v", signCount, err)
	}
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != firstBalance {
		t.Fatalf("second group command awarded points again: %d %v", user.Points, err)
	}
	var incomingDeletes int64
	if err := storage.DB.Model(&model.AutoDeleteMsg{}).Where("chat_id = ? AND message_id IN ?", -100, []int{41, 42}).Count(&incomingDeletes).Error; err != nil || incomingDeletes != 2 {
		t.Fatalf("group commands were not registered for cleanup: count=%d err=%v", incomingDeletes, err)
	}
	mu.Lock()
	joined := strings.Join(sent, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "签到成功") || !strings.Contains(joined, "已经打过卡") {
		t.Fatalf("group did not receive sign-in and duplicate replies: %q", joined)
	}
	if !strings.Contains(joined, "👤 道友："+textfmt.MarkdownCode("Group`Name")) || strings.Contains(joined, "\x02") || strings.Contains(joined, "\x03") {
		t.Fatalf("display name broke Telegram reply formatting: %q", joined)
	}
	msg.From.ID = 2
	msg.MessageID = 43
	msg.Text = "我想签到"
	handleInteractiveMessage(bot, msg)
	if err := storage.DB.Model(&model.SignInLog{}).Where("user_id = ?", 2).Count(&signCount).Error; err != nil || signCount != 0 {
		t.Fatalf("non-command group text signed in: count=%d err=%v", signCount, err)
	}
}

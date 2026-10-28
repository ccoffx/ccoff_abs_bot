package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSandboxPrivateCommandsSeedOnceAndControlMock(t *testing.T) {
	setupTreasureTestDB(t)
	config.AppConfig.AppEnv = "test"
	config.AppConfig.AdminIDs = map[int64]bool{99: true}
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	mock, err := abs.OpenMockTransport(filepath.Join(t.TempDir(), "mock.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := abs.DefaultClient
	abs.DefaultClient = &abs.AbsClient{BaseURL: "https://abs.mock.invalid", Token: "mock", HttpClient: &http.Client{Transport: mock}}
	t.Cleanup(func() { abs.DefaultClient = previous })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":99,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	message := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 99, Type: "private"}, From: &tgbotapi.User{ID: 99}}
	for _, text := range []string{"/test seed", "/test seed", "/test listening 2026-10-28 2.5", "/test failure 500"} {
		message.Text = text
		if !handleTestCommand(bot, message) {
			t.Fatal("test command not handled")
		}
	}
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", 99).First(&user).Error; err != nil || user.Points != 1000 || !security.VerifySensitiveToken("TestSecurity123!", user.SecurityCode) {
		t.Fatalf("seed changed: %+v %v", user, err)
	}
	if _, code, _ := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(user.AbsUserID), nil); code != 500 {
		t.Fatal("fault not injected")
	}
	message.Text = "/test failure off"
	handleTestCommand(bot, message)
	if body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(user.AbsUserID), nil); err != nil || code != 200 || !strings.Contains(string(body), "9000") {
		t.Fatalf("listening data missing: %s %v", body, err)
	}
	config.AppConfig.AppEnv = "production"
	message.Text = "/test seed"
	if handleTestCommand(bot, message) {
		t.Fatal("test command enabled in production")
	}
}

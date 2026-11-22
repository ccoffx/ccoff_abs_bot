package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func newCommandEntryTestBot(t *testing.T) (*tgbotapi.BotAPI, func() []string, func()) {
	t.Helper()
	var mu sync.Mutex
	var replies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			fmt.Fprint(w, `{"ok":true,"result":true}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if raw := r.Form.Get("reply_markup"); raw != "" {
			var markup map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &markup); err != nil {
				t.Error(err)
			} else if keyboard, ok := markup["inline_keyboard"]; ok && (len(keyboard) == 0 || keyboard[0] != '[') {
				t.Errorf("Telegram rejects a non-array keyboard: %s", raw)
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"field inline_keyboard must be of type Array"}`)
				return
			}
		}
		mu.Lock()
		if text := r.Form.Get("text"); text != "" {
			replies = append(replies, text)
		}
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), replies...)
		}, func() {
			mu.Lock()
			defer mu.Unlock()
			replies = nil
		}
}

func TestParameterizedCommandsShowUsageWithoutMutation(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1) })
	bot, replies, reset := newCommandEntryTestBot(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	commands := []string{
		"创建宗门", "确认创建宗门", "加入宗门", "确认加入宗门", "捐献宗门", "贡献换声望",
		"任命长老", "任命成员", "踢出宗门", "转让宗主", "升级科技", "确认升级科技",
		"宗门改名", "确认宗门改名", "修改宗门名称", "确认修改宗门名称",
		"参加宗门抽奖", "查看宗门抽奖", "重发宗门抽奖", "提醒宗门抽奖", "补发宗门抽奖提醒", "取消宗门抽奖",
		"设置突破成功率", "设置突破消耗", "设置突破冷却", "设置突破最低修为", "设置境界门槛", "设置小境界门槛",
		"设置秘境档位", "设置秘境倍率", "设置秘境掉落",
		"频道公告重试", "频道公告已发", "频道公告未发", "频道提醒重试", "频道提醒已发", "公告异常已发", "公告异常未发",
		"设置突破成功率 1", "设置境界门槛 1 10", "设置秘境倍率 normal 1", "设置秘境掉落 normal 1 丹药",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			telegram.ClearSession(1)
			reset()
			msg.Text = "  " + command + "  "
			handleInteractiveMessage(bot, msg)
			got := replies()
			if len(got) != 1 || !strings.Contains(got[0], "用法：") || !strings.Contains(got[0], strings.Fields(command)[0]+" ") {
				t.Fatalf("expected specific usage for %q, got %q", command, got)
			}
			if telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatal("incomplete command started a confirmation session")
			}
		})
	}
	if treasureTestBalance(t, 1) != 100 || treasureTestCount(t, &model.PointTransaction{}) != 0 || treasureTestCount(t, &model.AuditLog{}) != 0 {
		t.Fatal("incomplete commands changed assets or performed audited operations")
	}
}

func TestCultivationSettingMenuPromptsAndKeepsConfirmation(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1) })
	bot, replies, reset := newCommandEntryTestBot(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	_, keyboard := renderAdminMenu("cultivation", "super_admin")
	settings := 0
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if button.CallbackData == nil || !strings.Contains(*button.CallbackData, ":set_") {
				continue
			}
			settings++
			reset()
			cb := &tgbotapi.CallbackQuery{ID: "setting", From: msg.From, Message: msg, Data: *button.CallbackData}
			if !handleAdminMenuCallback(bot, cb) {
				t.Fatalf("unhandled button: %s", button.Text)
			}
			got := replies()
			if len(got) != 1 || !strings.Contains(got[0], "用法：") || !strings.Contains(got[0], button.Text) || telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatalf("button %s did not show usage: %q", button.Text, got)
			}
		}
	}
	if settings != 6 {
		t.Fatalf("expected six setting buttons, got %d", settings)
	}
	for _, tc := range []struct{ input, step, confirm string }{
		{"设置突破消耗 1 100", "WAITING_CONFIRM_CULTIVATION_ADMIN_WRITE", "确认执行斗气配置"},
		{"设置秘境倍率 normal 1 100 100", "WAITING_CONFIRM_SECT_REALM_CONFIG", "确认执行秘境配置"},
	} {
		telegram.ClearSession(1)
		reset()
		msg.Text = tc.input
		handleInteractiveMessage(bot, msg)
		got := strings.Join(replies(), "\n")
		if telegram.GetSession(1).GetStep() != tc.step || !strings.Contains(got, tc.confirm) {
			t.Fatalf("complete command skipped confirmation: %q, step=%s reply=%s", tc.input, telegram.GetSession(1).GetStep(), got)
		}
	}
	if treasureTestCount(t, &model.AuditLog{}) != 0 {
		t.Fatal("configuration preview performed an audited write")
	}
}

func TestIncompleteAdminCommandsStillRequirePrivateSuperAdmin(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1) })
	bot, replies, reset := newCommandEntryTestBot(t)
	for _, command := range []string{"设置突破成功率", "设置秘境倍率", "频道公告重试", "公告异常已发"} {
		for _, private := range []bool{true, false} {
			telegram.ClearSession(1)
			reset()
			config.AppConfig.AdminIDs = map[int64]bool{1: !private}
			chat := &tgbotapi.Chat{ID: 1, Type: "private"}
			if !private {
				chat = &tgbotapi.Chat{ID: -100, Type: "supergroup"}
			}
			handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: chat, Text: command})
			got := strings.Join(replies(), "\n")
			if got == "" || strings.Contains(got, "用法：") || telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatalf("missing access rejection for %q private=%t: %s", command, private, got)
			}
		}
	}
}

func TestBookRequestTextCommandsMatchMenuEntries(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1); telegram.ClearSession(2) })
	if err := storage.DB.AutoMigrate(&model.BookRequest{}); err != nil {
		t.Fatal(err)
	}
	bot, replies, reset := newCommandEntryTestBot(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	for _, command := range []struct{ text, menu string }{
		{"求书", "📚 求书"}, {"我的求书", "📋 我的求书"}, {"待处理求书", "📚 待处理求书"},
	} {
		var expectedReply, expectedStep string
		for i, text := range []string{command.menu, command.text} {
			telegram.ClearSession(1)
			reset()
			msg.Text = text
			handleInteractiveMessage(bot, msg)
			got := strings.Join(replies(), "\n")
			step := telegram.GetSession(1).GetStep()
			if got == "" {
				t.Fatalf("book request command %q did not reply", text)
			}
			if i == 0 {
				expectedReply, expectedStep = got, step
			} else if got != expectedReply || step != expectedStep {
				t.Fatalf("text command %q differs from menu: reply=%q step=%s", text, got, step)
			}
		}
	}
	reset()
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "待处理求书"})
	if got := strings.Join(replies(), "\n"); !strings.Contains(got, "权限不足") {
		t.Fatalf("text alias bypassed administrator permission: %s", got)
	}
}

func TestMenuEntriesAlwaysReply(t *testing.T) {
	// InitDB also initializes process-wide rule caches; isolate the full-schema
	// menu exercise from tests that install their own databases and cached rules.
	if os.Getenv("MENU_ENTRY_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMenuEntriesAlwaysReply$")
		cmd.Env = append(os.Environ(), "MENU_ENTRY_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("menu routing failed: %v\n%s", err, output)
		}
		return
	}
	config.AppConfig = &config.Config{DbURL: filepath.Join(t.TempDir(), "menus.db"), DatabaseMaxOpenConns: 2, DatabaseMaxIdleConns: 2, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64)}
	InitDB()
	joinVerification = nil
	absServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(absServer.Close)
	abs.DefaultClient = &abs.AbsClient{BaseURL: absServer.URL, Token: "test", HttpClient: absServer.Client()}
	bot, replies, reset := newCommandEntryTestBot(t)
	for i, role := range []string{"user", "admin", "super_admin"} {
		id := int64(2001 + i)
		if err := storage.DB.Create(&model.User{TelegramID: id, Username: role, AbsUserID: role, Points: 100, Role: role}).Error; err != nil {
			t.Fatal(err)
		}
		msg := &tgbotapi.Message{MessageID: 123, From: &tgbotapi.User{ID: id, FirstName: role}, Chat: &tgbotapi.Chat{ID: id, Type: "private"}}
		callbacks := map[string]bool{}
		collect := func(keyboard tgbotapi.InlineKeyboardMarkup) {
			for _, row := range keyboard.InlineKeyboard {
				for _, button := range row {
					if button.CallbackData != nil {
						callbacks[*button.CallbackData] = true
					}
				}
			}
		}
		for _, panel := range []string{"profile", "daily", "cultivation", "sect", "activity", "book", "assets", "account", "security"} {
			_, keyboard := renderFeatureMenu(panel, id)
			collect(keyboard)
		}
		if role != "user" {
			for _, panel := range []string{"overview", "users", "assets", "cards", "events", "books", "cultivation", "ops", "config", "flow"} {
				_, keyboard := renderAdminMenu(panel, role)
				collect(keyboard)
			}
		}
		keys := make([]string, 0, len(callbacks))
		for key := range callbacks {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			t.Run(role+"/"+key, func(t *testing.T) {
				telegram.ClearSession(id)
				reset()
				cb := &tgbotapi.CallbackQuery{ID: "menu", From: msg.From, Message: msg, Data: key}
				if !handleMenuCallback(bot, cb) && !handleAdminMenuCallback(bot, cb) {
					t.Fatalf("unhandled menu callback: %s", key)
				}
				// Entry receipts are sent by a bounded background queue.
				deadline := time.Now().Add(time.Second)
				for len(replies()) == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if len(replies()) == 0 {
					t.Fatal("menu only acknowledged the click without replying or updating its panel")
				}
			})
		}
		keyboards := []tgbotapi.ReplyKeyboardMarkup{telegram.UserMainMenu}
		if role == "admin" {
			keyboards = append(keyboards, telegram.NormalAdminMenu)
		} else if role == "super_admin" {
			keyboards = append(keyboards, telegram.SuperAdminMenu)
		}
		for _, keyboard := range keyboards {
			for _, row := range keyboard.Keyboard {
				for _, button := range row {
					telegram.ClearSession(id)
					reset()
					msg.Text = button.Text
					handleInteractiveMessage(bot, msg)
					if len(replies()) == 0 {
						t.Errorf("%s primary menu %q did not reply", role, button.Text)
					}
				}
			}
		}
	}
}

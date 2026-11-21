package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func TestSectHornMenuAndTextEntry(t *testing.T) {
	setupTreasureTestDB(t)
	previous := joinVerification
	joinVerification = nil
	telegram.ClearSession(1)
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1) })
	if err := storage.DB.AutoMigrate(&model.Sect{}, &model.SectMember{}, &model.SectHornBroadcast{}, &model.SectHornDelivery{}, &model.BookRequest{}); err != nil {
		t.Fatal(err)
	}
	sect := model.Sect{Name: "测试宗门", OwnerID: 1}
	if err := storage.DB.Create(&sect).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SectMember{SectID: int64(sect.ID), UserID: 1, Role: "owner"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).
		Updates(map[string]interface{}{"status": "active", "abs_user_id": "test-abs"}).Error; err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var replies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			mu.Lock()
			replies = append(replies, r.Form.Get("text"))
			mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
		default:
			t.Errorf("unexpected Telegram request: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{MessageID: 123, From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	cb := &tgbotapi.CallbackQuery{ID: "horn-menu", From: msg.From, Message: msg}

	for _, horn := range []struct{ key, command, scope, cost string }{
		{"sect_horn", "宗门喇叭", "sect", "2 积分"},
		{"world_horn", "世界喇叭", "world", "5 积分"},
	} {
		t.Run(horn.key, func(t *testing.T) {
			for _, entry := range []string{"menu", "text"} {
				telegram.ClearSession(1)
				mu.Lock()
				replies = nil
				mu.Unlock()
				if entry == "menu" {
					cb.Data = "menu:cmd:" + horn.key
					if !handleMenuCallback(bot, cb) {
						t.Fatal("horn menu callback was not handled")
					}
				} else {
					msg.Text = "  " + horn.command + "  "
					handleInteractiveMessage(bot, msg)
				}
				mu.Lock()
				got := append([]string(nil), replies...)
				mu.Unlock()
				if len(got) != 1 || !strings.Contains(got[0], horn.command+" 内容") || !strings.Contains(got[0], horn.cost) || strings.Contains(got[0], "正文不能为空") {
					t.Fatalf("%s should show horn usage, got %q", entry, got)
				}
				if telegram.GetSession(1).GetStep() != "IDLE" {
					t.Fatal("opening usage started a confirmation session")
				}
			}

			msg.Text = horn.command + " 今晚九点开启秘境，大家提前准备。"
			handleInteractiveMessage(bot, msg)
			session := telegram.GetSession(1)
			if session.GetStep() != "WAITING_CONFIRM_SECT_HORN" || session.GetTemp("sect_horn_scope") != horn.scope || session.GetTemp("sect_horn_content") != "今晚九点开启秘境，大家提前准备。" {
				t.Fatal("horn with content did not enter its confirmation session")
			}
			if treasureTestBalance(t, 1) != 100 || treasureTestCount(t, &model.SectHornBroadcast{}) != 0 {
				t.Fatal("usage or preview charged points or submitted a broadcast")
			}
		})
	}
}

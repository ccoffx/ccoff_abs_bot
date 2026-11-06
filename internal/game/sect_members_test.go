package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSectMembersSendAndPageKeyboard(t *testing.T) {
	for _, count := range []int{1, 30, 31} {
		t.Run(fmt.Sprintf("members=%d", count), func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.AutoMigrate(&model.Sect{}, &model.SectMember{}, &model.SectTechnology{}, &model.Cultivation{}); err != nil {
				t.Fatal(err)
			}
			sect := model.Sect{Name: "测试宗门", OwnerID: 1, Level: 3, MemberCount: count}
			if err := storage.DB.Create(&sect).Error; err != nil {
				t.Fatal(err)
			}
			for id := 1; id <= count; id++ {
				member := model.SectMember{SectID: int64(sect.ID), UserID: int64(id), UserName: fmt.Sprintf("成员%d", id), Role: sectRoleMember}
				if err := storage.DB.Create(&member).Error; err != nil {
					t.Fatal(err)
				}
			}

			var sends, edits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
				if method == "getMe" {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				if method == "answerCallbackQuery" {
					fmt.Fprint(w, `{"ok":true,"result":true}`)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				var markup struct {
					InlineKeyboard [][]tgbotapi.InlineKeyboardButton `json:"inline_keyboard"`
				}
				if err := json.Unmarshal([]byte(r.Form.Get("reply_markup")), &markup); err != nil || markup.InlineKeyboard == nil {
					t.Errorf("Telegram requires an array for inline_keyboard: markup=%s err=%v", r.Form.Get("reply_markup"), err)
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Field inline_keyboard must be of type Array"}`)
					return
				}
				if count <= sectMemberListPageSize {
					if len(markup.InlineKeyboard) != 0 {
						t.Errorf("single-page list has pagination buttons: %+v", markup)
					}
				} else {
					want := sectMemberPageCallbackPrefix + "2"
					if method == "editMessageText" {
						want = sectMemberPageCallbackPrefix + "1"
					}
					if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 || markup.InlineKeyboard[0][0].CallbackData == nil || *markup.InlineKeyboard[0][0].CallbackData != want {
						t.Errorf("incorrect pagination buttons: %+v, want %s", markup, want)
					}
				}
				if method == "sendMessage" {
					sends.Add(1)
				} else if method == "editMessageText" {
					edits.Add(1)
				} else {
					t.Errorf("unexpected Telegram method: %s", method)
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":1,"type":"private"}}}`)
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			msg := &tgbotapi.Message{MessageID: 42, From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
			if !HandleSectCommand(bot, msg, "宗门成员") {
				t.Fatal("member command was not handled")
			}
			// An old page-2 button must also work after membership drops to one page.
			cb := &tgbotapi.CallbackQuery{ID: "page", From: msg.From, Message: msg, Data: sectMemberPageCallbackPrefix + "2"}
			if !HandleSectMemberPageCallback(bot, cb) {
				t.Fatal("member page callback was not handled")
			}
			if sends.Load() != 1 || edits.Load() != 1 {
				t.Fatalf("member list was not delivered: sends=%d edits=%d", sends.Load(), edits.Load())
			}
		})
	}
}

package books

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ccoff-abs-bot/internal/model"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestBookRequestClearsButtonsAfterClosingAndPublishing(t *testing.T) {
	db := setupBookAnnouncementStateDB(t)
	if err := db.AutoMigrate(&model.BookRequest{}, &model.BookRequestLog{}, &model.User{}, &model.AuditLog{}, &model.PointTransaction{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{TelegramID: 1, Username: "reader", Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	req := model.BookRequest{UserID: 1, UserName: "reader", Status: bookRequestStatusPending, CostPaid: 5, AdminChatID: 2, AdminMessageID: 11}
	if err := db.Create(&req).Error; err != nil {
		t.Fatal(err)
	}
	var cleared atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if strings.Contains(r.URL.Path, "/editMessage") {
			var markup struct {
				InlineKeyboard [][]tgbotapi.InlineKeyboardButton `json:"inline_keyboard"`
			}
			if err := json.Unmarshal([]byte(r.Form.Get("reply_markup")), &markup); err != nil || markup.InlineKeyboard == nil || len(markup.InlineKeyboard) != 0 {
				t.Errorf("expected Telegram button removal with an empty array, got %s", r.Form.Get("reply_markup"))
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"field inline_keyboard must be of type Array"}`)
				return
			}
			cleared.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		} else {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
		}
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	cb := &tgbotapi.CallbackQuery{ID: "cancel", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 10, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
	EditBookRequestAdminMessage(bot, 2, 11, req, true)
	removeBookAnnouncementPreviewButtons(bot, cb)
	handleBookRequestUserCancel(bot, cb, req.ID, true)
	if cleared.Load() != 4 {
		t.Fatalf("expected admin close, preview removal and both cancellation panel updates, got %d", cleared.Load())
	}
	var user model.User
	if err := db.First(&req, req.ID).Error; err != nil || req.Status != BookRequestStatusCancelled {
		t.Fatalf("request was not cancelled: %+v %v", req, err)
	}
	if err := db.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 105 {
		t.Fatalf("cancellation refund changed: points=%d err=%v", user.Points, err)
	}
	handleBookRequestUserCancel(bot, cb, req.ID, true)
	if err := db.First(&user, user.ID).Error; err != nil || user.Points != 105 {
		t.Fatalf("repeated cancellation refunded twice: points=%d err=%v", user.Points, err)
	}
}

func TestBookRequestCancelPreservesRemainingButtonsOnReadFailure(t *testing.T) {
	for _, scenario := range []string{"success", "list_failure", "telegram_failure"} {
		t.Run(scenario, func(t *testing.T) {
			db := setupBookAnnouncementStateDB(t)
			if err := db.AutoMigrate(&model.BookRequest{}, &model.BookRequestLog{}, &model.User{}, &model.AuditLog{}, &model.PointTransaction{}, &model.ListeningAbuseRecord{}); err != nil {
				t.Fatal(err)
			}
			user := model.User{TelegramID: 1, Username: "reader", AbsUserID: "abs-reader", Points: 100}
			if err := db.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			first := model.BookRequest{UserID: 1, Status: bookRequestStatusPending, CostPaid: 5}
			second := model.BookRequest{UserID: 1, Status: bookRequestStatusPending, CostPaid: 5}
			for _, req := range []*model.BookRequest{&first, &second} {
				if err := db.Create(req).Error; err != nil {
					t.Fatal(err)
				}
			}
			injected := false
			if scenario == "list_failure" {
				if err := db.Callback().Query().Before("gorm:query").Register("test:pending_list_failure", func(tx *gorm.DB) {
					where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
					if tx.Statement.Table != "book_requests" || !ok {
						return
					}
					for _, expression := range where.Exprs {
						if expr, ok := expression.(clause.Expr); ok && strings.Contains(expr.SQL, "status") {
							injected = true
							tx.AddError(errors.New("injected pending request list read failure"))
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer db.Callback().Query().Remove("test:pending_list_failure")
			}
			var mu sync.Mutex
			var keyboards []tgbotapi.InlineKeyboardMarkup
			var texts []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				mu.Lock()
				if strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup") {
					var markup tgbotapi.InlineKeyboardMarkup
					if err := json.Unmarshal([]byte(r.Form.Get("reply_markup")), &markup); err != nil {
						t.Error(err)
					}
					keyboards = append(keyboards, markup)
				}
				if strings.HasSuffix(r.URL.Path, "/sendMessage") {
					texts = append(texts, r.Form.Get("text"))
				}
				mu.Unlock()
				if scenario == "telegram_failure" && strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup") {
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"message can't be edited"}`)
				} else if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				} else {
					fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
				}
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			cb := &tgbotapi.CallbackQuery{ID: "cancel", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 10, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
			handleBookRequestUserCancel(bot, cb, first.ID, true)
			mu.Lock()
			gotKeyboards := append([]tgbotapi.InlineKeyboardMarkup(nil), keyboards...)
			gotText := strings.Join(texts, "\n")
			mu.Unlock()
			if scenario == "list_failure" {
				if !injected || len(gotKeyboards) != 0 {
					t.Fatalf("failed read changed the existing buttons: injected=%t keyboards=%+v", injected, gotKeyboards)
				}
			} else if len(gotKeyboards) != 1 || len(gotKeyboards[0].InlineKeyboard) != 1 || gotKeyboards[0].InlineKeyboard[0][0].CallbackData == nil || *gotKeyboards[0].InlineKeyboard[0][0].CallbackData != fmt.Sprintf("br_ucancel_%d", second.ID) {
				t.Fatalf("remaining request lost its button: %+v", gotKeyboards)
			}
			if (scenario != "success") != strings.Contains(gotText, "列表按钮暂未刷新") {
				t.Fatalf("incorrect refresh feedback: %s", gotText)
			}
			handleBookRequestUserCancel(bot, cb, first.ID, true)
			if err := db.First(&user, user.ID).Error; err != nil || user.Points != 105 {
				t.Fatalf("cancellation did not refund exactly once: points=%d err=%v", user.Points, err)
			}
			if err := db.First(&second, second.ID).Error; err != nil || second.Status != bookRequestStatusPending {
				t.Fatalf("other request was changed: %+v err=%v", second, err)
			}
			if scenario == "list_failure" {
				mu.Lock()
				texts = nil
				mu.Unlock()
				ShowMyBookRequests(bot, 1, 1)
				mu.Lock()
				gotText = strings.Join(texts, "\n")
				mu.Unlock()
				if !strings.Contains(gotText, "查询求书取消入口失败") || strings.Contains(gotText, "我的求书记录") {
					t.Fatalf("failed list read was presented as a normal buttonless list: %s", gotText)
				}
			}
		})
	}
}

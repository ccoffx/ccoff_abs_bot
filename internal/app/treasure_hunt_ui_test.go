package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestTreasureTelegramFlowAndFailedPublication(t *testing.T) {
	for _, failPublication := range []bool{false, true} {
		t.Run(fmt.Sprint(failPublication), func(t *testing.T) {
			setupTreasureTestDB(t)
			var fail atomic.Bool
			fail.Store(failPublication)
			var mu sync.Mutex
			var edits []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"TestBot","username":"test_bot"}}`)
				case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				default:
					if strings.HasSuffix(r.URL.Path, "/sendMessage") && r.Form.Get("reply_markup") != "" && fail.Load() {
						fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"forbidden"}`)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/editMessageText") {
						mu.Lock()
						edits = append(edits, r.Form.Get("text"))
						mu.Unlock()
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 123, "chat": map[string]interface{}{"id": -100, "type": "supergroup"}, "text": r.Form.Get("text")}})
				}
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			message := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, From: &tgbotapi.User{ID: 1}, Text: "发起寻宝"}
			if !activity.HandleTreasureHuntCommand(bot, message, message.Text) {
				t.Fatal("start command not handled")
			}
			var round model.TreasureRound
			if err := storage.DB.Order("id DESC").First(&round).Error; err != nil {
				t.Fatal(err)
			}
			if failPublication {
				if round.Status != activity.TreasureCancelled || treasureTestCount(t, &model.TreasurePick{}) != 0 || treasureTestBalance(t, 1) != 100 {
					t.Fatal("failed publication charged player or kept group occupied")
				}
				fail.Store(false)
				if !activity.HandleTreasureHuntCommand(bot, message, message.Text) {
					t.Fatal("retry not handled")
				}
				round = model.TreasureRound{}
				if err := storage.DB.Order("id DESC").First(&round).Error; err != nil {
					t.Fatal(err)
				}
			}
			if round.Status != activity.TreasureActive || round.MessageID != 123 {
				t.Fatal("successful publication was not activated")
			}
			if err := storage.DB.Model(&round).Update("rewards_json", "[0,0,0,0,2,2,2,5,10]").Error; err != nil {
				t.Fatal(err)
			}
			callback := &tgbotapi.CallbackQuery{ID: "callback", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 123, Chat: message.Chat}, Data: fmt.Sprintf("hunt:%d:8", round.ID)}
			if !activity.HandleTreasureHuntCallback(bot, callback) || treasureTestBalance(t, 1) != 107 {
				t.Fatal("callback did not award fixed prize")
			}
			if !activity.HandleTreasureHuntCallback(bot, callback) || treasureTestBalance(t, 1) != 107 {
				t.Fatal("callback retry duplicated charge/reward")
			}
			callback.Message = &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: -200, Type: "supergroup"}}
			callback.From = &tgbotapi.User{ID: 2}
			activity.HandleTreasureHuntCallback(bot, callback)
			if treasureTestBalance(t, 2) != 100 {
				t.Fatal("forwarded panel charged another user")
			}
			if err := storage.DB.Model(&round).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
				t.Fatal(err)
			}
			activity.ProcessExpiredTreasureRounds(bot, time.Now())
			if err := storage.DB.First(&round, round.ID).Error; err != nil || round.Status != activity.TreasureClosed {
				t.Fatal("scheduler did not close expired round", err)
			}
			mu.Lock()
			lastEdit := edits[len(edits)-1]
			mu.Unlock()
			if !strings.Contains(lastEdit, "本局已结束") || !strings.Contains(lastEdit, "系统补贴 7 分") {
				t.Fatal("final panel did not show settlement")
			}
		})
	}
}

package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestBreakthroughGuardTelegramRecruitmentAndSettlement(t *testing.T) {
	for _, failPublication := range []bool{false, true} {
		t.Run(fmt.Sprint(failPublication), func(t *testing.T) {
			setupGuardTestDB(t)
			previousConfig := config.AppConfig
			config.AppConfig = &config.Config{NoticeGroupID: -100}
			t.Cleanup(func() { config.AppConfig = previousConfig })
			if err := storage.DB.AutoMigrate(&model.AutoDeleteMsg{}, &model.SystemConfig{}, &model.RedPacket{}); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL`).Error; err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			lastEdit := ""
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
					if failPublication && strings.HasSuffix(r.URL.Path, "/sendMessage") && r.Form.Get("reply_markup") != "" {
						fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"forbidden"}`)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/editMessageText") {
						mu.Lock()
						lastEdit = r.Form.Get("text")
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
			source := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, From: &tgbotapi.User{ID: 1, FirstName: "道友"}}
			BeginBreakthroughWithGuards(bot, source, "USE_INVENTORY")
			var ritual model.BreakthroughRitual
			if err := storage.DB.Order("id DESC").First(&ritual).Error; err != nil {
				t.Fatal(err)
			}
			if failPublication {
				if ritual.Status != BreakthroughRitualCancelled || guardTestBalance(t, 1) != 5000 {
					t.Fatal("failed publication left an active request or consumed resources")
				}
				return
			}
			if ritual.Status != BreakthroughRitualWaiting || ritual.ChatID != -100 || ritual.MessageID != 123 {
				t.Fatalf("recruitment not activated: %+v", ritual)
			}
			cb := &tgbotapi.CallbackQuery{ID: "guard", From: &tgbotapi.User{ID: 2, FirstName: "互道者"}, Message: &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}}, Data: fmt.Sprintf("btguard:%d:join", ritual.ID)}
			if !HandleBreakthroughGuardCallback(bot, cb) || guardTestBalance(t, 2) != 95 {
				t.Fatal("callback did not charge exactly five points")
			}
			HandleBreakthroughGuardCallback(bot, cb)
			if guardTestBalance(t, 2) != 95 {
				t.Fatal("duplicate callback charged twice")
			}
			cb.From = source.From
			cb.Data = fmt.Sprintf("btguard:%d:start", ritual.ID)
			HandleBreakthroughGuardCallback(bot, cb)
			ProcessDueBreakthroughRituals(bot, time.Now())
			var settled model.BreakthroughRitual
			if err := storage.DB.First(&settled, ritual.ID).Error; err != nil || settled.Status != BreakthroughRitualSettled {
				t.Fatalf("scheduler did not settle: %+v %v", settled, err)
			}
			var attempt model.BreakthroughAttempt
			if err := storage.DB.First(&attempt, settled.AttemptID).Error; err != nil {
				t.Fatal(err)
			}
			want := 95
			if attempt.Result == "success" {
				want = 105
			}
			if guardTestBalance(t, 2) != want || guardTestBalance(t, 3) != 100 {
				t.Fatal("incorrect volunteer payment or bystander charge")
			}
			ProcessDueBreakthroughRituals(bot, time.Now())
			if guardTestBalance(t, 2) != want {
				t.Fatal("scheduler retry paid twice")
			}
			mu.Lock()
			text := lastEdit
			mu.Unlock()
			if !strings.Contains(text, "已结算") {
				t.Fatalf("final group panel not updated: %s", text)
			}
		})
	}
}

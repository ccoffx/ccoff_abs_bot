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

func TestTreasureResultsHiddenUntilSettlement(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		boxes []int
		until time.Time
	}{
		{"jackpot", []int{8}, time.Now().Add(time.Minute)},
		{"mixed", []int{0, 3, 8}, time.Now().Add(time.Minute)},
		{"all opened", []int{0, 1, 2, 3, 4, 5, 6, 7, 8}, time.Now().Add(time.Minute)},
		{"expired awaiting settlement", []int{8}, time.Now().Add(-time.Second)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var activeText, activeMarkup string
			for variant, rewards := range [][]int{
				{0, 0, 0, 1, 1, 1, 2, 2, 10},
				{10, 0, 0, 1, 1, 1, 2, 2, 0},
			} {
				encoded, _ := json.Marshal(rewards)
				round := model.TreasureRound{ID: 1, EntryCost: 2, RewardsJSON: string(encoded), Status: activity.TreasureActive, ExpiresAt: scenario.until}
				var picks []model.TreasurePick
				for _, box := range scenario.boxes {
					picks = append(picks, model.TreasurePick{BoxIndex: box, UserName: fmt.Sprintf("Player%d", box), Cost: 2, Reward: rewards[box]})
				}
				text, markup, err := activity.RenderTreasurePanel(round, picks)
				if err != nil {
					t.Fatal(err)
				}
				encodedMarkup, _ := json.Marshal(markup)
				if variant == 0 {
					activeText, activeMarkup = text, string(encodedMarkup)
				} else if text != activeText || string(encodedMarkup) != activeMarkup {
					t.Fatal("moving the jackpot changed the active panel and exposed results")
				}
				if !strings.Contains(text, "已开箱，待揭晓") || strings.Contains(text, "获得") || strings.Contains(text, "总奖励") || strings.Contains(text, "系统补贴") {
					t.Fatal("unsettled panel exposed results")
				}
				for _, box := range scenario.boxes {
					if markup.InlineKeyboard[box/3][box%3].Text != fmt.Sprintf("%d号 ✓ 已开", box+1) {
						t.Fatal("opened button exposed results before settlement")
					}
				}
				round.Status = activity.TreasureClosed
				text, markup, err = activity.RenderTreasurePanel(round, picks)
				if err != nil {
					t.Fatal(err)
				}
				for _, pick := range picks {
					if !strings.Contains(text, fmt.Sprintf("获得%d分（净%+d）", pick.Reward, pick.Reward-pick.Cost)) || markup.InlineKeyboard[pick.BoxIndex/3][pick.BoxIndex%3].Text != fmt.Sprintf("%d号 ✓ %d分", pick.BoxIndex+1, pick.Reward) {
						t.Fatal("settled panel did not reveal opened results")
					}
				}
			}
		})
	}
}

func TestTreasureTelegramFlowAndFailedPublication(t *testing.T) {
	for _, failPublication := range []bool{false, true} {
		t.Run(fmt.Sprint(failPublication), func(t *testing.T) {
			setupTreasureTestDB(t)
			var fail atomic.Bool
			fail.Store(failPublication)
			var mu sync.Mutex
			var edits []string
			var editMarkups, messages, answers []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"TestBot","username":"test_bot"}}`)
				case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
					mu.Lock()
					answers = append(answers, r.Form.Get("text"))
					mu.Unlock()
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				default:
					if strings.HasSuffix(r.URL.Path, "/sendMessage") && r.Form.Get("reply_markup") != "" && fail.Load() {
						fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"forbidden"}`)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/editMessageText") {
						mu.Lock()
						edits = append(edits, r.Form.Get("text"))
						editMarkups = append(editMarkups, r.Form.Get("reply_markup"))
						mu.Unlock()
					}
					if strings.HasSuffix(r.URL.Path, "/sendMessage") {
						mu.Lock()
						messages = append(messages, r.Form.Get("text"))
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
			if err := storage.DB.Model(&round).Update("rewards_json", "[0,0,0,1,1,1,2,2,10]").Error; err != nil {
				t.Fatal(err)
			}
			callback := &tgbotapi.CallbackQuery{ID: "callback", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 123, Chat: message.Chat}, Data: fmt.Sprintf("hunt:%d:8", round.ID)}
			if !activity.HandleTreasureHuntCallback(bot, callback) || treasureTestBalance(t, 1) != 108 {
				t.Fatal("callback did not award fixed prize")
			}
			mu.Lock()
			activeEdit, activeMarkup, answer := edits[len(edits)-1], editMarkups[len(editMarkups)-1], answers[len(answers)-1]
			mu.Unlock()
			if !strings.Contains(activeEdit, "已开箱，待揭晓") || strings.Contains(activeEdit, "获得10分") || !strings.Contains(activeMarkup, "9号 ✓ 已开") || strings.Contains(activeMarkup, "9号 ✓ 10分") {
				t.Fatal("callback refresh revealed the jackpot before settlement")
			}
			if !strings.Contains(answer, "结算后统一揭晓") || strings.Contains(answer, "获得") || strings.Contains(answer, "净变化") {
				t.Fatal("callback answer revealed the result before settlement")
			}
			if !activity.HandleTreasureHuntCommand(bot, message, "寻宝状态") {
				t.Fatal("status command not handled")
			}
			mu.Lock()
			statusText := messages[len(messages)-1]
			mu.Unlock()
			if !strings.Contains(statusText, "已开箱，待揭晓") || strings.Contains(statusText, "获得10分") || strings.Contains(statusText, "系统补贴") {
				t.Fatal("status command revealed the jackpot before settlement")
			}
			if !activity.HandleTreasureHuntCallback(bot, callback) || treasureTestBalance(t, 1) != 108 {
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
			if !strings.Contains(lastEdit, "本局已结束") || !strings.Contains(lastEdit, "系统补贴 8 分") || !strings.Contains(lastEdit, "获得10分（净+8）") {
				t.Fatal("final panel did not show settlement")
			}
			if !activity.HandleTreasureHuntCommand(bot, message, message.Text) {
				t.Fatal("next start command not handled")
			}
			var next model.TreasureRound
			if err := storage.DB.Order("id DESC").First(&next).Error; err != nil || next.ID == round.ID || next.Status != activity.TreasureActive {
				t.Fatal("could not start next round immediately after settlement", err)
			}
		})
	}
}

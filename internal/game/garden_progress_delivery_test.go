package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
)

func TestGardenCallbackDeliveryFailureDoesNotRepeatRewards(t *testing.T) {
	db := setupGardenProgressDB(t)
	gardenTestZeroEntropy(t)
	if err := db.Create(&model.GardenProgress{UserID: 1, Experience: 12000}).Error; err != nil {
		t.Fatal(err)
	}
	gardenTestPlant(t, 1, "zijingzhi")
	gardenTestMature(t)
	edits := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			edits <- r.Form.Get("text")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502,"description":"delivery unavailable"}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	callback := &tgbotapi.CallbackQuery{
		ID: "garden-test", From: &tgbotapi.User{ID: 1},
		Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}},
	}
	callback.Data = "garden:level"
	if !HandleGardenCallback(bot, callback) {
		t.Fatal("level callback not routed")
	}
	if len(edits) != 1 {
		t.Fatal("level callback did not render")
	}
	if text := <-edits; !strings.Contains(text, "药园等级说明") || !strings.Contains(text, "80%") {
		t.Fatal("level callback missing rules", text)
	}
	callback.Data = "garden:harvestall"
	for i := 0; i < 2; i++ {
		if !HandleGardenCallback(bot, callback) || len(edits) != 1 {
			t.Fatal("harvest callback not rendered")
		}
		text := <-edits
		if i == 0 && (!strings.Contains(text, "经验 +18") || !strings.Contains(text, "溢出 +28 积分")) {
			t.Fatal("harvest result missing rewards", text)
		}
		if i == 1 && strings.Contains(text, "经验 +18") {
			t.Fatal("retry returned a second harvest result", text)
		}
	}
	experience, err := gardenExperience(db, 1)
	if err != nil || experience != 12018 {
		t.Fatal("delivery retry duplicated experience", experience, err)
	}
	var user model.User
	if err := db.Where("telegram_id = ?", 1).Take(&user).Error; err != nil || user.Points != 128 {
		t.Fatal("delivery retry duplicated points", user.Points, err)
	}
	var count int64
	if err := db.Model(&model.PointTransaction{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("delivery retry duplicated ledger", count, err)
	}
}

func TestGardenPlantAllCallbackDeliveryFailureAndRetry(t *testing.T) {
	db := setupGardenProgressDB(t)
	gardenBatchPlots(t, 3)
	if err := GardenGrantInventoryInTx(db, 1, "青灵叶种子", 8); err != nil {
		t.Fatal(err)
	}
	edits := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			edits <- r.Form.Get("text")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502,"description":"delivery unavailable"}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	cb := &tgbotapi.CallbackQuery{ID: "batch", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 42, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
	cb.Data = "garden:plantalllist"
	if !HandleGardenCallback(bot, cb) || len(edits) != 1 {
		t.Fatal("batch picker not routed")
	}
	if text := <-edits; !strings.Contains(text, "一键种植") {
		t.Fatal(text)
	}
	cb.Data = "garden:plantall:qingling"
	for i := 0; i < 2; i++ {
		if !HandleGardenCallback(bot, cb) || len(edits) != 1 {
			t.Fatal("batch planting not routed")
		}
		text := <-edits
		if i == 0 && (!strings.Contains(text, "青灵叶 ×3") || !strings.Contains(text, "1、2、3")) {
			t.Fatal(text)
		}
		if i == 1 && !strings.Contains(text, "暂无空闲灵田") {
			t.Fatal("retry did not refresh", text)
		}
	}
	gardenBatchStock(t, 5)
	var count int64
	if err := db.Model(&model.GardenPlanting{}).Count(&count).Error; err != nil || count != 3 {
		t.Fatal(count, err)
	}
	for _, data := range []string{"garden:plantall", "garden:plantall:", "garden:plantall:qingling:bad"} {
		cb.Data = data
		if !HandleGardenCallback(bot, cb) || len(edits) != 0 {
			t.Fatal("malformed callback executed", data)
		}
	}
	gardenBatchStock(t, 5)
}

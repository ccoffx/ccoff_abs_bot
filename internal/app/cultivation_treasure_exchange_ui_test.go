package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestCultivationTreasureTelegramFlowAndRetiredCallbacks(t *testing.T) {
	setupCultivationTreasureTestDB(t)
	var mu sync.Mutex
	var text, keyboard string
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
			mu.Lock()
			text, keyboard = r.Form.Get("text"), r.Form.Get("reply_markup")
			mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
		}
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	cb := &tgbotapi.CallbackQuery{ID: "callback", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
	click := func(data string) (string, string) {
		t.Helper()
		cb.Data = data
		if !handleMenuCallback(bot, cb) {
			t.Fatalf("unhandled callback: %s", data)
		}
		mu.Lock()
		defer mu.Unlock()
		return text, keyboard
	}
	_, keys := click("shop:home")
	if !strings.Contains(keys, "shop:treasure") {
		t.Fatal("shop has no material entry")
	}
	panel, _ := click("shop:treasure")
	if !strings.Contains(panel, "地心熔岩髓") || strings.Contains(panel, "破宗源晶") {
		t.Fatal("shop offered the wrong realm's treasure")
	}
	_, keys = click("shop:treasure_prepare")
	var markup tgbotapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(keys), &markup); err != nil {
		t.Fatal(err)
	}
	confirm := *markup.InlineKeyboard[0][0].CallbackData
	if !strings.HasPrefix(confirm, "shop:treasure_confirm:") || exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 3 {
		t.Fatal("confirmation screen charged or has invalid callback")
	}
	cb.Message.Chat = &tgbotapi.Chat{ID: -100, Type: "supergroup"}
	click(confirm)
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 3 {
		t.Fatal("group callback charged assets")
	}
	cb.Message.Chat = &tgbotapi.Chat{ID: 2, Type: "private"}
	cb.From.ID = 2
	click(confirm)
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 3 {
		t.Fatal("foreign confirmation charged original user's assets")
	}
	cb.Message.Chat.ID, cb.From.ID = 1, 1
	panel, _ = click(confirm)
	if !strings.Contains(panel, "兑换成功") || exchangeTestQuantity(t, "地心熔岩髓") != 1 {
		t.Fatal("exchange callback did not deliver material")
	}
	click(confirm)
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 2 || exchangeTestQuantity(t, "地心熔岩髓") != 1 || treasureTestBalance(t, 1) != 100 {
		t.Fatal("old callbacks or double-click changed assets")
	}
	// 旧贡献卦象不再放大捐献奖励，捐献积分与贡献维持 1:1。
	if err := storage.DB.AutoMigrate(&model.SectContributionLog{}); err != nil {
		t.Fatal(err)
	}
	sect := model.Sect{Name: "测试宗门", Prestige: 3000}
	if err := storage.DB.Create(&sect).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SectMember{SectID: int64(sect.ID), UserID: 1, Role: "member"}).Error; err != nil {
		t.Fatal(err)
	}
	seedUnusedSectTables(t, int64(sect.ID), "contribution")
	game.HandleDonateSect(bot, &tgbotapi.Message{MessageID: 123, Chat: cb.Message.Chat, From: cb.From}, "20")
	var member model.SectMember
	if err := storage.DB.First(&member, "user_id = ?", 1).Error; err != nil || member.Contribution != 20 || member.WeeklyContribution != 20 || treasureTestBalance(t, 1) != 80 {
		t.Fatalf("donation retained a removed bonus: %+v, %v", member, err)
	}
}

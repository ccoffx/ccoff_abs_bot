package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func TestAdminGeneratedCodesSurviveFailedTelegramDelivery(t *testing.T) {
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.AuditLog{}, &model.InviteCode{}, &model.RenewCode{}, &model.AdminCodeReceipt{}); err != nil {
		t.Fatal(err)
	}
	configPepper := strings.Repeat("s", 64)
	config.AppConfig.SecurityPepper = configPepper
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	config.AppConfig.NoticeGroupID = 0
	telegram.ClearSession(1)
	t.Cleanup(func() { telegram.ClearSession(1) })

	var failing atomic.Bool
	failing.Store(true)
	var sentMu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if failing.Load() {
			fmt.Fprint(w, `{"ok":false,"error_code":503,"description":"temporary failure"}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		sentMu.Lock()
		sent = append(sent, r.Form.Get("text"))
		sentMu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	codes, err := generateInviteCodesWithAudit(1, 2, storage.AutomaticAdminReason)
	if err != nil || len(codes) != 2 {
		t.Fatalf("generate invite codes: count=%d err=%v", len(codes), err)
	}
	if found, err := deliverPendingAdminCodeReceipt(bot, 1); !found || err == nil {
		t.Fatalf("failed send acknowledged or lost receipt: found=%v err=%v", found, err)
	}
	var receipt model.AdminCodeReceipt
	if err := storage.DB.Where("actor_id = ? AND delivered_at IS NULL", 1).First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		if strings.Contains(receipt.ReplyEncrypted, code) {
			t.Fatal("receipt stored plaintext code")
		}
	}
	if _, err := generateRenewCodesWithAudit(1, 30, 1, storage.AutomaticAdminReason); err != errAdminCodeDeliveryPending {
		t.Fatalf("pending batch allowed a second generation: %v", err)
	}
	var renewCount int64
	if err := storage.DB.Model(&model.RenewCode{}).Count(&renewCount).Error; err != nil || renewCount != 0 {
		t.Fatalf("pending batch created renew codes: count=%d err=%v", renewCount, err)
	}

	failing.Store(false)
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "生成续期卡"})
	if step := telegram.GetSession(1).GetStep(); step == "WAITING_GEN_RENEW_DAYS" {
		t.Fatal("recovery command started a new generation wizard")
	}
	sentMu.Lock()
	message := strings.Join(sent, "\n")
	sentMu.Unlock()
	for _, code := range codes {
		if strings.Count(message, code) != 1 {
			t.Fatal("replay did not deliver the original batch exactly once")
		}
	}
	if found, err := deliverPendingAdminCodeReceipt(bot, 1); found || err != nil {
		t.Fatalf("acknowledged batch replayed again: found=%v err=%v", found, err)
	}
	if _, err := generateRenewCodesWithAudit(1, 30, 1, storage.AutomaticAdminReason); err != nil {
		t.Fatalf("new batch blocked after delivery: %v", err)
	}
	if found, err := deliverPendingAdminCodeReceipt(bot, 1); !found || err != nil {
		t.Fatalf("renew batch delivery failed: found=%v err=%v", found, err)
	}
	if err := storage.DB.AutoMigrate(&model.WhitelistCode{}); err != nil {
		t.Fatal(err)
	}
	if codes, err := generateWhitelistCards(1, 100, storage.AutomaticAdminReason); err != nil || len(codes) != 100 {
		t.Fatalf("whitelist batch generation failed: count=%d err=%v", len(codes), err)
	}
	failing.Store(true)
	if found, err := deliverPendingAdminCodeReceipt(bot, 1); !found || err == nil {
		t.Fatalf("failed whitelist send acknowledged: found=%v err=%v", found, err)
	}
	failing.Store(false)
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "生成白名单卡"})
	if step := telegram.GetSession(1).GetStep(); step == "WL_CARD_COUNT" {
		t.Fatal("whitelist recovery started a new generation wizard")
	}
	sentMu.Lock()
	last := sent[len(sent)-1]
	sentMu.Unlock()
	if units := len(utf16.Encode([]rune(last))); units > 3800 || strings.Count(last, "WL-") != 100 {
		t.Fatalf("whitelist recovery lost cards or exceeded Telegram length: units=%d", units)
	}
}

package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func TestPointExchangeRecoversCodeWithoutChargingAgain(t *testing.T) {
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.InviteCode{}, &model.RenewCode{}, &model.ExchangeCodeReceipt{}); err != nil {
		t.Fatal(err)
	}
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	config.AppConfig.NoticeGroupID = 0
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("points", 1000).Error; err != nil {
		t.Fatal(err)
	}
	telegram.ClearSession(1)
	t.Cleanup(func() { telegram.ClearSession(1) })
	var failing atomic.Bool
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
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "Buyer"}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	send := func(value string) { msg.Text = value; handleInteractiveMessage(bot, msg) }
	send("积分兑换")
	if step := telegram.GetSession(1).GetStep(); step != "WAITING_EXCHANGE_CHOICE" {
		t.Fatalf("exchange menu not opened: %s", step)
	}
	if err := storage.DB.Exec(`CREATE TRIGGER fail_exchange_receipt BEFORE INSERT ON exchange_code_receipts BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	send("1")
	var user model.User
	var codeCount int64
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 1000 {
		t.Fatalf("receipt failure charged points: %d %v", user.Points, err)
	}
	if err := storage.DB.Model(&model.InviteCode{}).Count(&codeCount).Error; err != nil || codeCount != 0 {
		t.Fatalf("receipt failure created code: %d %v", codeCount, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_exchange_receipt").Error; err != nil {
		t.Fatal(err)
	}
	send("积分兑换")
	failing.Store(true)
	send("1")
	var receipt model.ExchangeCodeReceipt
	if err := storage.DB.Where("user_id = ? AND delivered_at IS NULL", 1).First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	plain, err := security.DecryptSecretReceipt("exchange-code", receipt.ID, receipt.ReplyEncrypted)
	if err != nil || !strings.Contains(plain, "邀请码") {
		t.Fatalf("exchange receipt is unrecoverable: %v", err)
	}
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Points != 700 {
		t.Fatalf("exchange cost not committed once: %d", user.Points)
	}
	if err := storage.DB.Model(&model.InviteCode{}).Count(&codeCount).Error; err != nil || codeCount != 1 {
		t.Fatalf("invite code count=%d err=%v", codeCount, err)
	}
	if step := telegram.GetSession(1).GetStep(); step == "WAITING_EXCHANGE_CHOICE" {
		t.Fatal("failed delivery left a purchase session open")
	}

	failing.Store(false)
	send("积分兑换")
	sentMu.Lock()
	last := sent[len(sent)-1]
	sentMu.Unlock()
	if last != plain {
		t.Fatalf("recovery delivered a different code: %q", last)
	}
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 700 {
		t.Fatalf("recovery charged points again: %d %v", user.Points, err)
	}
	if err := storage.DB.Model(&model.InviteCode{}).Count(&codeCount).Error; err != nil || codeCount != 1 {
		t.Fatalf("recovery generated another code: %d %v", codeCount, err)
	}
	send("积分兑换")
	if step := telegram.GetSession(1).GetStep(); step != "WAITING_EXCHANGE_CHOICE" {
		t.Fatalf("new exchange blocked after delivery: %s", step)
	}
	failing.Store(true)
	send("2")
	receipt = model.ExchangeCodeReceipt{}
	if err := storage.DB.Where("user_id = ? AND kind = ? AND delivered_at IS NULL", 1, "renew").First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	renewPlain, err := security.DecryptSecretReceipt("exchange-code", receipt.ID, receipt.ReplyEncrypted)
	if err != nil || !strings.Contains(renewPlain, "续期卡密") {
		t.Fatalf("renew receipt is unrecoverable: %v", err)
	}
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 550 {
		t.Fatalf("renew exchange cost incorrect: %d %v", user.Points, err)
	}
	failing.Store(false)
	send("积分兑换")
	sentMu.Lock()
	last = sent[len(sent)-1]
	sentMu.Unlock()
	if last != renewPlain {
		t.Fatal("renew recovery delivered a different code")
	}
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 550 {
		t.Fatalf("renew recovery charged points again: %d %v", user.Points, err)
	}
}

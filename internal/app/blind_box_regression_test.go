package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func blindBoxFixture(t *testing.T) {
	t.Helper()
	channelOpsFixture(t)
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	config.AppConfig.NoticeGroupID = 0
	if err := storage.DB.AutoMigrate(&model.InviteCode{}, &model.RenewCode{}, &model.BlindBoxReceipt{}); err != nil {
		t.Fatal(err)
	}
}

func TestBlindBoxDeliveryFailureCanRecoverWithoutAnotherCharge(t *testing.T) {
	blindBoxFixture(t)
	var failing atomic.Bool
	failing.Store(true)
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
			return
		}
		if failing.Load() {
			fmt.Fprint(w, `{"ok":false,"error_code":503,"description":"temporary failure"}`)
			return
		}
		sent.Add(1)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":2,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	telegram.ClearSession(2)
	t.Cleanup(func() { telegram.ClearSession(2) })
	telegram.GetSession(2).SetStep("WAITING_CONFIRM_BLIND_BOX")
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 2, FirstName: "test"}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "确认开启权益盲盒"}
	handleInteractiveMessage(bot, msg)
	var receipt model.BlindBoxReceipt
	if err := storage.DB.First(&receipt, "user_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if receipt.DeliveredAt != nil {
		t.Fatal("failed Telegram delivery acknowledged")
	}
	plain, err := security.DecryptSecretReceipt("blind-box-result", receipt.ID, receipt.ReplyEncrypted)
	if err != nil || (!strings.Contains(plain, "卡密") && !strings.Contains(plain, "邀请码")) {
		t.Fatalf("reward unrecoverable: %v", err)
	}
	if strings.Contains(receipt.ReplyEncrypted, "专属卡密") || !strings.HasPrefix(receipt.ReplyEncrypted, "gcm$") {
		t.Fatal("reward persisted in plaintext")
	}
	replay, _, err := activity.ExecuteBlindBoxOpen(msg.From)
	if err != nil || replay != plain {
		t.Fatalf("retry did not reuse reward: %v", err)
	}
	var u model.User
	storage.DB.First(&u, "telegram_id = ?", 2)
	if u.Points != 80 {
		t.Fatalf("retry charged twice: %d", u.Points)
	}
	// A fresh session can recover even after spending the remaining balance.
	storage.DB.Model(&u).Update("points", 0)
	failing.Store(false)
	telegram.ClearSession(2)
	msg.Text = "补领盲盒"
	handleInteractiveMessage(bot, msg)
	if err := storage.DB.First(&receipt, "id = ?", receipt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if receipt.DeliveredAt == nil || sent.Load() == 0 {
		t.Fatal("recovery command did not deliver persisted reward")
	}
	var count int64
	storage.DB.Model(&model.BlindBoxReceipt{}).Count(&count)
	if count != 1 {
		t.Fatal("recovery created another draw")
	}
}

func TestBlindBoxReceiptFailureRollsBackRewardAndDebit(t *testing.T) {
	blindBoxFixture(t)
	if err := storage.DB.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON blind_box_receipts BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := activity.ExecuteBlindBoxOpen(&tgbotapi.User{ID: 2}); err == nil {
		t.Fatal("expected receipt failure")
	}
	var u model.User
	storage.DB.First(&u, "telegram_id = ?", 2)
	if u.Points != 100 {
		t.Fatal("failed transaction consumed points")
	}
	for _, v := range []any{&model.InviteCode{}, &model.RenewCode{}, &model.PointTransaction{}} {
		var count int64
		if err := storage.DB.Model(v).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("failed transaction left reward/debit: %d %v", count, err)
		}
	}
}

func TestBlindBoxConcurrentRetriesSharePendingReward(t *testing.T) {
	blindBoxFixture(t)
	var wg sync.WaitGroup
	replies := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, _, err := activity.ExecuteBlindBoxOpen(&tgbotapi.User{ID: 2})
			if err != nil {
				t.Error(err)
			}
			replies <- reply
		}()
	}
	wg.Wait()
	close(replies)
	first := ""
	for reply := range replies {
		if first == "" {
			first = reply
		}
		if reply != first {
			t.Fatal("concurrent retry produced multiple rewards")
		}
	}
	var u model.User
	storage.DB.First(&u, "telegram_id = ?", 2)
	if u.Points != 80 {
		t.Fatalf("concurrent retries charged %d points", 100-u.Points)
	}
}

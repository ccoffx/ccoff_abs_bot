package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func setupWhitelistCardTest(t *testing.T) {
	t.Helper()
	setupTreasureTestDB(t)
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	// Environment-configured super admins need not have a super_admin DB role.
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	if err := storage.DB.AutoMigrate(&model.WhitelistCode{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id IN ?", []int64{2, 3, 4}).Update("abs_user_id", "abs-test").Error; err != nil {
		t.Fatal(err)
	}
}

func newWhitelistCard(t *testing.T) string {
	t.Helper()
	codes, err := generateWhitelistCards(1, 1, "测试生成白名单卡密")
	if err != nil || len(codes) != 1 {
		t.Fatalf("generate: %v %v", codes, err)
	}
	return security.HashSensitiveToken(codes[0])
}

func TestWhitelistCardSingleUseAndConcurrentRedemption(t *testing.T) {
	setupWhitelistCardTest(t)
	hash := newWhitelistCard(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []int64{2, 3} {
		wg.Add(1)
		go func(id int64) { defer wg.Done(); results <- redeemWhitelistCard(id, hash) }(id)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, errWhitelistCardInvalid) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful redeems: %d", successes)
	}
	var card model.WhitelistCode
	if err := storage.DB.First(&card, "code_hash = ?", hash).Error; err != nil {
		t.Fatal(err)
	}
	if !card.IsUsed || card.UsedAt == nil {
		t.Fatal("missing consumption record")
	}
	if err := redeemWhitelistCard(card.UsedByID, hash); !errors.Is(err, errWhitelistCardAlready) {
		t.Fatal(err)
	}
	var user model.User
	if err := storage.DB.First(&user, "telegram_id = ?", card.UsedByID).Error; err != nil {
		t.Fatal(err)
	}
	if !user.IsWhitelist || user.WhitelistExpire != nil {
		t.Fatal("permanent whitelist missing")
	}
}

func TestWhitelistCardEligibilityDoesNotConsume(t *testing.T) {
	setupWhitelistCardTest(t)
	hash := newWhitelistCard(t)
	for _, tc := range []struct {
		id   int64
		want error
	}{
		{1, errWhitelistCardAdmin}, {5, errWhitelistCardAccount}, {999, errWhitelistCardAccount},
	} {
		if err := redeemWhitelistCard(tc.id, hash); !errors.Is(err, tc.want) {
			t.Fatalf("user %d: %v", tc.id, err)
		}
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 4).Update("account_type", "trial").Error; err != nil {
		t.Fatal(err)
	}
	if err := redeemWhitelistCard(4, hash); !errors.Is(err, errWhitelistCardAccount) {
		t.Fatal(err)
	}
	if err := redeemWhitelistCard(2, security.HashSensitiveToken("WL-fake")); !errors.Is(err, errWhitelistCardInvalid) {
		t.Fatal(err)
	}
	if err := redeemWhitelistCard(2, hash); err != nil {
		t.Fatal(err)
	}
	second := newWhitelistCard(t)
	if err := redeemWhitelistCard(2, second); !errors.Is(err, errWhitelistCardAlready) {
		t.Fatal(err)
	}
	if err := redeemWhitelistCard(3, second); err != nil {
		t.Fatal(err)
	}
}

func TestWhitelistCardAuditFailureRollsBack(t *testing.T) {
	setupWhitelistCardTest(t)
	hash := newWhitelistCard(t)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_whitelist_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if codes, err := generateWhitelistCards(1, 2, "测试审计回滚行为"); err == nil || codes != nil {
		t.Fatal("generation should fail")
	}
	if err := redeemWhitelistCard(2, hash); err == nil {
		t.Fatal("redemption should fail")
	}
	var count int64
	if err := storage.DB.Model(&model.WhitelistCode{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("partial generation: %d %v", count, err)
	}
	var user model.User
	storage.DB.First(&user, "telegram_id = ?", 2)
	if user.IsWhitelist {
		t.Fatal("user change not rolled back")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_whitelist_audit").Error; err != nil {
		t.Fatal(err)
	}
	if err := redeemWhitelistCard(2, hash); err != nil {
		t.Fatalf("card consumed on failure: %v", err)
	}
}

func TestWhitelistCardGenerationPermissionsAndStorage(t *testing.T) {
	setupWhitelistCardTest(t)
	if _, err := generateWhitelistCards(2, 1, "未授权生成卡密测试"); err == nil {
		t.Fatal("non-super admin generated cards")
	}
	for _, count := range []int{0, 101} {
		if _, err := generateWhitelistCards(1, count, "测试卡密数量限制"); err == nil {
			t.Fatal("invalid count accepted")
		}
	}
	codes, err := generateWhitelistCards(1, 100, "批量生成白名单卡测试")
	if err != nil || len(codes) != 100 {
		t.Fatalf("batch: %d %v", len(codes), err)
	}
	var record model.WhitelistCode
	if err := storage.DB.First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(codes[0], "WL-") || record.CodeHash == codes[0] || record.CodePreview == codes[0] {
		t.Fatal("raw secret stored or invalid format")
	}
	if command, ok := menuCommandText("whitelist_card"); !ok || command != "兑换白名单" {
		t.Fatal("missing user entry")
	}
	if command, superOnly, ok := adminMenuCommandText("gen_whitelist"); !ok || !superOnly || command != "生成白名单卡" {
		t.Fatal("missing super admin entry")
	}
}

func TestWhitelistCardTelegramFlow(t *testing.T) {
	setupWhitelistCardTest(t)
	var lastText string
	var mu sync.Mutex
	output := func() string { mu.Lock(); defer mu.Unlock(); return lastText }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"TestBot","username":"test_bot"}}`)
			return
		}
		mu.Lock()
		lastText = r.Form.Get("text")
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer telegram.ClearSession(1)
	defer telegram.ClearSession(2)
	send := func(id int64, text string) {
		t.Helper()
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: id}, Chat: &tgbotapi.Chat{ID: id, Type: "private"}, Text: text})
	}
	send(1, "生成白名单卡")
	if telegram.GetSession(1).GetStep() != "WL_CARD_COUNT" {
		t.Fatalf("generation routed incorrectly: %s", output())
	}
	send(1, "1")
	send(1, "测试白名单生成完整流程")
	send(1, "确认生成白名单卡")
	fields := strings.Fields(output())
	code := fields[len(fields)-1]
	if !strings.HasPrefix(code, "WL-") {
		t.Fatalf("code not delivered: %s", output())
	}
	send(2, "兑换白名单")
	if telegram.GetSession(2).GetStep() != "WL_CARD_INPUT" {
		t.Fatalf("redemption routed to generic exchange: %s", output())
	}
	send(2, code)
	if telegram.GetSession(2).GetStep() != "WL_CARD_REDEEM" {
		t.Fatalf("missing confirmation: %s", output())
	}
	var unused int64
	storage.DB.Model(&model.WhitelistCode{}).Where("is_used = ?", false).Count(&unused)
	if unused != 1 {
		t.Fatal("preview consumed card")
	}
	send(2, "取消")
	if telegram.GetSession(2).GetStep() != "IDLE" {
		t.Fatal("cancel failed")
	}
	send(2, "兑换白名单")
	send(2, code)
	send(2, "确认兑换白名单")
	if !strings.Contains(output(), "兑换成功") {
		t.Fatalf("redemption failed: %s", output())
	}
	if !isHighRiskAuditAction("GENERATE_WHITELIST_CODES") || !isHighRiskAuditAction("REDEEM_WHITELIST_CODE") {
		t.Fatal("audit summary excludes whitelist cards")
	}
}

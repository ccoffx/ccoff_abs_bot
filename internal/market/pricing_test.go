package market

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func newMarketplacePricingBot(t *testing.T) (*tgbotapi.BotAPI, *tgbotapi.Message, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		mu.Lock()
		last = r.FormValue("text")
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	return bot, msg, func() string { mu.Lock(); defer mu.Unlock(); return last }
}

func TestMarketplacePricingIntegerBounds(t *testing.T) {
	for _, tc := range []struct{ value, min, max int }{
		{0, 1, 100000}, {1, 1, 1}, {3, 3, 3}, {15, 13, 17},
		{150, 128, 172}, {300, 255, 345}, {100000, 85000, 100000},
	} {
		t.Run(fmt.Sprint(tc.value), func(t *testing.T) {
			pricing := marketplacePricingForValue(tc.value)
			if pricing.Min != tc.min || pricing.Max != tc.max {
				t.Fatalf("incorrect integer bounds: %+v", pricing)
			}
			for _, price := range []int{tc.min, tc.max} {
				if err := pricing.validate(price); err != nil {
					t.Fatalf("displayed boundary %d rejected: %v", price, err)
				}
			}
			for price, want := range map[int]error{tc.min - 1: storage.ErrMarketplacePriceBelowFloor, tc.max + 1: storage.ErrMarketplacePriceAboveCeiling} {
				err := pricing.validate(price)
				if !errors.Is(err, want) || !strings.Contains(marketplaceCreateErrorText(err), fmt.Sprintf("%d–%d", tc.min, tc.max)) {
					t.Fatalf("missing actionable price error for %d: %v", price, err)
				}
			}
		})
	}
}

func TestSecretWizardQuotesActualCodeAndRechecksCurrentPrice(t *testing.T) {
	setupCodeOwnershipTest(t)
	bot, msg, last := newMarketplacePricingBot(t)
	raw := "pricing-renew-secret"
	if err := storage.CreateRenewCodeRecordWithMeta(storage.DB, raw, 60, storage.RenewCodeSourcePointExchange, 1); err != nil {
		t.Fatal(err)
	}
	session := telegram.GetSession(1)
	t.Cleanup(func() { telegram.ClearSession(1) })
	session.SetTemp("market_type", marketplaceTypeSecret)
	session.SetStep("WAITING_MARKET_NAME")
	handleMarketplaceStep(bot, msg, "自定义商品名称", session)
	if session.GetStep() != "WAITING_MARKET_SECRETS" {
		t.Fatal("price requested before identifying the code")
	}
	handleMarketplaceStep(bot, msg, raw, session)
	if session.GetStep() != "WAITING_MARKET_PRICE" || !strings.Contains(last(), "255–345") || !strings.Contains(last(), "300 积分/件") {
		t.Fatalf("missing actual 60-day renewal quote: %s", last())
	}
	if encrypted := session.GetTemp("market_secrets_enc"); !strings.HasPrefix(encrypted, "gcm$") || strings.Contains(encrypted, raw) {
		t.Fatal("pending codes must remain encrypted")
	}
	handleMarketplaceStep(bot, msg, "254", session)
	if session.GetStep() != "WAITING_MARKET_PRICE" || !strings.Contains(last(), "255–345") {
		t.Fatalf("cannot directly retry price: %s", last())
	}
	var count int64
	if err := storage.DB.Model(&model.MarketplaceListing{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed price created a listing: count=%d err=%v", count, err)
	}
	if err := storage.DB.Create(&model.SystemConfig{Key: "renew_price", Value: "200"}).Error; err != nil {
		t.Fatal(err)
	}
	handleMarketplaceStep(bot, msg, "300", session)
	if !strings.Contains(last(), "340–460") || !strings.Contains(last(), "400 积分/件") {
		t.Fatalf("stale quote accepted or new range missing: %s", last())
	}
	handleMarketplaceStep(bot, msg, "340", session)
	var listing model.MarketplaceListing
	if err := storage.DB.First(&listing).Error; err != nil || listing.Price != 340 {
		t.Fatalf("corrected price not listed: %+v err=%v", listing, err)
	}
	if _, exists := telegram.UserSessions.Load(int64(1)); exists {
		t.Fatal("completed listing retained pending codes")
	}
}

func TestSecretPricingBatchIntersectionAndInvalidCodes(t *testing.T) {
	setupCodeOwnershipTest(t)
	for raw, days := range map[string]int{"renew-30": 30, "renew-35": 35, "renew-60": 60} {
		if err := storage.CreateRenewCodeRecordWithMeta(storage.DB, raw, days, storage.RenewCodeSourcePointExchange, 1); err != nil {
			t.Fatal(err)
		}
	}
	pricing, err := inspectMarketplaceSecretPricing(1, []string{"renew-30", "renew-35"})
	if err != nil || pricing.Min != 149 || pricing.Max != 172 || pricing.OriginalMin != 150 || pricing.OriginalMax != 175 {
		t.Fatalf("batch must use shared integer range: %+v err=%v", pricing, err)
	}
	if _, err := inspectMarketplaceSecretPricing(1, []string{"renew-30", "renew-60"}); !errors.Is(err, errMarketplaceNoCommonPrice) {
		t.Fatalf("incompatible durations must be split: %v", err)
	}
	if err := storage.CreateInviteCodeRecord(storage.DB, "invite-code"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user  int64
		codes []string
		want  error
	}{
		{1, []string{"invite-code", "renew-30"}, storage.ErrMarketplaceMixedSecretSource},
		{1, []string{"unknown-code"}, storage.ErrMarketplaceUnverifiedSecret},
		{2, []string{"renew-30"}, storage.ErrMarketplaceSecretOwnerMismatch},
	} {
		if _, err := inspectMarketplaceSecretPricing(tc.user, tc.codes); !errors.Is(err, tc.want) {
			t.Fatalf("invalid batch quoted: %v, want %v", err, tc.want)
		}
	}
}

func TestInventoryWizardShowsRangeAndPreservesStockOnBadPrice(t *testing.T) {
	for _, tc := range []struct{ item, rangeText, bad, good string }{
		{"回气草种子", "13–17", "12", "13"},
		{"无参考价物品", "1–100000", "0", "1"},
	} {
		t.Run(tc.item, func(t *testing.T) {
			setupCodeOwnershipTest(t)
			if err := storage.DB.AutoMigrate(&model.Inventory{}); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Create(&model.Inventory{UserID: 1, ItemName: tc.item, Quantity: 5}).Error; err != nil {
				t.Fatal(err)
			}
			bot, msg, last := newMarketplacePricingBot(t)
			session := telegram.GetSession(1)
			t.Cleanup(func() { telegram.ClearSession(1) })
			session.SetTemp("market_type", marketplaceTypeInventory)
			session.SetStep("WAITING_MARKET_INVENTORY_ITEM")
			handleMarketplaceStep(bot, msg, tc.item, session)
			handleMarketplaceStep(bot, msg, "2", session)
			if !strings.Contains(last(), tc.rangeText) || session.GetStep() != "WAITING_MARKET_PRICE" {
				t.Fatalf("missing unit price range: %s", last())
			}
			handleMarketplaceStep(bot, msg, tc.bad, session)
			var inv model.Inventory
			if err := storage.DB.First(&inv).Error; err != nil || inv.Quantity != 5 || session.GetStep() != "WAITING_MARKET_PRICE" {
				t.Fatalf("bad price changed stock or blocked retry: %+v err=%v", inv, err)
			}
			handleMarketplaceStep(bot, msg, tc.good, session)
			if err := storage.DB.First(&inv).Error; err != nil || inv.Quantity != 3 {
				t.Fatalf("corrected price failed: %+v err=%v message=%s", inv, err, last())
			}
		})
	}
}

func TestSpiritFirePriceLookupDoesNotListOrLock(t *testing.T) {
	setupFireMarket(t)
	bot, msg, last := newMarketplacePricingBot(t)
	if !handleSpiritFireListingCommand(bot, msg, "上架异火 19") || !strings.Contains(last(), "1700–2300") || !strings.Contains(last(), "2000 积分/件") {
		t.Fatalf("missing flame quote: %s", last())
	}
	var fire model.SpiritFire
	if err := storage.DB.First(&fire, 19).Error; err != nil || fire.ListingID != 0 || fire.OwnerID != 1 {
		t.Fatalf("lookup locked or transferred fire: %+v err=%v", fire, err)
	}
	handleSpiritFireListingCommand(bot, msg, "上架异火 19 2301")
	if !strings.Contains(last(), "1700–2300") {
		t.Fatalf("flame price error missing bounds: %s", last())
	}
	handleSpiritFireListingCommand(bot, msg, "上架异火 19 1700")
	if err := storage.DB.First(&fire, 19).Error; err != nil || fire.ListingID == 0 {
		t.Fatalf("corrected flame price not accepted: %+v err=%v", fire, err)
	}
}

func TestSecretListingsRejectZeroReferencePriceAndInvalidDuration(t *testing.T) {
	setupCodeOwnershipTest(t)
	if err := storage.DB.Create(&model.SystemConfig{Key: "invite_price", Value: "0"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateInviteCodeRecord(storage.DB, "free-invite"); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectMarketplaceSecretPricing(1, []string{"free-invite"}); err == nil {
		t.Fatal("zero-priced system code was quoted as an unknown-price item")
	}
	if _, err := createMarketplaceSecretListing(1, "seller", "免费邀请码", 100000, []string{"free-invite"}); err == nil {
		t.Fatal("zero-priced system code bypassed the price ceiling")
	}
	for _, days := range []int{-1, 0, 366} {
		raw := fmt.Sprintf("bad-renew-%d", days)
		if err := storage.CreateRenewCodeRecord(storage.DB, raw, days); !errors.Is(err, storage.ErrInvalidRenewCode) {
			t.Fatalf("created invalid %d-day card: %v", days, err)
		}
		// Even malformed records inserted outside the creation helper cannot
		// be quoted, sold or cashed out as valid 30-day cards.
		code := model.RenewCode{CodeHash: security.HashSensitiveToken(raw), Days: days}
		if err := storage.DB.Create(&code).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := inspectMarketplaceSecretPricing(1, []string{raw}); !errors.Is(err, storage.ErrMarketplaceVerifiedInvalid) {
			t.Fatalf("quoted invalid %d-day card: %v", days, err)
		}
		if _, err := InspectCodeCashout(raw, 1); !errors.Is(err, ErrCodeCashoutInvalid) {
			t.Fatalf("cashed out invalid %d-day card: %v", days, err)
		}
	}
}

func TestSecretPriceAboveListingCapHasActionableError(t *testing.T) {
	setupCodeOwnershipTest(t)
	if err := storage.DB.Create(&model.SystemConfig{Key: "renew_price", Value: "100000"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateRenewCodeRecord(storage.DB, "expensive-annual-card", 365); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectMarketplaceSecretPricing(1, []string{"expensive-annual-card"}); !errors.Is(err, errMarketplaceNoAllowedPrice) {
		t.Fatalf("unlistable card incorrectly advised to split batch: %v", err)
	}
}

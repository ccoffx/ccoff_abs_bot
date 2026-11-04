package market

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestOrderReportNeedsNoExplanationAndRemainsBuyerBound(t *testing.T) {
	setupCodeOwnershipTest(t)
	if err := storage.DB.AutoMigrate(&model.MarketplaceDispute{}); err != nil {
		t.Fatal(err)
	}
	p := model.MarketplacePurchase{BuyerID: 1, SellerID: 2, ListingID: 3, ItemName: "测试商品"}
	if err := storage.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 3}, Chat: &tgbotapi.Chat{ID: 3, Type: "private"}}
	command := fmt.Sprintf("举报订单 %d", p.ID)
	handleMarketplaceDispute(bot, msg, command)
	var count int64
	storage.DB.Model(&model.MarketplaceDispute{}).Count(&count)
	if count != 0 {
		t.Fatal("another user reported someone else's order")
	}
	msg.From.ID = 1
	msg.Chat.ID = 1
	handleMarketplaceDispute(bot, msg, command)
	handleMarketplaceDispute(bot, msg, command)
	storage.DB.Model(&model.MarketplaceDispute{}).Count(&count)
	if count != 1 {
		t.Fatalf("want one dispute, got %d", count)
	}
	var d model.MarketplaceDispute
	if err := storage.DB.First(&d).Error; err != nil {
		t.Fatal(err)
	}
	if d.Reason != "用户提交订单异常举报" {
		t.Fatal("missing generated explanation")
	}
}

package activity

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"ccoff-abs-bot/internal/model"
)

func TestLargeLotteryResultFitsTelegramAndRetainsClaimInstructions(t *testing.T) {
	winners := make([]lotteryDrawWinnerInfo, 100)
	for i := range winners {
		winners[i] = lotteryDrawWinnerInfo{UserID: int64(i + 1), UserName: strings.Repeat("😀", 40), PrizeType: lotteryPrizeManual, DisplayName: strings.Repeat("礼", 80)}
	}
	text := lotteryResultText(model.LotteryActivity{Title: "大型抽奖"}, 100, 100, winners)
	if n := len(utf16.Encode([]rune(text))); n > 4096 {
		t.Fatalf("result too long for Telegram: %d UTF-16 units", n)
	}
	if !strings.Contains(text, "领取奖励") || !strings.Contains(text, "24 小时") || !strings.Contains(text, "100") {
		t.Fatal("summary lost activity/claim information")
	}
}

func TestLargeLotteryChannelIntroFitsTelegram(t *testing.T) {
	db := setupLotteryDB(t)
	if err := db.AutoMigrate(&model.AnnouncementDelivery{}); err != nil {
		t.Fatal(err)
	}
	a := model.LotteryActivity{Title: "大型抽奖", Mode: lotteryModeCount, Status: lotteryStatusActive, ParticipantLimit: 100, AnnounceIsChannel: true, AnnounceChatID: -100200}
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := db.Create(&model.LotteryPrize{ActivityID: a.ID, PrizeType: lotteryPrizeManual, DisplayName: fmt.Sprintf("%d%s", i, strings.Repeat("😀", 70)), Quantity: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := queueLotteryIntro(db, a); err != nil {
		t.Fatal(err)
	}
	var delivery model.AnnouncementDelivery
	if err := db.First(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if n := len(utf16.Encode([]rune(delivery.Text))); n > 4096 {
		t.Fatalf("intro too long for Telegram: %d UTF-16 units", n)
	}
	if !strings.Contains(delivery.Text, "参与抽奖") {
		t.Fatal("summary lost entry instructions")
	}
}

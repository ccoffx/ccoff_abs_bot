package activity

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func TestLotteryMixedPrizesGenerateCodesOnlyOnClaim(t *testing.T) {
	db := setupLotteryDB(t)
	bot, _ := newClaimBot(t)
	session := &telegram.SessionState{}
	for key, value := range map[string]string{"lottery_title": "多种奖品测试", "lottery_mode": lotteryModeCount, "lottery_limit": "4", "lottery_entry_cost": "0", "lottery_prizes": "积分 100 1\n邀请码 1\n续期 30 1\n白名单 1"} {
		session.SetTemp(key, value)
	}
	id, err := createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}}, session)
	if err != nil {
		t.Fatal(err)
	}
	assertRewardCount(t, &model.LotteryPrize{}, 4)
	for _, record := range []any{&model.InviteCode{}, &model.RenewCode{}, &model.WhitelistCode{}} {
		assertRewardCount(t, record, 0)
	}
	for i := int64(101); i <= 104; i++ {
		if _, _, err := joinLotteryActivity(id, &tgbotapi.User{ID: i}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := drawLotteryActivity(bot, id, "manual", 1); err != nil {
		t.Fatal(err)
	}
	// Drawing assigns prize types, but does not create any of the redeemable codes.
	for _, record := range []any{&model.InviteCode{}, &model.RenewCode{}, &model.WhitelistCode{}} {
		assertRewardCount(t, record, 0)
	}
	var winners []model.LotteryWinner
	if err := db.Where("activity_id = ?", id).Find(&winners).Error; err != nil {
		t.Fatal(err)
	}
	if len(winners) != 4 {
		t.Fatalf("winners=%d", len(winners))
	}
	seen := map[string]bool{}
	for _, winner := range winners {
		if seen[winner.PrizeType] {
			t.Fatalf("duplicate prize type: %s", winner.PrizeType)
		}
		seen[winner.PrizeType] = true
		reply, err := claimLotteryWinner(model.LotteryActivity{Model: gorm.Model{ID: id}}, winner)
		if err != nil {
			t.Fatal(err)
		}
		if winner.PrizeType == lotteryPrizeWhitelist {
			code := regexp.MustCompile(`WL-[a-f0-9]{32}`).FindString(reply)
			if code == "" {
				t.Fatalf("missing whitelist card: %s", reply)
			}
			var card model.WhitelistCode
			if err := db.Where("code_hash = ?", security.HashSensitiveToken(code)).First(&card).Error; err != nil {
				t.Fatal(err)
			}
			if card.IsUsed || card.CreatedByID != 1 || card.CodePreview != security.MaskSecret(code) {
				t.Fatalf("invalid whitelist card: %+v", card)
			}
		}
	}
	for _, record := range []any{&model.InviteCode{}, &model.RenewCode{}, &model.WhitelistCode{}} {
		assertRewardCount(t, record, 1)
	}
}

func TestLotteryWhitelistValidation(t *testing.T) {
	for _, input := range []string{"白名单 1", "白名单卡 100"} {
		specs, err := parseLotteryPrizeSpecs(input)
		if err != nil || len(specs) != 1 || specs[0].PrizeType != lotteryPrizeWhitelist {
			t.Fatalf("%q: %+v %v", input, specs, err)
		}
	}
	for _, input := range []string{"白名单 0", "白名单 -1", "白名单 101", "白名单 abc", "白名单 1 2", "白名单 100\n邀请码 1"} {
		if _, err := parseLotteryPrizeSpecs(input); err == nil {
			t.Fatalf("accepted invalid input %q", input)
		}
	}
}

func TestLotteryWhitelistConcurrentClaimAndDeliveryRetry(t *testing.T) {
	setupLotteryDB(t)
	lot, winner := seedClaimReward(t, 101, lotteryPrizeWhitelist, 1, time.Now().Add(time.Hour))
	var wg sync.WaitGroup
	replies := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			reply, err := claimLotteryWinner(lot, winner)
			if err != nil {
				t.Error(err)
			}
			replies <- reply
		})
	}
	wg.Wait()
	close(replies)
	var original string
	for reply := range replies {
		if original == "" {
			original = reply
		}
		if reply != original {
			t.Fatal("concurrent claims generated different cards")
		}
	}
	assertRewardCount(t, &model.WhitelistCode{}, 1)
	bot, capture := newClaimBot(t)
	capture.setFailure(101)
	claimAllLotteryRewards(bot, 101, 101)
	if reloadWinner(t, winner.ID).RewardDeliveredAt != nil {
		t.Fatal("failed delivery marked delivered")
	}
	capture.setFailure(0)
	claimAllLotteryRewards(bot, 101, 101)
	if reloadWinner(t, winner.ID).RewardDeliveredAt == nil {
		t.Fatal("retry not delivered")
	}
	prepared := telegram.PrepareCopyable(tgbotapi.NewMessage(101, original)).(tgbotapi.MessageConfig)
	for _, msg := range capture.snapshot() {
		if msg.ChatID == 101 && strings.Contains(msg.Text, "你的专属白名单卡") && msg.Text != prepared.Text {
			t.Fatal("retry changed card")
		}
	}
	assertRewardCount(t, &model.WhitelistCode{}, 1)
	assertRewardCount(t, &model.LotteryClaimLog{}, 1)
}

func TestLotteryWhitelistClaimRollback(t *testing.T) {
	db := setupLotteryDB(t)
	lot, winner := seedClaimReward(t, 101, lotteryPrizeWhitelist, 1, time.Now().Add(time.Hour))
	failure := errors.New("claim log unavailable")
	hook := "test_whitelist_claim_rollback"
	if err := db.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "lottery_claim_logs" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(hook) })
	if _, err := claimLotteryWinner(lot, winner); !errors.Is(err, failure) {
		t.Fatalf("expected rollback error: %v", err)
	}
	assertRewardCount(t, &model.WhitelistCode{}, 0)
	if got := reloadWinner(t, winner.ID); got.Status != lotteryWinnerPending || got.RewardResultEncrypted != "" {
		t.Fatalf("claim not rolled back: %+v", got)
	}
	db.Callback().Create().Remove(hook)
	if reply, err := claimLotteryWinner(lot, winner); err != nil || !strings.Contains(reply, "WL-") {
		t.Fatalf("retry failed: %s %v", reply, err)
	}
	assertRewardCount(t, &model.WhitelistCode{}, 1)
}

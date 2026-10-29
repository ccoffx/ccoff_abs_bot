package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func seedClaimReward(t *testing.T, userID int64, kind string, amount int, deadline time.Time) (model.LotteryActivity, model.LotteryWinner) {
	t.Helper()
	user := model.User{TelegramID: userID, Username: fmt.Sprintf("winner%d", userID)}
	if err := storage.DB.Where("telegram_id = ?", userID).FirstOrCreate(&user).Error; err != nil {
		t.Fatal(err)
	}
	drawn := deadline.Add(-24 * time.Hour)
	lot := model.LotteryActivity{Title: fmt.Sprintf("福利%d", userID), Mode: lotteryModeCount, Status: lotteryStatusDrawn, DrawnAt: &drawn, ClaimDeadlineAt: &deadline}
	if err := storage.DB.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	prize := model.LotteryPrize{ActivityID: lot.ID, PrizeType: kind, Amount: amount, Quantity: 1, DisplayName: "回气丹"}
	if kind == lotteryPrizeCustomCode {
		var err error
		prize.SecretEncrypted, err = encryptLotteryCustomCode("礼品`卡\\_[]")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.Create(&prize).Error; err != nil {
		t.Fatal(err)
	}
	winner := model.LotteryWinner{ActivityID: lot.ID, UserID: userID, UserName: "中奖道友", PrizeID: prize.ID, PrizeType: kind, Amount: amount, Status: lotteryWinnerPending}
	if err := storage.DB.Create(&winner).Error; err != nil {
		t.Fatal(err)
	}
	return lot, winner
}

type claimMessage struct {
	ChatID   int64
	Text     string
	Entities []tgbotapi.MessageEntity
}
type claimBotCapture struct {
	mu       sync.Mutex
	messages []claimMessage
	failUser int64
}

func (c *claimBotCapture) snapshot() []claimMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]claimMessage(nil), c.messages...)
}
func (c *claimBotCapture) setFailure(user int64) { c.mu.Lock(); defer c.mu.Unlock(); c.failUser = user }
func newClaimBot(t *testing.T) (*tgbotapi.BotAPI, *claimBotCapture) {
	t.Helper()
	capture := &claimBotCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		id, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		msg := claimMessage{ChatID: id, Text: r.Form.Get("text")}
		if raw := r.Form.Get("entities"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &msg.Entities); err != nil {
				t.Error(err)
			}
		}
		capture.mu.Lock()
		if msg.Text != "" {
			capture.messages = append(capture.messages, msg)
		}
		fail := id == capture.failUser && id != 0
		capture.mu.Unlock()
		if fail {
			fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot can't initiate conversation with a user"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":7,"chat":{"id":%d,"type":"private"}}}`, id)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, capture
}

func reloadWinner(t *testing.T, id uint) model.LotteryWinner {
	t.Helper()
	var winner model.LotteryWinner
	if err := storage.DB.First(&winner, id).Error; err != nil {
		t.Fatal(err)
	}
	return winner
}
func assertRewardCount(t *testing.T, modelType interface{}, want int64) {
	t.Helper()
	var count int64
	if err := storage.DB.Model(modelType).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%T count=%d want=%d", modelType, count, want)
	}
}

func TestLotteryClaimIsOwnerOnlyAndPrivate(t *testing.T) {
	setupKeywordDB(t)
	_, winner := seedClaimReward(t, 101, lotteryPrizePoints, 20, time.Now().Add(time.Hour))
	bot, capture := newClaimBot(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 101}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}}
	if !HandleLotteryClaimCommand(bot, msg, lotteryClaimCommand) {
		t.Fatal("group command not routed")
	}
	if got := reloadWinner(t, winner.ID); got.Status != lotteryWinnerPending {
		t.Fatal("group command awarded reward")
	}
	out := capture.snapshot()
	if !strings.Contains(out[0].Text, "Start") || !strings.Contains(out[0].Text, "领取奖励") || len(out[0].Entities) == 0 {
		t.Fatalf("missing private/copy instructions: %+v", out)
	}
	// Another account cannot claim or discover the owner's prize.
	msg.From.ID = 102
	msg.Chat = &tgbotapi.Chat{ID: 102, Type: "private"}
	HandleLotteryClaimCommand(bot, msg, lotteryClaimCommand)
	if got := reloadWinner(t, winner.ID); got.Status != lotteryWinnerPending {
		t.Fatal("another account claimed prize")
	}
	// No start flag is required for group participation. Receiving this private
	// command is sufficient after the winner opens/starts the Bot.
	msg.From.ID = 101
	msg.Chat = &tgbotapi.Chat{ID: 101, Type: "private"}
	session := &telegram.SessionState{}
	session.SetStep("WAITING_MARKET_PRICE")
	if !HandleLotteryCommand(bot, msg, lotteryClaimCommand, session, "user") {
		t.Fatal("claim was swallowed by form")
	}
	if got := reloadWinner(t, winner.ID); got.Status != lotteryWinnerClaimed || got.RewardDeliveredAt == nil {
		t.Fatalf("private claim failed: %+v", got)
	}
	if session.GetStep() != "WAITING_MARKET_PRICE" {
		t.Fatal("claim discarded the form")
	}
	if HandleLotteryCommand(bot, msg, "抽奖领奖 旧暗号", session, "user") {
		t.Fatal("old claim route still active")
	}
}

func TestLotteryClaimAllPartialFailureAndRepeat(t *testing.T) {
	setupKeywordDB(t)
	_, first := seedClaimReward(t, 101, lotteryPrizePoints, 20, time.Now().Add(time.Hour))
	_, broken := seedClaimReward(t, 101, "invalid_prize", 1, time.Now().Add(time.Hour))
	_, last := seedClaimReward(t, 101, lotteryPrizePoints, 30, time.Now().Add(time.Hour))
	bot, _ := newClaimBot(t)
	claimAllLotteryRewards(bot, 101, 101)
	if reloadWinner(t, first.ID).Status != lotteryWinnerClaimed || reloadWinner(t, last.ID).Status != lotteryWinnerClaimed || reloadWinner(t, broken.ID).Status != lotteryWinnerPending {
		t.Fatal("partial failure blocked independent rewards")
	}
	var user model.User
	storage.DB.Where("telegram_id = ?", 101).First(&user)
	if user.Points != 50 {
		t.Fatalf("points=%d", user.Points)
	}
	assertRewardCount(t, &model.PointTransaction{}, 2)
	claimAllLotteryRewards(bot, 101, 101)
	assertRewardCount(t, &model.PointTransaction{}, 2)
	// Failed prize can be repaired and retried without changing successful prizes.
	storage.DB.Model(&model.LotteryWinner{}).Where("id = ?", broken.ID).Updates(map[string]interface{}{"prize_type": lotteryPrizePoints, "amount": 7})
	claimAllLotteryRewards(bot, 101, 101)
	storage.DB.Where("telegram_id = ?", 101).First(&user)
	if user.Points != 57 {
		t.Fatalf("retry points=%d", user.Points)
	}
	assertRewardCount(t, &model.LotteryClaimLog{}, 3)
}

func TestLotteryClaimDeliveryRetryKeepsOriginalCode(t *testing.T) {
	setupKeywordDB(t)
	_, winner := seedClaimReward(t, 101, lotteryPrizeInvite, 1, time.Now().Add(time.Hour))
	bot, capture := newClaimBot(t)
	capture.setFailure(101)
	claimAllLotteryRewards(bot, 101, 101)
	got := reloadWinner(t, winner.ID)
	if got.Status != lotteryWinnerClaimed || got.RewardDeliveredAt != nil || !strings.HasPrefix(got.RewardResultEncrypted, "gcm$") {
		t.Fatalf("failed send lost receipt: %+v", got)
	}
	original, err := decryptLotteryRewardResult(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.RewardResultEncrypted, "YQM-") {
		t.Fatal("plaintext code stored in receipt")
	}
	assertRewardCount(t, &model.InviteCode{}, 1)
	// Already claimed before the deadline: re-delivery still works after expiry.
	past := time.Now().Add(-time.Minute)
	storage.DB.Model(&model.LotteryActivity{}).Where("id = ?", winner.ActivityID).Update("claim_deadline_at", past)
	capture.setFailure(0)
	claimAllLotteryRewards(bot, 101, 101)
	got = reloadWinner(t, winner.ID)
	if got.RewardDeliveredAt == nil {
		t.Fatal("retry did not mark delivery")
	}
	recovered, err := decryptLotteryRewardResult(got)
	if err != nil || recovered != original {
		t.Fatalf("retry changed the reward: %v", err)
	}
	assertRewardCount(t, &model.InviteCode{}, 1)
	assertRewardCount(t, &model.LotteryClaimLog{}, 1)
	msgs := capture.snapshot()
	var sent []string
	for _, msg := range msgs {
		if msg.ChatID == 101 && strings.Contains(msg.Text, "你的专属邀请码") {
			sent = append(sent, msg.Text)
		}
	}
	if len(sent) != 2 || sent[0] != sent[1] {
		t.Fatalf("different delivered codes: %+v", sent)
	}
	claimAllLotteryRewards(bot, 101, 101)
	assertRewardCount(t, &model.InviteCode{}, 1)
}

func TestLotteryClaimConcurrencyAndEncryptionRollback(t *testing.T) {
	setupKeywordDB(t)
	lot, winner := seedClaimReward(t, 101, lotteryPrizePoints, 15, time.Now().Add(time.Hour))
	var wg sync.WaitGroup
	results := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { reply, err := claimLotteryWinner(lot, winner); results <- reply; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for reply := range results {
		if first == "" {
			first = reply
		}
		if reply != first {
			t.Fatal("concurrent receipt changed")
		}
	}
	assertRewardCount(t, &model.PointTransaction{}, 1)
	assertRewardCount(t, &model.LotteryClaimLog{}, 1)
	lot, second := seedClaimReward(t, 102, lotteryPrizePoints, 17, time.Now().Add(time.Hour))
	pepper := config.AppConfig.SecurityPepper
	config.AppConfig.SecurityPepper = ""
	if _, err := claimLotteryWinner(lot, second); err == nil {
		t.Fatal("missing encryption key accepted")
	}
	config.AppConfig.SecurityPepper = pepper
	if got := reloadWinner(t, second.ID); got.Status != lotteryWinnerPending || got.RewardResultEncrypted != "" {
		t.Fatal("failed receipt save did not roll back")
	}
	assertRewardCount(t, &model.PointTransaction{}, 1)
	if _, err := claimLotteryWinner(lot, second); err != nil {
		t.Fatal(err)
	}
	assertRewardCount(t, &model.PointTransaction{}, 2)
}

func TestLotteryClaimExpiryBoundaryAndCleanup(t *testing.T) {
	setupKeywordDB(t)
	deadline := time.Now().Add(time.Hour)
	lot, winner := seedClaimReward(t, 101, lotteryPrizePoints, 20, deadline)
	if lotteryWinnerShouldExpire(lot, winner, deadline.Add(-time.Nanosecond)) || !lotteryWinnerShouldExpire(lot, winner, deadline) || !lotteryWinnerShouldExpire(lot, winner, deadline.Add(time.Nanosecond)) {
		t.Fatal("deadline boundary inconsistent")
	}
	if got := lotteryClaimDeadline(lot, *lot.DrawnAt); !got.Equal(deadline) {
		t.Fatalf("not 24 hours: %v", got)
	}
	past := time.Now().Add(-time.Second)
	storage.DB.Model(&lot).Update("claim_deadline_at", past)
	// The transaction must reload the actual deadline instead of using stale input.
	if _, err := claimLotteryWinner(lot, winner); !errors.Is(err, storage.ErrLotteryClaimExpired) {
		t.Fatalf("stale deadline accepted: %v", err)
	}
	if reloadWinner(t, winner.ID).Status != lotteryWinnerExpired {
		t.Fatal("expired status not committed")
	}
	assertRewardCount(t, &model.PointTransaction{}, 0)
	_, second := seedClaimReward(t, 102, lotteryPrizeInvite, 1, past)
	expirePendingLotteryRewards(time.Now())
	expirePendingLotteryRewards(time.Now())
	if reloadWinner(t, second.ID).Status != lotteryWinnerExpired {
		t.Fatal("scheduler did not expire pending prize")
	}
	assertRewardCount(t, &model.LotteryClaimLog{}, 2)
	assertRewardCount(t, &model.InviteCode{}, 0)
	bot, _ := newClaimBot(t)
	runLotterySchedulerTick(bot)
	var expiredActivity model.LotteryActivity
	if err := storage.DB.First(&expiredActivity, second.ActivityID).Error; err != nil {
		t.Fatal(err)
	}
	if !expiredActivity.PinsUnpinned {
		t.Fatal("pin cleanup did not use the reward deadline")
	}

}

func TestLotteryClaimPrizeTypesAndBoundCiphertext(t *testing.T) {
	setupKeywordDB(t)
	for _, kind := range []string{lotteryPrizeRenew, lotteryPrizePill, lotteryPrizeManual, lotteryPrizeCustomCode} {
		lot, winner := seedClaimReward(t, 101, kind, 30, time.Now().Add(time.Hour))
		reply, err := claimLotteryWinner(lot, winner)
		if err != nil || reply == "" {
			t.Fatalf("%s: %v", kind, err)
		}
		got := reloadWinner(t, winner.ID)
		recovered, err := decryptLotteryRewardResult(got)
		if err != nil || recovered != reply {
			t.Fatalf("%s receipt: %v", kind, err)
		}
		got.UserID++
		if _, err := decryptLotteryRewardResult(got); err == nil {
			t.Fatal("receipt can be moved to another user")
		}
		got = reloadWinner(t, winner.ID)
		got.ID++
		if _, err := decryptLotteryRewardResult(got); err == nil {
			t.Fatal("receipt can be moved to another winner")
		}
	}
	assertRewardCount(t, &model.RenewCode{}, 1)
	assertRewardCount(t, &model.Inventory{}, 1)
}

func TestLotteryDrawAnnouncementExplainsClaimDespitePrivateFailure(t *testing.T) {
	setupKeywordDB(t)
	lot, winner := seedClaimReward(t, 101, lotteryPrizeCustomCode, 1, time.Now().UTC().Add(time.Hour))
	config.AppConfig.NoticeGroupID = -100
	bot, capture := newClaimBot(t)
	capture.setFailure(101)
	notifyLotteryDrawn(bot, lot, 3, 1, []lotteryDrawWinnerInfo{{UserID: 101, UserName: "中奖道友", PrizeType: winner.PrizeType, DisplayName: "礼品卡"}})
	var found bool
	for _, msg := range capture.snapshot() {
		if msg.ChatID != -100 {
			continue
		}
		found = true
		for _, s := range []string{"中奖道友", "领取奖励", "24 小时", "北京时间", "逾期未领取自动失效", formatLotteryClaimDeadline(lot)} {
			if !strings.Contains(msg.Text, s) {
				t.Fatalf("announcement lacks %q: %q", s, msg.Text)
			}
		}
		if strings.Contains(msg.Text, "已私聊") || strings.Contains(msg.Text, "暗号") || strings.Contains(msg.Text, "礼品`卡") || len(msg.Entities) == 0 {
			t.Fatalf("misleading/leaking announcement: %+v", msg)
		}
	}
	if !found || reloadWinner(t, winner.ID).Status != lotteryWinnerPending {
		t.Fatal("notification failure changed entitlement")
	}
	capture.setFailure(0)
	claimAllLotteryRewards(bot, 101, 101)
	if reloadWinner(t, winner.ID).Status != lotteryWinnerClaimed {
		t.Fatal("cannot claim after failed notification")
	}
}

func TestLotteryWizardGoesDirectlyToPrizes(t *testing.T) {
	setupKeywordDB(t)
	bot, capture := newClaimBot(t)
	session := &telegram.SessionState{}
	session.SetStep("WAITING_LOTTERY_ENTRY_COST")
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	handleLotteryCreateStep(bot, msg, "0", session, "super_admin")
	if session.GetStep() != "WAITING_LOTTERY_PRIZES" {
		t.Fatalf("unexpected step: %s", session.GetStep())
	}
	for _, msg := range capture.snapshot() {
		if strings.Contains(msg.Text, "暗号") {
			t.Fatal("wizard still asks for a claim secret")
		}
	}
	session.SetTemp("lottery_mode", lotteryModeCount)
	session.SetTemp("lottery_limit", "10")
	confirmation := formatLotteryCreateConfirm(session, []lotteryPrizeSpec{})
	if !strings.Contains(confirmation, "24 小时") || !strings.Contains(confirmation, lotteryClaimCommand) || strings.Contains(confirmation, "暗号") {
		t.Fatalf("incorrect confirmation: %s", confirmation)
	}
}

func TestLotteryUnstartedParticipantClaimsAfterActualDraw(t *testing.T) {
	db := setupKeywordDB(t)
	bot, capture := newClaimBot(t)
	capture.setFailure(101)
	config.AppConfig.NoticeGroupID = -100
	future := time.Now().Add(time.Hour)
	lot := newKeywordLottery("春日好运")
	lot.Mode = lotteryModeTime
	lot.DrawAt = &future
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.LotteryPrize{ActivityID: lot.ID, PrizeType: lotteryPrizePoints, Amount: 11, Quantity: 2, DisplayName: "11 积分"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{101, 102} {
		if _, _, err := joinLotteryActivity(lot.ID, &tgbotapi.User{ID: id, FirstName: "未启动用户"}, -100); err != nil {
			t.Fatal(err)
		}
	}
	scheduled := time.Now().Add(-time.Hour)
	if err := db.Model(&lot).Update("draw_at", scheduled).Error; err != nil {
		t.Fatal(err)
	}
	if err := drawLotteryActivity(bot, lot.ID, "scheduled", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&lot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if lot.DrawnAt == nil || lot.ClaimDeadlineAt == nil || lot.ClaimDeadlineAt.Sub(*lot.DrawnAt) != 24*time.Hour {
		t.Fatalf("deadline not based on actual draw: %+v", lot)
	}
	if lot.ClaimDeadlineAt.Sub(scheduled) <= 24*time.Hour {
		t.Fatal("late scheduler shortened reward lifetime")
	}
	assertRewardCount(t, &model.PointTransaction{}, 0)
	capture.setFailure(0)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 101}, Chat: &tgbotapi.Chat{ID: 101, Type: "private"}}
	if !HandleLotteryClaimCommand(bot, msg, lotteryClaimCommand) {
		t.Fatal("winner could not claim after opening Bot")
	}
	var user model.User
	if err := db.Where("telegram_id = ?", 101).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Points != 11 {
		t.Fatalf("reward points=%d", user.Points)
	}
	assertRewardCount(t, &model.PointTransaction{}, 1)
}

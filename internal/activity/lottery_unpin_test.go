package activity

import (
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
)

func TestExpiredLotteryUnpinRetriesAfterTelegramFailure(t *testing.T) {
	db := setupLotteryDB(t)
	bot, capture := newClaimBot(t)
	deadline := time.Now().Add(-time.Minute)
	lot := model.LotteryActivity{
		Title:           "置顶重试",
		Mode:            lotteryModeCount,
		Status:          lotteryStatusDrawn,
		AnnounceChatID:  -100,
		IntroMessageID:  42,
		IntroPinned:     true,
		ClaimDeadlineAt: &deadline,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}

	capture.setFailure(-100)
	unpinExpiredLotteryMessages(bot, time.Now())
	if err := db.First(&lot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if lot.PinsUnpinned {
		t.Fatal("failed Telegram unpin was marked complete")
	}

	capture.setFailure(0)
	unpinExpiredLotteryMessages(bot, time.Now())
	if err := db.First(&lot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !lot.PinsUnpinned {
		t.Fatal("successful retry did not complete pin cleanup")
	}
}

func TestExpiredLotteryUnpinFailureDoesNotStarveLaterActivities(t *testing.T) {
	db := setupLotteryDB(t)
	deadline := time.Now().Add(-time.Minute)
	for i := 0; i < 20; i++ {
		lot := model.LotteryActivity{
			Title:           "持续失败",
			Mode:            lotteryModeCount,
			Status:          lotteryStatusDrawn,
			AnnounceChatID:  -100,
			IntroMessageID:  42,
			IntroPinned:     true,
			ClaimDeadlineAt: &deadline,
		}
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
	}
	later := model.LotteryActivity{Title: "后续活动", Mode: lotteryModeCount, Status: lotteryStatusDrawn, ClaimDeadlineAt: &deadline}
	if err := db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}

	unpinExpiredLotteryMessages(nil, time.Now())
	if err := db.First(&later, later.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !later.PinsUnpinned {
		t.Fatal("earlier unpin failures prevented later activity cleanup")
	}
}

func TestExpiredLotteryRewardFailureDoesNotStarveLaterWinners(t *testing.T) {
	db := setupLotteryDB(t)
	deadline := time.Now().Add(-time.Minute)
	for i := 0; i < 101; i++ {
		lot := model.LotteryActivity{Title: "过期奖励", Mode: lotteryModeCount, Status: lotteryStatusDrawn, ClaimDeadlineAt: &deadline}
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		userID := int64(0) // Invalid old records fail settlement and remain pending.
		if i == 100 {
			userID = 91348
		}
		winner := model.LotteryWinner{ActivityID: lot.ID, UserID: userID, Status: lotteryWinnerPending}
		if err := db.Create(&winner).Error; err != nil {
			t.Fatal(err)
		}
	}

	expirePendingLotteryRewards(time.Now())
	var later model.LotteryWinner
	if err := db.Order("id desc").First(&later).Error; err != nil {
		t.Fatal(err)
	}
	if later.Status != lotteryWinnerExpired {
		t.Fatal("earlier invalid rewards prevented later winner expiry")
	}
}

func TestScheduledLotteryFailureDoesNotStarveLaterDraw(t *testing.T) {
	db := setupLotteryDB(t)
	due := time.Now().Add(-time.Minute)
	for i := 0; i < 20; i++ {
		lot := model.LotteryActivity{Title: "损坏的待开奖活动", Mode: lotteryModeTime, Status: lotteryStatusActive, DrawAt: &due, DrawnAt: &due}
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
	}
	later := model.LotteryActivity{Title: "后续待开奖活动", Mode: lotteryModeTime, Status: lotteryStatusActive, DrawAt: &due}
	if err := db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}

	runLotterySchedulerTick(nil)
	if err := db.First(&later, later.ID).Error; err != nil {
		t.Fatal(err)
	}
	if later.Status != lotteryStatusClosed {
		t.Fatalf("earlier failed draws blocked later activity: status=%s", later.Status)
	}
}

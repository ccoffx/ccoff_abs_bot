package activity

import (
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestFullCountLotteryRecoversAfterRestartAndDrawFailure(t *testing.T) {
	db := setupLotteryDB(t)
	bot, _ := newClaimBot(t)
	session := &telegram.SessionState{}
	for k, v := range map[string]string{"lottery_title": "恢复开奖", "lottery_mode": lotteryModeCount, "lottery_limit": "2", "lottery_entry_cost": "0", "lottery_prizes": "积分 100 1"} {
		session.SetTemp(k, v)
	}
	id, err := createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}}, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{101, 102} {
		if _, _, err := joinLotteryActivity(id, &tgbotapi.User{ID: userID, FirstName: "参与者"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(`CREATE TRIGGER fail_draw BEFORE INSERT ON lottery_winners BEGIN SELECT RAISE(ABORT,'temporary failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	runLotterySchedulerTick(bot)
	var lot model.LotteryActivity
	if err := db.First(&lot, id).Error; err != nil || lot.Status != lotteryStatusActive {
		t.Fatalf("failed draw lost retry state: %+v %v", lot, err)
	}
	if err := db.Exec("DROP TRIGGER fail_draw").Error; err != nil {
		t.Fatal(err)
	}
	runLotterySchedulerTick(bot)
	if err := db.First(&lot, id).Error; err != nil || lot.Status != lotteryStatusDrawn {
		t.Fatalf("full lottery not recovered: %+v %v", lot, err)
	}
	runLotterySchedulerTick(bot)
	var winners int64
	db.Model(&model.LotteryWinner{}).Where("activity_id = ?", id).Count(&winners)
	if winners != 1 {
		t.Fatalf("recovery rerolled winners: %d", winners)
	}
}

package activity

import (
	"crypto/rand"
	"errors"
	"testing"

	"ccoff-abs-bot/internal/model"
)

type lotteryFailingEntropy struct {
	remaining int
	err       error
}

func (reader *lotteryFailingEntropy) Read(p []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, reader.err
	}
	reader.remaining--
	clear(p)
	return len(p), nil
}

func TestLotteryEntropyFailureRollsBackDraw(t *testing.T) {
	for _, successfulReads := range []int{0, 1} {
		stage := []string{"participants", "prizes"}[successfulReads]
		t.Run(stage, func(t *testing.T) {
			db := setupLotteryDB(t)
			bot, capture := newClaimBot(t)
			lot := newTestLottery("随机源故障")
			if err := db.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			for _, userID := range []int64{101, 102} {
				if err := db.Create(&model.LotteryParticipant{ActivityID: lot.ID, UserID: userID, EntryCost: 50}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.LotteryPrize{ActivityID: lot.ID, PrizeType: lotteryPrizePoints, Amount: 10, Quantity: 2}).Error; err != nil {
				t.Fatal(err)
			}
			entropyErr := errors.New("entropy unavailable")
			err := func() error {
				previous := rand.Reader
				rand.Reader = &lotteryFailingEntropy{remaining: successfulReads, err: entropyErr}
				defer func() { rand.Reader = previous }()
				return drawLotteryActivity(bot, lot.ID, "manual", 1)
			}()
			if !errors.Is(err, entropyErr) {
				t.Fatalf("draw did not preserve entropy failure: %v", err)
			}
			if err := db.First(&lot, lot.ID).Error; err != nil {
				t.Fatal(err)
			}
			if lot.Status != lotteryStatusActive || lot.DrawnAt != nil || lot.ClaimDeadlineAt != nil || lot.WinnersCount != 0 || lot.FusionPoolPoints != 0 || lot.TotalEntryCost != 0 {
				t.Fatalf("failed draw left settlement evidence: %+v", lot)
			}
			for _, row := range []any{&model.LotteryWinner{}, &model.PointTransaction{}, &model.AuditLog{}, &model.SystemConfig{}} {
				assertRewardCount(t, row, 0)
			}
			if len(capture.snapshot()) != 0 {
				t.Fatal("announced a draw with failed randomness")
			}
		})
	}
}

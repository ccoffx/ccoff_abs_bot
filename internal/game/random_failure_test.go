package game

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

type gameFailingEntropy struct {
	remaining int
	err       error
}

func (reader *gameFailingEntropy) Read(p []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, reader.err
	}
	reader.remaining--
	clear(p)
	return len(p), nil
}

func TestSectLotteryEntropyFailureRollsBackDraw(t *testing.T) {
	for _, successfulReads := range []int{0, 1} {
		stage := []string{"entries", "prizes"}[successfulReads]
		t.Run(stage, func(t *testing.T) {
			bot := setupSectRegressionTest(t)
			lot := model.SectLottery{SectID: 1, CreatorID: 1, Title: "随机源故障", Mode: sectLotteryModeCount, Status: sectLotteryStatusActive, EntryCount: 2, PrizeCount: 2}
			if err := storage.DB.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			code, err := encryptSectLotterySecret("test-prize")
			if err != nil {
				t.Fatal(err)
			}
			for _, userID := range []int64{1, 2} {
				if err := storage.DB.Create(&model.SectLotteryEntry{LotteryID: lot.ID, SectID: 1, UserID: userID}).Error; err != nil {
					t.Fatal(err)
				}
				if err := storage.DB.Create(&model.SectLotteryPrize{LotteryID: lot.ID, CodeEnc: code, Status: sectLotteryPrizePending}).Error; err != nil {
					t.Fatal(err)
				}
			}
			entropyErr := errors.New("entropy unavailable")
			err = func() error {
				previous := rand.Reader
				rand.Reader = &gameFailingEntropy{remaining: successfulReads, err: entropyErr}
				defer func() { rand.Reader = previous }()
				return drawSectLottery(bot, lot.ID, "scheduled")
			}()
			if !errors.Is(err, entropyErr) {
				t.Fatalf("draw did not preserve entropy failure: %v", err)
			}
			if err := storage.DB.First(&lot, lot.ID).Error; err != nil {
				t.Fatal(err)
			}
			if lot.Status != sectLotteryStatusActive || lot.DrawnAt != nil || lot.WinnerCount != 0 {
				t.Fatalf("failed draw left settlement evidence: %+v", lot)
			}
			var winners, assigned, audits int64
			if err := storage.DB.Model(&model.SectLotteryWinner{}).Count(&winners).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.SectLotteryPrize{}).Where("status <> ? OR assigned_user_id <> 0", sectLotteryPrizePending).Count(&assigned).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.AuditLog{}).Count(&audits).Error; err != nil {
				t.Fatal(err)
			}
			if winners != 0 || assigned != 0 || audits != 0 {
				t.Fatalf("failed draw committed winners=%d assigned=%d audits=%d", winners, assigned, audits)
			}
		})
	}
}

func TestGardenEntropyFailureRollsBackHarvest(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "single"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.AutoMigrate(&model.GardenPlanting{}, &model.Inventory{}); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Exec("CREATE UNIQUE INDEX idx_test_inventory ON inventories(user_id, item_name) WHERE deleted_at IS NULL").Error; err != nil {
				t.Fatal(err)
			}
			// The fixed-yield first plot must also roll back if a later plot fails.
			for i, seed := range []string{"zijingzhi", "huiqicao"} {
				if err := storage.DB.Create(&model.GardenPlanting{UserID: 1, PlotNo: i + 1, SeedKey: seed, Status: gardenStatusGrowing, MaturesAt: time.Now().Add(-time.Hour)}).Error; err != nil {
					t.Fatal(err)
				}
			}
			entropyErr := errors.New("entropy unavailable")
			err := func() error {
				previous := rand.Reader
				rand.Reader = &gameFailingEntropy{err: entropyErr}
				defer func() { rand.Reader = previous }()
				if all {
					_, _, err := gardenHarvestAll(1)
					return err
				}
				_, _, err := gardenHarvestPlot(1, 2)
				return err
			}()
			if !errors.Is(err, entropyErr) {
				t.Fatalf("harvest did not preserve entropy failure: %v", err)
			}
			var harvested, inventory int64
			if err := storage.DB.Model(&model.GardenPlanting{}).Where("status <> ? OR quantity <> 0 OR harvested_at IS NOT NULL", gardenStatusGrowing).Count(&harvested).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.Inventory{}).Count(&inventory).Error; err != nil {
				t.Fatal(err)
			}
			if harvested != 0 || inventory != 0 {
				t.Fatalf("failed harvest consumed plots=%d or granted inventory=%d", harvested, inventory)
			}
		})
	}
}

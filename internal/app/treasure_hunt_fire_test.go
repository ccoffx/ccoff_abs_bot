package app

import (
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type treasureWinningEntropy struct{}

func (treasureWinningEntropy) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func treasureFireEligibleUser(t *testing.T) {
	t.Helper()
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Updates(map[string]any{"abs_user_id": "treasure-fire-user", "is_whitelist": true, "account_type": "formal"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Cultivation{UserID: 1, MajorRealm: 2}).Error; err != nil {
		t.Fatal(err)
	}
}

func treasureFireOwnedCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := storage.DB.Model(&model.SpiritFire{}).Where("owner_id <> 0").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTreasureFireSettlementAtomicityAndRestart(t *testing.T) {
	for _, failClose := range []bool{false, true} {
		name := "success"
		if failClose {
			name = "failed close rolls back"
		}
		t.Run(name, func(t *testing.T) {
			path := setupTreasureTestDB(t)
			treasureFireEligibleUser(t)
			if err := storage.DB.Create(&model.SystemConfig{Key: "fusion_pool_points", Value: "299"}).Error; err != nil {
				t.Fatal(err)
			}
			round := newTreasureTestRound(t, -100)
			treasureTestPick(t, round, 8, 1)
			for box := 0; box < 8; box++ {
				treasureTestPick(t, round, box, int64(box+2))
			}
			if treasureFireOwnedCount(t) != 0 || treasureTestCount(t, &model.SpiritFireNotice{}) != 0 {
				t.Fatal("fire was awarded before settlement")
			}
			previousEntropy := rand.Reader
			rand.Reader = treasureWinningEntropy{}
			t.Cleanup(func() { rand.Reader = previousEntropy })
			if failClose {
				if err := storage.DB.Exec(`CREATE TRIGGER fail_treasure_fire_close BEFORE UPDATE ON treasure_rounds WHEN NEW.status = 'closed' BEGIN SELECT RAISE(ABORT, 'close failed'); END`).Error; err != nil {
					t.Fatal(err)
				}
				closed, burst, err := activity.CloseTreasureRound(round.ID, time.Now())
				if err == nil || closed || burst || treasureFireOwnedCount(t) != 0 || treasureTestPool(t) != 299 || treasureTestCount(t, &model.RedPacket{}) != 0 || treasureTestCount(t, &model.SpiritFireNotice{}) != 0 || treasureTestCount(t, &model.AuditLog{}) != 0 {
					t.Fatal("failed settlement leaked fire, notice, audit or pool changes", err)
				}
				if err := storage.DB.First(&round, round.ID).Error; err != nil || round.Status != activity.TreasureActive || round.FireID != 0 {
					t.Fatal("failed settlement saved a fire receipt", err)
				}
				if err := storage.DB.Exec("DROP TRIGGER fail_treasure_fire_close").Error; err != nil {
					t.Fatal(err)
				}
			}
			closed, burst, err := activity.CloseTreasureRound(round.ID, time.Now())
			if err != nil || !closed || !burst || treasureFireOwnedCount(t) != 1 || treasureTestPool(t) != 0 {
				t.Fatal("settlement did not commit fire and pool together", err)
			}
			if err := storage.DB.First(&round, round.ID).Error; err != nil || round.FireID <= 1 || round.FireOwnerID != 1 || round.FireName == "" {
				t.Fatal("settlement lost fire receipt", err)
			}
			if treasureTestBalance(t, 1) != 108 || treasureTestCount(t, &model.SpiritFireNotice{}) != 1 || treasureTestCount(t, &model.AuditLog{}) != 1 || treasureTestCount(t, &model.RedPacket{}) != 1 {
				t.Fatal("fire changed jackpot points or created duplicate rewards")
			}
			var picks []model.TreasurePick
			if err := storage.DB.Where("round_id = ?", round.ID).Find(&picks).Error; err != nil {
				t.Fatal(err)
			}
			text, _, err := activity.RenderTreasurePanel(round, picks)
			if err != nil || !strings.Contains(text, "额外获得异火【"+round.FireName+"】") {
				t.Fatal("closed panel did not reveal the fire", err)
			}
			active := round
			active.Status = activity.TreasureActive
			text, _, err = activity.RenderTreasurePanel(active, picks)
			if err != nil || strings.Contains(text, round.FireName) || strings.Contains(text, "额外获得异火") {
				t.Fatal("active panel exposed the fire receipt", err)
			}
			reopened, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			previousDB := storage.DB
			storage.DB = reopened
			t.Cleanup(func() { sqlDB, _ := reopened.DB(); _ = sqlDB.Close(); storage.DB = previousDB })
			closed, burst, err = activity.CloseTreasureRound(round.ID, time.Now())
			if err != nil || closed || burst || treasureFireOwnedCount(t) != 1 || treasureTestCount(t, &model.SpiritFireNotice{}) != 1 || treasureTestCount(t, &model.AuditLog{}) != 1 || treasureTestPool(t) != 0 {
				t.Fatal("restart replay duplicated fire or settlement", err)
			}
		})
	}
}

func TestTreasureUnopenedJackpotCannotAwardFire(t *testing.T) {
	for _, box := range []int{0, 3, 7} {
		t.Run(fmt.Sprint(box+1), func(t *testing.T) {
			setupTreasureTestDB(t)
			treasureFireEligibleUser(t)
			round := newTreasureTestRound(t, -100)
			treasureTestPick(t, round, box, 1)
			previousEntropy := rand.Reader
			rand.Reader = treasureWinningEntropy{}
			t.Cleanup(func() { rand.Reader = previousEntropy })
			closed, _, err := activity.CloseTreasureRound(round.ID, round.ExpiresAt)
			if err != nil || !closed || treasureFireOwnedCount(t) != 0 || treasureTestCount(t, &model.SpiritFireNotice{}) != 0 {
				t.Fatal("unopened jackpot granted a fire", err)
			}
		})
	}
}

type treasureMissingEntropy struct{}

func (treasureMissingEntropy) Read(p []byte) (int, error) {
	clear(p)
	if len(p) > 0 {
		p[len(p)-1] = 1 // The 1..10000 draw returns 2, a nonwinning outcome.
	}
	return len(p), nil
}

func TestTreasureFireMissCannotBeRerolled(t *testing.T) {
	path := setupTreasureTestDB(t)
	treasureFireEligibleUser(t)
	round := newTreasureTestRound(t, -100)
	treasureTestPick(t, round, 8, 1)
	previousEntropy := rand.Reader
	rand.Reader = treasureMissingEntropy{}
	t.Cleanup(func() { rand.Reader = previousEntropy })
	closed, _, err := activity.CloseTreasureRound(round.ID, round.ExpiresAt)
	if err != nil || !closed || treasureFireOwnedCount(t) != 0 {
		t.Fatal("nonwinning draw failed", err)
	}
	reopened, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	previousDB := storage.DB
	storage.DB = reopened
	t.Cleanup(func() { sqlDB, _ := reopened.DB(); _ = sqlDB.Close(); storage.DB = previousDB })
	rand.Reader = treasureWinningEntropy{}
	closed, _, err = activity.CloseTreasureRound(round.ID, round.ExpiresAt.Add(time.Minute))
	if err != nil || closed || treasureFireOwnedCount(t) != 0 || treasureTestCount(t, &model.SpiritFireNotice{}) != 0 {
		t.Fatal("settled miss was rerolled after restart", err)
	}
}

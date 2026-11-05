package spiritfire

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

func TestTreasureFireOneInTenThousand(t *testing.T) {
	for _, draw := range []int{1, 2, 100, 10000} {
		t.Run(fmt.Sprint(draw), func(t *testing.T) {
			db, now := fixture(t)
			calls := 0
			var fire model.SpiritFire
			must(t, db.Transaction(func(tx *gorm.DB) error {
				var err error
				fire, err = awardTreasureInTx(tx, model.TreasurePick{RoundID: 1, UserID: 1, Reward: 10}, now, func(lo, hi int) (int, error) {
					calls++
					if calls == 1 {
						if lo != 1 || hi != 10000 {
							t.Fatalf("wrong probability range: %d..%d", lo, hi)
						}
						return draw, nil
					}
					return lo, nil
				})
				return err
			}))
			if (fire.ID != 0) != (draw == 1) {
				t.Fatalf("draw %d produced %+v", draw, fire)
			}
			if draw == 1 {
				if fire.Rank <= 1 || fire.OwnerID != 1 || calls != 2 {
					t.Fatal("invalid fire award", fire)
				}
				var saved model.SpiritFire
				must(t, db.First(&saved, fire.ID).Error)
				if saved.OwnerID != 1 || !saved.AcquiredAt.Equal(now) {
					t.Fatal("fire ownership was not committed")
				}
			} else if calls != 1 {
				t.Fatal("miss should not select a fire")
			}
			u, wallet := seekingState(t, db, 1)
			if u.Points != 100000 || wallet.Scrolls != 100 || wallet.Fragments != 0 || wallet.SeekingSpent != 0 {
				t.Fatal("treasure fire changed points, scrolls or fragments")
			}
		})
	}
}

func TestTreasureFireRequiresJackpotEligibilityAndFreePool(t *testing.T) {
	for _, scenario := range []string{"zero", "one", "two", "five", "no round", "no account", "low realm", "suspended", "trial", "empty", "listed"} {
		t.Run(scenario, func(t *testing.T) {
			db, now := fixture(t)
			pick := model.TreasurePick{RoundID: 1, UserID: 1, Reward: 10}
			switch scenario {
			case "zero":
				pick.Reward = 0
			case "one":
				pick.Reward = 1
			case "two":
				pick.Reward = 2
			case "five":
				pick.Reward = 5
			case "no round":
				pick.RoundID = 0
			case "no account":
				must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("abs_user_id", "").Error)
			case "low realm":
				must(t, db.Model(&model.Cultivation{}).Where("user_id = 1").Update("major_realm", 1).Error)
			case "suspended":
				must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("is_suspended", true).Error)
			case "trial":
				must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("account_type", "trial").Error)
			case "empty", "listed":
				must(t, db.Model(&model.SpiritFire{}).Where("rank > 1").Update("owner_id", 3).Error)
				if scenario == "listed" {
					must(t, db.Model(&model.SpiritFire{}).Where("id = 19").Updates(map[string]any{"owner_id": 0, "listing_id": 7}).Error)
				}
			}
			must(t, db.Transaction(func(tx *gorm.DB) error {
				fire, err := awardTreasureInTx(tx, pick, now, func(lo, hi int) (int, error) { return lo, nil })
				if fire.ID != 0 {
					t.Fatal("ineligible or unavailable award was granted", fire)
				}
				return err
			}))
			for _, row := range []any{&model.SpiritFireNotice{}, &model.AuditLog{}} {
				var n int64
				must(t, db.Model(row).Count(&n).Error)
				if n != 0 {
					t.Fatalf("unawarded treasure created %T records", row)
				}
			}
		})
	}
}

func TestTreasureFireEntropyFailureRollsBack(t *testing.T) {
	for successfulReads := 0; successfulReads < 2; successfulReads++ {
		t.Run(fmt.Sprint(successfulReads), func(t *testing.T) {
			db, now := fixture(t)
			entropyErr := errors.New("entropy unavailable")
			err := withEntropyFailure(successfulReads, entropyErr, func() error {
				return db.Transaction(func(tx *gorm.DB) error {
					_, err := AwardTreasureInTx(tx, model.TreasurePick{RoundID: 1, UserID: 1, Reward: 10}, now)
					return err
				})
			})
			if !errors.Is(err, entropyErr) {
				t.Fatal("entropy failure was not preserved", err)
			}
			assertNoRandomAwards(t, db)
		})
	}
}

func TestTreasureAndBossCannotDuplicateLastFire(t *testing.T) {
	db, now := fixture(t)
	rules := DefaultRules()
	rules.BossChance, rules.ScrollChance = 100, 0
	must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(rules)).Error)
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1 AND id <> 19").Update("owner_id", 3).Error)
	must(t, PrepareEvent(db, "boss:treasure-race", now))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, boss := range []bool{false, true} {
		wg.Add(1)
		go func(boss bool) {
			defer wg.Done()
			errs <- db.Transaction(func(tx *gorm.DB) error {
				if boss {
					return AwardInTx(tx, "boss:treasure-race", []int64{2}, true, now)
				}
				_, err := awardTreasureInTx(tx, model.TreasurePick{RoundID: 1, UserID: 1, Reward: 10}, now, func(lo, hi int) (int, error) { return lo, nil })
				return err
			})
		}(boss)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	var fire model.SpiritFire
	must(t, db.First(&fire, 19).Error)
	var notices int64
	must(t, db.Model(&model.SpiritFireNotice{}).Count(&notices).Error)
	if (fire.OwnerID != 1 && fire.OwnerID != 2) || notices != 1 {
		t.Fatal("last fire was lost or awarded twice", fire, notices)
	}
}

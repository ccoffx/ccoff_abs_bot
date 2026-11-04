package spiritfire

import (
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

type failingEntropy struct {
	remaining int
	err       error
}

func (reader *failingEntropy) Read(p []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, reader.err
	}
	reader.remaining--
	clear(p)
	return len(p), nil
}

func withEntropyFailure(successfulReads int, err error, run func() error) error {
	previous := rand.Reader
	rand.Reader = &failingEntropy{remaining: successfulReads, err: err}
	defer func() { rand.Reader = previous }()
	return run()
}

func assertNoRandomAwards(t *testing.T, db *gorm.DB) {
	t.Helper()
	u, w := seekingState(t, db, 1)
	if u.Points != 100000 || w.Fragments != 0 || w.Scrolls != 100 || w.SeekingSpent != 0 {
		t.Fatalf("failed random award changed wallet: points=%d wallet=%+v", u.Points, w)
	}
	var owned int64
	must(t, db.Model(&model.SpiritFire{}).Where("owner_id <> 0").Count(&owned).Error)
	if owned != 0 {
		t.Fatalf("failed random award assigned %d fires", owned)
	}
	for _, row := range []any{&model.SpiritFireNotice{}, &model.PointTransaction{}, &model.AuditLog{}} {
		var count int64
		must(t, db.Model(row).Count(&count).Error)
		if count != 0 {
			t.Fatalf("failed random award committed %d %T records", count, row)
		}
	}
}

func TestSeekingEntropyFailureRollsBackAwardsAndCosts(t *testing.T) {
	// Fail the scroll roll, fire roll, selection, or the next draw after a fire
	// has already been assigned inside the transaction.
	for successfulReads := 0; successfulReads <= 3; successfulReads++ {
		t.Run(fmt.Sprint(successfulReads), func(t *testing.T) {
			db, now := fixture(t)
			quote, err := SeekingQuote(db, 1, "seek", 10, now)
			must(t, err)
			entropyErr := errors.New("entropy unavailable")
			err = withEntropyFailure(successfulReads, entropyErr, func() error { return Confirm(db, 1, quote.ID, now) })
			if !errors.Is(err, entropyErr) {
				t.Fatalf("seeking did not preserve entropy failure: %v", err)
			}
			assertNoRandomAwards(t, db)
			must(t, db.First(&quote, "id = ?", quote.ID).Error)
			if quote.Done || quote.ResultJSON != "" {
				t.Fatalf("failed seeking completed its receipt: %+v", quote)
			}
		})
	}
}

func TestEventEntropyFailureRollsBackAwards(t *testing.T) {
	// The final case fails after a fire and one participant's scroll have been
	// granted, verifying that both participants' rewards roll back together.
	for successfulReads := 0; successfulReads <= 4; successfulReads++ {
		t.Run(fmt.Sprint(successfulReads), func(t *testing.T) {
			db, now := fixture(t)
			rules := DefaultRules()
			rules.BossChance, rules.ScrollChance = 100, 100
			must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(rules)).Error)
			must(t, PrepareEvent(db, "boss:random", now))
			entropyErr := errors.New("entropy unavailable")
			err := withEntropyFailure(successfulReads, entropyErr, func() error {
				return db.Transaction(func(tx *gorm.DB) error { return AwardInTx(tx, "boss:random", []int64{1, 2}, true, now) })
			})
			if !errors.Is(err, entropyErr) {
				t.Fatalf("event did not preserve entropy failure: %v", err)
			}
			assertNoRandomAwards(t, db)
			var event model.SpiritFireEvent
			must(t, db.First(&event, "id = ?", "boss:random").Error)
			if event.Settled || event.ResultJSON != "" {
				t.Fatalf("failed event was marked settled: %+v", event)
			}
		})
	}
}

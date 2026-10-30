package spiritfire

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func seekConfirmWithRoll(db *gorm.DB, q model.SpiritFireQuote, now time.Time, roll func(int, int) int) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		var latest model.SpiritFireQuote
		if err := tx.First(&latest, "id = ?", q.ID).Error; err != nil {
			return err
		}
		if latest.Done {
			return nil
		}
		return confirmSeeking(tx, latest, now, roll)
	})
}
func seekingState(t *testing.T, db *gorm.DB, user int64) (model.User, model.SpiritFireWallet) {
	t.Helper()
	var u model.User
	var w model.SpiritFireWallet
	must(t, db.Where("telegram_id = ?", user).First(&u).Error)
	must(t, db.First(&w, "user_id = ?", user).Error)
	return u, w
}

func TestSeekingTenAndReplay(t *testing.T) {
	db, now := fixture(t)
	q, err := SeekingQuote(db, 1, "seek", 10, now)
	must(t, err)
	// Both independent chances miss; every draw still earns a fragment.
	must(t, seekConfirmWithRoll(db, q, now, func(lo, hi int) int { return hi }))
	must(t, Confirm(db, 1, q.ID, now.Add(time.Hour))) // replay even after expiry
	u, w := seekingState(t, db, 1)
	if u.Points != 99000 || w.Fragments != 10 || w.Scrolls != 100 || w.SeekingSpent != 1000 {
		t.Fatalf("user=%+v wallet=%+v", u, w)
	}
	var n int64
	must(t, db.Model(&model.PointTransaction{}).Where("ref_id = ?", q.ID).Count(&n).Error)
	if n != 1 {
		t.Fatalf("duplicate debits: %d", n)
	}
	merge, err := SeekingQuote(db, 1, "scroll_merge", 1, now)
	must(t, err)
	must(t, Confirm(db, 1, merge.ID, now))
	must(t, Confirm(db, 1, merge.ID, now))
	u, w = seekingState(t, db, 1)
	if u.Points != 99000 || w.Fragments != 0 || w.Scrolls != 101 || w.SeekingSpent != 1000 {
		t.Fatal(u.Points, w)
	}
}

func TestSeekingLastFireStopsBatch(t *testing.T) {
	db, now := fixture(t)
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1 AND id <> 23").Update("owner_id", 2).Error)
	q, err := SeekingQuote(db, 1, "seek", 10, now)
	must(t, err)
	must(t, seekConfirmWithRoll(db, q, now, func(lo, hi int) int { return lo }))
	u, w := seekingState(t, db, 1)
	if u.Points != 99900 || w.Fragments != 1 || w.Scrolls != 101 || w.SeekingSpent != 100 {
		t.Fatal(u.Points, w)
	}
	var fire model.SpiritFire
	must(t, db.First(&fire, 23).Error)
	if fire.OwnerID != 1 {
		t.Fatal(fire)
	}
	var receipt model.SpiritFireQuote
	must(t, db.First(&receipt, "id = ?", q.ID).Error)
	var result SeekingResult
	must(t, json.Unmarshal([]byte(receipt.ResultJSON), &result))
	if result.Count != 1 || len(result.Fires) != 1 || result.Fires[0].ID != 23 {
		t.Fatal(result)
	}
	_, err = SeekingQuote(db, 1, "seek", 1, now)
	if !errors.Is(err, ErrSeekingEmpty) {
		t.Fatal(err)
	}
	// Direct purchase continues after the fire pool is exhausted.
	buy, err := SeekingQuote(db, 1, "scroll_buy", 10, now)
	must(t, err)
	must(t, Confirm(db, 1, buy.ID, now))
	u, w = seekingState(t, db, 1)
	if u.Points != 94900 || w.Scrolls != 111 || w.SeekingSpent != 5100 {
		t.Fatal(u.Points, w)
	}
}

func TestSeekingFailureDoesNotConsume(t *testing.T) {
	db, now := fixture(t)
	q, err := SeekingQuote(db, 1, "seek", 10, now)
	must(t, err)
	must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("points", 999).Error)
	rolled := false
	err = seekConfirmWithRoll(db, q, now, func(lo, hi int) int { rolled = true; return lo })
	if !errors.Is(err, storage.ErrPointsNotEnough) || rolled {
		t.Fatalf("err=%v rolled=%v", err, rolled)
	}
	must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("points", 100000).Error)
	// Failure after awards and debits have been attempted must roll back both.
	must(t, db.Exec("CREATE TRIGGER reject_seek_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END").Error)
	err = seekConfirmWithRoll(db, q, now, func(lo, hi int) int { return lo })
	if err == nil {
		t.Fatal("expected audit failure")
	}
	u, w := seekingState(t, db, 1)
	if u.Points != 100000 || w.Fragments != 0 || w.Scrolls != 100 {
		t.Fatal(u.Points, w)
	}
	var count int64
	must(t, db.Model(&model.SpiritFire{}).Where("owner_id = 1").Count(&count).Error)
	if count != 0 {
		t.Fatal("leaked fire", count)
	}
	var saved model.SpiritFireQuote
	must(t, db.First(&saved, "id = ?", q.ID).Error)
	if saved.Done {
		t.Fatal("failed quote completed")
	}
	must(t, db.Exec("DROP TRIGGER reject_seek_audit").Error)
	merge, err := SeekingQuote(db, 1, "scroll_merge", 1, now)
	must(t, err)
	if err = Confirm(db, 1, merge.ID, now); err == nil || !strings.Contains(err.Error(), "残页不足") {
		t.Fatal(err)
	}
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1").Update("owner_id", 2).Error)
	if err = Confirm(db, 1, q.ID, now); !errors.Is(err, ErrSeekingEmpty) {
		t.Fatal(err)
	}
	u, w = seekingState(t, db, 1)
	if u.Points != 100000 || w.Scrolls != 100 || w.Fragments != 0 {
		t.Fatal(u.Points, w)
	}
}

func TestSeekingAuthorizationExpiryAndConcurrentLastFire(t *testing.T) {
	db, now := fixture(t)
	for _, bad := range []int{-1, 0, 2, 100} {
		if _, err := SeekingQuote(db, 1, "seek", bad, now); err == nil {
			t.Fatal("accepted quantity", bad)
		}
	}
	q, err := SeekingQuote(db, 1, "seek", 1, now)
	must(t, err)
	if err := Confirm(db, 2, q.ID, now); err == nil {
		t.Fatal("another user confirmed")
	}
	if err := Confirm(db, 1, q.ID, now.Add(16*time.Minute)); err == nil {
		t.Fatal("expired quote accepted")
	}
	must(t, db.Model(&model.Cultivation{}).Where("user_id = 1").Update("major_realm", 0).Error)
	if err := Confirm(db, 1, q.ID, now); !errors.Is(err, ErrIneligible) {
		t.Fatal(err)
	}
	must(t, db.Model(&model.Cultivation{}).Where("user_id = 1").Update("major_realm", 2).Error)
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1 AND id <> 23").Update("owner_id", 3).Error)
	a, err := SeekingQuote(db, 1, "seek", 1, now)
	must(t, err)
	b, err := SeekingQuote(db, 2, "seek", 1, now)
	must(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, quote := range []model.SpiritFireQuote{a, b} {
		wg.Add(1)
		go func(q model.SpiritFireQuote) {
			defer wg.Done()
			errs <- seekConfirmWithRoll(db, q, now, func(lo, hi int) int { return lo })
		}(quote)
	}
	wg.Wait()
	close(errs)
	wins, empty := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrSeekingEmpty) {
			empty++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || empty != 1 {
		t.Fatal(wins, empty)
	}
	u1, w1 := seekingState(t, db, 1)
	u2, w2 := seekingState(t, db, 2)
	if u1.Points+u2.Points != 199900 || w1.Fragments+w2.Fragments != 1 {
		t.Fatal(u1.Points, u2.Points, w1, w2)
	}
}

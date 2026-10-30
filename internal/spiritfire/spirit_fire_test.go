package spiritfire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixture(t *testing.T) (*gorm.DB, time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fire.db")+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sql.SetMaxOpenConns(4)
	t.Cleanup(func() { sql.Close() })
	all := append(Models(), &model.User{}, &model.Cultivation{}, &model.ListeningAbuseRecord{}, &model.PointTransaction{}, &model.AuditLog{}, &model.DailyListeningStat{}, &model.MarketplaceListing{}, &model.MarketplaceSecret{})
	if err := db.AutoMigrate(all...); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, Location)
	if err := Initialize(db, now); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 3; id++ {
		must(t, db.Create(&model.User{TelegramID: id, Username: fmt.Sprint("u", id), AbsUserID: fmt.Sprint("abs", id), IsWhitelist: true, Points: 100000, AccountType: "formal"}).Error)
		must(t, db.Create(&model.Cultivation{UserID: id, MajorRealm: 2}).Error)
		must(t, db.Create(&model.SpiritFireWallet{UserID: id, Scrolls: 100}).Error)
	}
	return db, now
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func own(t *testing.T, db *gorm.DB, id uint, user int64, now time.Time) {
	t.Helper()
	must(t, db.Model(&model.SpiritFire{}).Where("id = ?", id).Updates(map[string]any{"owner_id": user, "acquired_at": now}).Error)
}
func learn(t *testing.T, db *gorm.DB, user int64, id uint, now time.Time) {
	t.Helper()
	q, err := Quote(db, user, id, "learn", now)
	must(t, err)
	must(t, Confirm(db, user, q.ID, now))
	must(t, Confirm(db, user, q.ID, now))
}

func TestCatalogAndBonus(t *testing.T) {
	db, _ := fixture(t)
	var fs []model.SpiritFire
	must(t, db.Find(&fs).Error)
	if len(fs) != 23 {
		t.Fatal(len(fs))
	}
	sum := 0
	r := DefaultRules()
	for _, f := range fs {
		if f.Rank > 1 {
			sum += r.Percents[f.Grade]
		}
	}
	if sum != 186 {
		t.Fatal(sum)
	}
	for _, tc := range []struct {
		raw, base float64
		p         int
		want      float64
	}{{0, 0, 186, 0}, {24, 8.5, 186, 28.83}, {30, 8.5, 186, 28.83}, {8, 6.4, 50, .8}, {8, 7.2, 50, .4}, {18, 8.5, 186, 17.67}, {4, 4, 186, 0}, {8, 6.4, 0, 0}} {
		if got := Bonus(tc.raw*3600, tc.base, tc.p); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
func TestPermanentMasteryAndStaleQuotes(t *testing.T) {
	db, now := fixture(t)
	own(t, db, 19, 1, now)
	own(t, db, 14, 1, now)
	own(t, db, 15, 1, now)
	qA, err := Quote(db, 1, 19, "learn", now)
	must(t, err)
	qB, err := Quote(db, 1, 14, "learn", now)
	must(t, err)
	must(t, Confirm(db, 1, qA.ID, now))
	if err := Confirm(db, 1, qB.ID, now); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale free quote: %v", err)
	}
	// Selling the first flame does not lower the historical count.
	own(t, db, 19, 2, now.Add(time.Minute))
	qB, err = Quote(db, 1, 14, "learn", now)
	must(t, err)
	if qB.Points != 1500 || qB.Scrolls != 1 {
		t.Fatal(qB)
	}
	must(t, Confirm(db, 1, qB.ID, now))
	must(t, Confirm(db, 1, qB.ID, now))
	own(t, db, 19, 1, now.Add(2*time.Minute))
	if _, err := Quote(db, 1, 19, "learn", now); err == nil {
		t.Fatal("relearning should not be offered")
	}
	qC, err := Quote(db, 1, 15, "learn", now)
	must(t, err)
	if qC.Points != 3000 || qC.Scrolls != 2 {
		t.Fatal(qC)
	}
	var u model.User
	must(t, db.Where("telegram_id = 1").First(&u).Error)
	if u.Points != 98500 {
		t.Fatal(u.Points)
	}
	var w model.SpiritFireWallet
	must(t, db.First(&w, 1).Error)
	if w.Scrolls != 99 {
		t.Fatal(w.Scrolls)
	}
}
func TestConcurrentLearningCannotDoubleFree(t *testing.T) {
	db, now := fixture(t)
	own(t, db, 19, 1, now)
	own(t, db, 14, 1, now)
	a, err := Quote(db, 1, 19, "learn", now)
	must(t, err)
	b, err := Quote(db, 1, 14, "learn", now)
	must(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, q := range []model.SpiritFireQuote{a, b} {
		wg.Add(1)
		go func(q model.SpiritFireQuote) { defer wg.Done(); errs <- Confirm(db, 1, q.ID, now) }(q)
	}
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, ErrChanged) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
}
func TestDailySnapshotHistoryAndFullFusion(t *testing.T) {
	db, now := fixture(t)
	ids := []uint{}
	for id := uint(2); id <= 23; id++ {
		own(t, db, id, 1, now)
		must(t, db.Create(&model.SpiritFireMastery{UserID: 1, FireID: id, CreatedAt: now}).Error)
		ids = append(ids, id)
	}
	must(t, Equip(db, 1, ids, now))
	must(t, db.Transaction(func(tx *gorm.DB) error {
		must(t, Lock(tx))
		s, err := Snapshot(tx, 1, Day(now))
		must(t, err)
		if s.Percent != 0 {
			t.Fatal(s)
		}
		return nil
	}))
	next := now.AddDate(0, 0, 1)
	stat := model.DailyListeningStat{UserID: 1, DayKey: Day(next), RawSeconds: 24 * 3600, EffectiveHours: 8.5}
	must(t, db.Create(&stat).Error)
	must(t, db.Transaction(func(tx *gorm.DB) error { must(t, Lock(tx)); return RefreshDaily(tx, &stat, 8.5, next) }))
	if math.Abs(stat.SpiritFireHours+8.5-37.33) > 1e-9 {
		t.Fatal(stat)
	}
	must(t, Equip(db, 1, nil, next))
	own(t, db, 19, 2, next)
	// Historical raw/retreat corrections recompute using the old immutable rate.
	stat.RawSeconds = 8 * 3600
	stat.EffectiveHours = 6.4
	must(t, db.Transaction(func(tx *gorm.DB) error { must(t, Lock(tx)); return RefreshDaily(tx, &stat, 7.2, next.AddDate(0, 0, 1)) }))
	if math.Abs(stat.SpiritFireHours-2.976) > 1e-9 {
		t.Fatal(stat.SpiritFireHours)
	}
	must(t, db.Transaction(func(tx *gorm.DB) error {
		must(t, Lock(tx))
		s, err := Snapshot(tx, 1, Day(next.AddDate(0, 0, 1)))
		must(t, err)
		if s.Percent != 0 {
			t.Fatal(s)
		}
		return nil
	}))
	must(t, db.Transaction(func(tx *gorm.DB) error {
		s, err := Snapshot(tx, 1, Day(now.AddDate(0, 0, -1)))
		must(t, err)
		if s.Percent != 0 || s.ID != 0 {
			t.Fatal(s)
		}
		return nil
	}))
}
func TestLearningFailureRollsBackScrollsAndAudit(t *testing.T) {
	db, now := fixture(t)
	own(t, db, 19, 1, now)
	own(t, db, 14, 1, now)
	learn(t, db, 1, 19, now)
	q, err := Quote(db, 1, 14, "learn", now)
	must(t, err)
	must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("points", 0).Error)
	if err := Confirm(db, 1, q.ID, now); err == nil {
		t.Fatal("expected insufficient points")
	}
	var w model.SpiritFireWallet
	must(t, db.First(&w, 1).Error)
	if w.Scrolls != 100 {
		t.Fatal(w)
	}
	n, err := MasteryCount(db, 1)
	must(t, err)
	if n != 1 {
		t.Fatal(n)
	}
	must(t, db.Exec("DROP TABLE spirit_fire_masteries").Error)
	if _, err := Quote(db, 1, 14, "learn", now); err == nil {
		t.Fatal("read error treated as zero masteries")
	}
}
func TestEventSnapshotAndIdempotentAwards(t *testing.T) {
	db, now := fixture(t)
	rules := DefaultRules()
	rules.BossChance = 100
	rules.ScrollChance = 100
	must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(rules)).Error)
	must(t, PrepareEvent(db, "boss:one", now))
	must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(DefaultRules())).Error)
	// An existing owner and a nonowner are both eligible, duplicates ignored.
	own(t, db, 19, 1, now)
	for i := 0; i < 2; i++ {
		must(t, db.Transaction(func(tx *gorm.DB) error { return AwardInTx(tx, "boss:one", []int64{1, 1, 2}, true, now) }))
	}
	var event model.SpiritFireEvent
	must(t, db.First(&event, "id = ?", "boss:one").Error)
	if !event.Settled {
		t.Fatal(event)
	}
	var n int64
	must(t, db.Model(&model.SpiritFire{}).Where("owner_id <> 0").Count(&n).Error)
	if n != 2 {
		t.Fatal(n)
	}
	for _, id := range []int64{1, 2} {
		var w model.SpiritFireWallet
		must(t, db.First(&w, id).Error)
		if w.Scrolls != 101 {
			t.Fatal(w)
		}
	}
	// When every flame is held, scrolls still drop.
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1").Update("owner_id", 1).Error)
	must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(rules)).Error)
	must(t, PrepareEvent(db, "realm:all", now))
	must(t, db.Transaction(func(tx *gorm.DB) error { return AwardInTx(tx, "realm:all", []int64{2}, false, now) }))
	var w model.SpiritFireWallet
	must(t, db.First(&w, 2).Error)
	if w.Scrolls != 102 {
		t.Fatal(w)
	}
}
func TestCashoutAndInactiveLifecycle(t *testing.T) {
	db, now := fixture(t)
	own(t, db, 19, 1, now)
	learn(t, db, 1, 19, now)
	q, err := Quote(db, 1, 19, "cashout", now)
	must(t, err)
	if q.Points != 1200 {
		t.Fatal(q)
	}
	must(t, Confirm(db, 1, q.ID, now))
	must(t, Confirm(db, 1, q.ID, now))
	var u model.User
	must(t, db.Where("telegram_id = 1").First(&u).Error)
	if u.Points != 101200 {
		t.Fatal(u.Points)
	}
	n, err := MasteryCount(db, 1)
	must(t, err)
	if n != 1 {
		t.Fatal(n)
	}
	own(t, db, 19, 1, now.AddDate(0, 0, -31))
	stat := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: Day(now), LastFetchedAt: now, RawSeconds: 0}
	must(t, db.Create(&stat).Error)
	must(t, SweepOwner(db, 1, false, now))
	var f model.SpiritFire
	must(t, db.First(&f, 19).Error)
	if f.GraceAt != nil {
		t.Fatal("unrefreshed owner warned")
	}
	must(t, SweepOwner(db, 1, true, now))
	must(t, db.First(&f, 19).Error)
	if f.GraceAt == nil {
		t.Fatal("grace missing")
	}
	later := now.AddDate(0, 0, 8)
	must(t, db.Model(&stat).Update("last_fetched_at", later).Error)
	must(t, SweepOwner(db, 1, true, later))
	must(t, db.First(&f, 19).Error)
	if f.OwnerID != 0 {
		t.Fatal(f)
	}
}

func TestRulesConfirmationNextDayAndEligibilityFailures(t *testing.T) {
	db, now := fixture(t)
	if _, err := RulesQuote(db, 1, DefaultRules(), now); err == nil {
		t.Fatal("ordinary user configured rewards")
	}
	must(t, db.Model(&model.User{}).Where("telegram_id = 1").Update("role", "super_admin").Error)
	r := DefaultRules()
	r.BossChance = 100
	r.ScrollChance = 100
	r.Percents[3] = 20
	q, err := RulesQuote(db, 1, r, now)
	must(t, err)
	must(t, Confirm(db, 1, q.ID, now))
	must(t, Confirm(db, 1, q.ID, now))
	today, err := RulesAt(db, Day(now))
	must(t, err)
	if today.BossChance != 1 {
		t.Fatal(today)
	}
	next, err := RulesAt(db, Tomorrow(now))
	must(t, err)
	if next.BossChance != 100 || next.Percents[3] != 20 {
		t.Fatal(next)
	}
	for _, raw := range []string{`{}`, `{"boss_percent":1,"realm_percent":1,"scroll_percent":10,"prices":[1,2,3,4],"percents":[5,8,10]}`, `{"boss_percent":101,"realm_percent":1,"scroll_percent":10,"prices":[1,2,3,4],"percents":[5,8,10,12]}`} {
		if _, err := ParseRules(raw); err == nil {
			t.Fatal("invalid config accepted", raw)
		}
	}
	must(t, db.Model(&model.User{}).Where("telegram_id = 2").Update("account_type", "trial").Error)
	ok, err := Eligible(db, 2, now)
	must(t, err)
	if ok {
		t.Fatal("trial eligible")
	}
	must(t, db.Model(&model.Cultivation{}).Where("user_id = 3").Update("major_realm", 1).Error)
	ok, err = Eligible(db, 3, now)
	must(t, err)
	if ok {
		t.Fatal("low realm eligible")
	}
	must(t, db.Exec("DROP TABLE listening_abuse_records").Error)
	if _, err := Eligible(db, 1, now); err == nil {
		t.Fatal("account read failure ignored")
	}
}
func TestConcurrentEventsCannotDuplicateLastFlame(t *testing.T) {
	db, now := fixture(t)
	r := DefaultRules()
	r.BossChance = 100
	r.ScrollChance = 0
	must(t, db.Model(&model.SpiritFireRules{}).Where("effective_day = ?", Day(now)).Update("json", Encode(r)).Error)
	must(t, db.Model(&model.SpiritFire{}).Where("rank > 1 AND id <> 19").Updates(map[string]any{"owner_id": 3, "acquired_at": now}).Error)
	must(t, PrepareEvent(db, "boss:a", now))
	must(t, PrepareEvent(db, "boss:b", now))
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"boss:a", "boss:b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			errs <- db.Transaction(func(tx *gorm.DB) error { return AwardInTx(tx, key, []int64{1, 2}, true, now) })
		}(key)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	var events []model.SpiritFireEvent
	must(t, db.Find(&events).Error)
	wins := 0
	for _, e := range events {
		var result AwardResult
		must(t, json.Unmarshal([]byte(e.ResultJSON), &result))
		if result.FireID != 0 {
			wins++
			if result.FireID != 19 {
				t.Fatal(result)
			}
		}
	}
	if wins != 1 {
		t.Fatal("last flame awarded multiple times", wins)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AwardInTx(tx, "boss:missing", []int64{1}, true, now) }); err == nil {
		t.Fatal("missing activity snapshot ignored")
	}
}
func TestGraceRecoveryAndDeletedAccountPreserveMastery(t *testing.T) {
	db, now := fixture(t)
	own(t, db, 19, 1, now.AddDate(0, 0, -31))
	learn(t, db, 1, 19, now)
	stat := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: Day(now), LastFetchedAt: now}
	must(t, db.Create(&stat).Error)
	must(t, SweepOwner(db, 1, true, now))
	must(t, db.Model(&stat).Update("raw_seconds", 600).Error)
	must(t, SweepOwner(db, 1, true, now.Add(time.Hour)))
	var f model.SpiritFire
	must(t, db.First(&f, 19).Error)
	if f.OwnerID != 1 || f.GraceAt != nil {
		t.Fatal(f)
	}
	must(t, db.Where("telegram_id = 1").Delete(&model.User{}).Error)
	must(t, SweepOwner(db, 1, false, now.Add(2*time.Hour)))
	must(t, db.First(&f, 19).Error)
	if f.OwnerID != 0 {
		t.Fatal(f)
	}
	n, err := MasteryCount(db, 1)
	must(t, err)
	if n != 1 {
		t.Fatal(n)
	}
}

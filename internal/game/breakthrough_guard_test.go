package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupGuardTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "guards.db")+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection.SetMaxOpenConns(1)
	previousDB := storage.DB
	previousRules := GetCultivationRules()
	storage.DB = db
	rules := buildDefaultCultivationRuleSet("guard_test")
	cultivationRuleCache.mu.Lock()
	cultivationRuleCache.rules = rules
	cultivationRuleCache.mu.Unlock()
	t.Cleanup(func() {
		storage.DB = previousDB
		cultivationRuleCache.mu.Lock()
		cultivationRuleCache.rules = previousRules
		cultivationRuleCache.mu.Unlock()
		_ = connection.Close()
	})
	if err := db.AutoMigrate(&model.User{}, &model.Cultivation{}, &model.Inventory{}, &model.BreakthroughRitual{}, &model.BreakthroughGuard{}, &model.BreakthroughAttempt{}, &model.PointTransaction{}, &model.AuditLog{}, &model.ListeningAbuseRecord{}, &model.SectMember{}, &model.Sect{}, &model.SectContributionLog{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX idx_guard_active_test ON breakthrough_rituals(user_id) WHERE status IN ('publishing','waiting')`).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 12; id++ {
		if err := db.Create(&model.User{TelegramID: id, Username: fmt.Sprint("user", id), AbsUserID: fmt.Sprint("abs", id), Points: 100, Status: "active", AccountType: "formal"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Cultivation{UserID: id, MajorRealm: 2, MinorRealm: 4, TotalAudioTime: 150, ConsolidateUntil: time.Now().Add(-time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 1).Update("points", 5000).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]interface{}{"major_realm": 5, "total_audio_time": 1500}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Inventory{UserID: 1, ItemName: CultivationTreasureNameForRealm(5), Quantity: 5}).Error; err != nil {
		t.Fatal(err)
	}
}

func guardTestRitual(t *testing.T) *model.BreakthroughRitual {
	t.Helper()
	ritual, err := CreateBreakthroughRitual(1, "道友", 1, -100, "USE_INVENTORY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateBreakthroughRitual(ritual.ID, 123, time.Now()); err != nil {
		t.Fatal(err)
	}
	return ritual
}

func guardTestJoin(id uint, userID int64) error {
	return JoinBreakthroughGuard(id, -100, 123, &tgbotapi.User{ID: userID, FirstName: fmt.Sprint("user", userID)}, time.Now())
}

func guardTestBalance(t *testing.T, id int64) int {
	t.Helper()
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", id).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user.Points
}

func TestBreakthroughGuardRates(t *testing.T) {
	for _, item := range []struct {
		base  float64
		count int
		want  float64
	}{{.1, 0, .1}, {.1, 1, .11}, {.1, 5, .15}, {.1, 8, .15}, {.8, 5, .85}, {.94, 5, .95}, {1, 0, 1}} {
		if got := BreakthroughGuardSuccessRate(item.base, item.count); got != item.want {
			t.Errorf("rate(%v,%d)=%v want %v", item.base, item.count, got, item.want)
		}
	}
}

func TestBreakthroughGuardsAtomicOutcomesAndNoBystanderPenalty(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			setupGuardTestDB(t)
			ritual := guardTestRitual(t)
			if _, err := ResolveBreakthrough(1, "USE_INVENTORY", 0, time.Now()); !errors.Is(err, ErrBreakthroughRitualPending) {
				t.Fatalf("parallel attempt: %v", err)
			}
			if err := guardTestJoin(ritual.ID, 1); !errors.Is(err, ErrBreakthroughGuardSelf) {
				t.Fatalf("self join: %v", err)
			}
			for id := int64(2); id <= 6; id++ {
				if err := guardTestJoin(ritual.ID, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := guardTestJoin(ritual.ID, 7); !errors.Is(err, ErrBreakthroughRitualClosed) && !errors.Is(err, ErrBreakthroughGuardFull) {
				t.Fatalf("sixth participant: %v", err)
			}
			roll := 16
			if success {
				roll = 15
			}
			result, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", ritual.ID, time.Now(), func() (int, error) { return roll, nil })
			if err != nil || result.IsSuccess != success || result.GuardCount != 5 || result.SuccessRate != .15 {
				t.Fatalf("settlement: %+v %v", result, err)
			}
			want := 95
			if success {
				want = 105
			}
			for id := int64(2); id <= 6; id++ {
				if got := guardTestBalance(t, id); got != want {
					t.Fatalf("helper %d balance %d want %d", id, got, want)
				}
			}
			if got := guardTestBalance(t, 7); got != 100 {
				t.Fatalf("bystander was charged: %d", got)
			}
			if _, err := ResolveBreakthrough(1, "USE_INVENTORY", ritual.ID, time.Now()); !errors.Is(err, ErrBreakthroughRitualClosed) {
				t.Fatalf("replayed settlement: %v", err)
			}
			var attempt model.BreakthroughAttempt
			if err := storage.DB.First(&attempt, result.AttemptID).Error; err != nil {
				t.Fatal(err)
			}
			if attempt.SuccessRate != .15 || attempt.BaseSuccessRate != .1 || attempt.GuardBonusPercent != 5 {
				t.Fatalf("incorrect probability audit: %+v", attempt)
			}
			var count int64
			storage.DB.Model(&model.BreakthroughAttempt{}).Count(&count)
			if count != 1 {
				t.Fatalf("duplicate attempts: %d", count)
			}
		})
	}
}

func TestBreakthroughGuardCancellationAndDuplicateJoin(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	if err := guardTestJoin(ritual.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := guardTestJoin(ritual.ID, 2); !errors.Is(err, ErrBreakthroughGuardJoined) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := JoinBreakthroughGuard(ritual.ID, -200, 123, &tgbotapi.User{ID: 3}, time.Now()); !errors.Is(err, ErrBreakthroughRitualClosed) {
		t.Fatalf("forwarded panel: %v", err)
	}
	if err := CancelBreakthroughRitual(ritual.ID, 2); !errors.Is(err, ErrBreakthroughGuardSelf) {
		t.Fatalf("nonowner cancellation: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := CancelBreakthroughRitual(ritual.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := guardTestBalance(t, 2); got != 100 {
		t.Fatalf("refund duplicated or missing: %d", got)
	}
	if got := guardTestBalance(t, 1); got != 5000 {
		t.Fatalf("cancelled breakthrough cost: %d", got)
	}
}

func TestBreakthroughGuardPayoutFailureRollsBackEntireAttempt(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	if err := guardTestJoin(ritual.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(ritual).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	const hook = "guard_test_payout_failure"
	if err := storage.DB.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if entry, ok := tx.Statement.Dest.(*model.PointTransaction); ok && entry.Type == "breakthrough_guard_reward" {
			tx.AddError(errors.New("injected payout failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", ritual.ID, time.Now(), func() (int, error) { return 1, nil })
	storage.DB.Callback().Create().Remove(hook)
	if err == nil {
		t.Fatal("injected failure did not abort transaction")
	}
	if guardTestBalance(t, 1) != 5000 || guardTestBalance(t, 2) != 95 {
		t.Fatal("failed transaction changed wallets")
	}
	var count int64
	storage.DB.Model(&model.BreakthroughAttempt{}).Count(&count)
	if count != 0 {
		t.Fatal("failed transaction persisted attempt")
	}
	var item model.Inventory
	if err := storage.DB.Where("user_id = ?", 1).First(&item).Error; err != nil || item.Quantity != 5 {
		t.Fatal("failed transaction consumed treasure", err)
	}
	if _, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", ritual.ID, time.Now(), func() (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if guardTestBalance(t, 2) != 105 {
		t.Fatal("retry did not settle once")
	}
}

func TestBreakthroughGuardDailyLimitAndGuarantee(t *testing.T) {
	setupGuardTestDB(t)
	for i := 0; i < 3; i++ {
		ritual := guardTestRitual(t)
		if err := guardTestJoin(ritual.ID, 2); err != nil {
			t.Fatal(err)
		}
		if err := storage.DB.Model(ritual).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", ritual.ID, time.Now(), func() (int, error) { return 100, nil }); err != nil {
			t.Fatal(err)
		}
		storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("consolidate_until", time.Now().Add(-time.Hour))
		storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("points", 5000)
	}
	ritual := guardTestRitual(t)
	if err := guardTestJoin(ritual.ID, 2); !errors.Is(err, ErrBreakthroughGuardDailyLimit) {
		t.Fatalf("daily limit: %v", err)
	}
	if err := CancelBreakthroughRitual(ritual.ID, 1); err != nil {
		t.Fatal(err)
	}
	storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("tribulation_fails", 10)
	if _, err := CreateBreakthroughRitual(1, "道友", 1, -100, "USE_INVENTORY", time.Now()); !errors.Is(err, ErrBreakthroughGuardUnavailable) {
		t.Fatalf("guaranteed recruitment: %v", err)
	}
	if guardTestBalance(t, 2) != 85 {
		t.Fatal("quota rejection charged helper")
	}
}

func TestBreakthroughGuardConcurrentCapacity(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	var group sync.WaitGroup
	for id := int64(2); id <= 12; id++ {
		group.Add(1)
		go func(id int64) { defer group.Done(); _ = guardTestJoin(ritual.ID, id) }(id)
	}
	group.Wait()
	var count int64
	if err := storage.DB.Model(&model.BreakthroughGuard{}).Where("ritual_id = ?", ritual.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("capacity not enforced: %d", count)
	}
	total := 0
	for id := int64(2); id <= 12; id++ {
		total += guardTestBalance(t, id)
	}
	if total != 1100-25 {
		t.Fatalf("concurrent joining lost points: %d", total)
	}
}

func TestBreakthroughGuardRestartAndFrozenRules(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	if err := guardTestJoin(ritual.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(ritual).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	// A fresh DB handle has no transient sessions or in-memory recruitment state.
	dsn := storage.DB.Dialector.(*sqlite.Dialector).DSN
	oldConnection, _ := storage.DB.DB()
	if err := oldConnection.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := reopened.DB()
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	storage.DB = reopened
	cultivationRuleCache.mu.Lock()
	changed := cultivationRuleCache.rules.Breakthroughs[5]
	changed.SuccessRate = .5
	changed.PointsCost = 3000
	cultivationRuleCache.rules.Breakthroughs[5] = changed
	cultivationRuleCache.mu.Unlock()
	result, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", ritual.ID, time.Now(), func() (int, error) { return 11, nil })
	if err != nil || !result.IsSuccess || result.SuccessRate != .11 {
		t.Fatalf("restart lost frozen rules: %+v %v", result, err)
	}
	if guardTestBalance(t, 1) != 3800 || guardTestBalance(t, 2) != 105 {
		t.Fatal("restart or new config changed payment")
	}
}

func TestBreakthroughGuardRejectsExpiredAccountsAndBrokenSnapshot(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	expired := time.Now().Add(-time.Hour)
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Update("expire_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	if err := guardTestJoin(ritual.ID, 2); !errors.Is(err, ErrBreakthroughGuardIneligible) {
		t.Fatalf("expired helper accepted: %v", err)
	}
	if guardTestBalance(t, 2) != 100 {
		t.Fatal("ineligible helper charged")
	}
	if err := guardTestJoin(ritual.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(ritual).Updates(map[string]interface{}{"rules_json": "{}", "deadline": time.Now().Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveBreakthrough(1, "USE_INVENTORY", ritual.ID, time.Now()); !errors.Is(err, ErrBreakthroughGuardRulesInvalid) {
		t.Fatalf("broken rules executed: %v", err)
	}
	if err := CancelBreakthroughRitual(ritual.ID, 1); err != nil {
		t.Fatal(err)
	}
	if guardTestBalance(t, 3) != 100 {
		t.Fatal("broken snapshot cancellation lost stake")
	}
}

func TestBreakthroughGuardRequiredResourceLossCancelsSafely(t *testing.T) {
	setupGuardTestDB(t)
	ritual := guardTestRitual(t)
	if err := guardTestJoin(ritual.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.Inventory{}).Where("user_id = ?", 1).Update("quantity", 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(ritual).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	_, err := ResolveBreakthrough(1, "USE_INVENTORY", ritual.ID, time.Now())
	if !errors.Is(err, storage.ErrNoBreakthroughPill) || !isBreakthroughCancellationError(err) {
		t.Fatalf("resource loss not cancellable: %v", err)
	}
	if err := CancelBreakthroughRitual(ritual.ID, 1); err != nil {
		t.Fatal(err)
	}
	if guardTestBalance(t, 1) != 5000 || guardTestBalance(t, 2) != 100 {
		t.Fatal("failed preflight consumed funds")
	}
}

func TestBreakthroughGuardAdmissionIsCertainAndTimeBound(t *testing.T) {
	setupGuardTestDB(t)
	// A guaranteed result must not offer a risk-free reward to helpers.
	storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("tribulation_fails", 10)
	if _, err := CreateBreakthroughRitual(1, "道友", 1, -100, "USE_INVENTORY", time.Now()); !errors.Is(err, ErrBreakthroughGuardUnavailable) {
		t.Fatalf("guarantee admitted: %v", err)
	}
	storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("tribulation_fails", 0)
	cultivationRuleCache.mu.Lock()
	cfg := cultivationRuleCache.rules.Breakthroughs[5]
	cfg.SuccessRate = 1
	cultivationRuleCache.rules.Breakthroughs[5] = cfg
	cultivationRuleCache.mu.Unlock()
	if _, err := CreateBreakthroughRitual(1, "道友", 1, -100, "USE_INVENTORY", time.Now()); !errors.Is(err, ErrBreakthroughGuardUnavailable) {
		t.Fatalf("certain result admitted: %v", err)
	}
	cultivationRuleCache.mu.Lock()
	cfg.SuccessRate = .1
	cultivationRuleCache.rules.Breakthroughs[5] = cfg
	cultivationRuleCache.mu.Unlock()
	ritual := guardTestRitual(t)
	if _, err := ResolveBreakthrough(1, "USE_INVENTORY", ritual.ID, time.Now()); !errors.Is(err, ErrBreakthroughRitualNotDue) {
		t.Fatalf("early settlement: %v", err)
	}
	if err := storage.DB.Model(ritual).Update("deadline", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := guardTestJoin(ritual.ID, 2); !errors.Is(err, ErrBreakthroughRitualClosed) {
		t.Fatalf("late join: %v", err)
	}
	// The snapshot itself retains the original rate even if global config changes.
	var snapshot BreakthroughSetting
	if err := json.Unmarshal([]byte(ritual.RulesJSON), &snapshot); err != nil || snapshot.SuccessRate != .1 {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
}

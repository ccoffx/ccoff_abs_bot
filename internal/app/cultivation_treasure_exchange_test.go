package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

func setupCultivationTreasureTestDB(t *testing.T) {
	t.Helper()
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.Inventory{}, &model.CultivationTreasureExchange{}, &model.AuditLog{},
		&model.SectMember{}, &model.Sect{}, &model.SectTechnology{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_inventory_user_item_unique ON inventories(user_id,item_name) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Cultivation{UserID: 1, MajorRealm: 5, MinorRealm: 4}).Error; err != nil {
		t.Fatal(err)
	}
	if err := game.GardenGrantInventoryInTx(storage.DB, 1, game.SectSecretRealmTokenItemName, 3); err != nil {
		t.Fatal(err)
	}
}

func exchangeTestQuantity(t *testing.T, name string) int {
	t.Helper()
	var total int
	if err := storage.DB.Model(&model.Inventory{}).Select("COALESCE(SUM(quantity),0)").Where("user_id = ? AND item_name = ?", 1, name).Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	return total
}

func prepareExchangeForTest(t *testing.T) *model.CultivationTreasureExchange {
	t.Helper()
	exchange, err := game.PrepareCultivationTreasureExchange(storage.DB, 1)
	if err != nil {
		t.Fatal(err)
	}
	return exchange
}

func TestCultivationTreasureExchangeAllRealms(t *testing.T) {
	for major := 5; major <= 12; major++ {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			setupCultivationTreasureTestDB(t)
			if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", major).Error; err != nil {
				t.Fatal(err)
			}
			name := game.CultivationTreasureNameForRealm(major)
			if err := game.GardenGrantInventoryInTx(storage.DB, 1, name, 2); err != nil {
				t.Fatal(err)
			}
			exchange := prepareExchangeForTest(t)
			if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 3 {
				t.Fatal("preview charged tokens")
			}
			result, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID)
			if err != nil || result == nil || result.TreasureName != name || result.Status != game.CultivationTreasureExchangeCompleted {
				t.Fatalf("incorrect exchange: %+v, %v", result, err)
			}
			if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 2 || exchangeTestQuantity(t, name) != 3 || treasureTestBalance(t, 1) != 100 {
				t.Fatal("exchange changed points, lost existing treasure, or used incorrect cost")
			}
			if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); !errors.Is(err, game.ErrCultivationTreasureCompleted) {
				t.Fatalf("replayed confirmation: %v", err)
			}
			if exchangeTestQuantity(t, name) != 3 || treasureTestCount(t, &model.AuditLog{}) != 1 {
				t.Fatal("replayed confirmation duplicated assets or audit")
			}
		})
	}
}

func TestCultivationTreasureExchangeRejectsUnavailableAndFailedReads(t *testing.T) {
	setupCultivationTreasureTestDB(t)
	if _, err := game.PrepareCultivationTreasureExchange(storage.DB, 2); !errors.Is(err, game.ErrCultivationTreasureNoProfile) {
		t.Fatalf("missing profile: %v", err)
	}
	for _, major := range []int{-1, 0, 1, 2, 3, 4, 13, 14} {
		if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", major).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := game.PrepareCultivationTreasureExchange(storage.DB, 1); !errors.Is(err, game.ErrCultivationTreasureUnavailable) {
			t.Fatalf("unexpected offer for realm %d: %v", major, err)
		}
	}
	if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", 5).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Migrator().DropTable(&model.Inventory{}); err != nil {
		t.Fatal(err)
	}
	_, err := game.PrepareCultivationTreasureExchange(storage.DB, 1)
	if err == nil || errors.Is(err, game.ErrCultivationTreasureTokens) || game.CultivationTreasureExchangeErrorText(err) != "材料兑换暂时不可用，请稍后再试。" {
		t.Fatalf("inventory read failure was hidden as zero balance: %v", err)
	}
}

func TestCultivationTreasureExchangeStaleAndForeignConfirmations(t *testing.T) {
	for _, scenario := range []string{"foreign", "expired", "realm_changed", "tokens_spent", "invalid_id"} {
		t.Run(scenario, func(t *testing.T) {
			setupCultivationTreasureTestDB(t)
			exchange := prepareExchangeForTest(t)
			userID, id := int64(1), exchange.ID
			want := game.ErrCultivationTreasureExpired
			var mutationErr error
			switch scenario {
			case "foreign":
				userID = 2
			case "expired":
				mutationErr = storage.DB.Model(exchange).Update("created_at", time.Now().Add(-16*time.Minute)).Error
			case "realm_changed":
				mutationErr = storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", 6).Error
				want = game.ErrCultivationTreasureRealmChanged
			case "tokens_spent":
				mutationErr = storage.DB.Model(&model.Inventory{}).Where("item_name = ?", game.SectSecretRealmTokenItemName).Update("quantity", 0).Error
				want = game.ErrCultivationTreasureTokens
			case "invalid_id":
				id += 10000
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, userID, id); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if exchangeTestQuantity(t, exchange.TreasureName) != 0 || treasureTestCount(t, &model.AuditLog{}) != 0 {
				t.Fatal("invalid confirmation issued rewards")
			}
			if err := storage.DB.First(exchange, exchange.ID).Error; err != nil || exchange.Status != game.CultivationTreasureExchangePending {
				t.Fatalf("failed confirmation consumed the confirmation record: %+v, %v", exchange, err)
			}
			wantTokens := 3
			if scenario == "tokens_spent" {
				wantTokens = 0
			}
			if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != wantTokens {
				t.Fatal("invalid confirmation consumed tokens")
			}
		})
	}
}

func TestCultivationTreasureExchangeConcurrentConfirmations(t *testing.T) {
	for _, sameConfirmation := range []bool{true, false} {
		t.Run(fmt.Sprint(sameConfirmation), func(t *testing.T) {
			setupCultivationTreasureTestDB(t)
			if err := storage.DB.Model(&model.Inventory{}).Where("item_name = ?", game.SectSecretRealmTokenItemName).Update("quantity", 1).Error; err != nil {
				t.Fatal(err)
			}
			first := prepareExchangeForTest(t)
			ids := make([]uint, 12)
			for i := range ids {
				ids[i] = first.ID
				if !sameConfirmation {
					ids[i] = prepareExchangeForTest(t).ID
				}
			}
			var wg sync.WaitGroup
			outcomes := make(chan error, len(ids))
			for _, id := range ids {
				wg.Add(1)
				go func(id uint) {
					defer wg.Done()
					_, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, id)
					outcomes <- err
				}(id)
			}
			wg.Wait()
			close(outcomes)
			successes := 0
			for err := range outcomes {
				if err == nil {
					successes++
				} else if !errors.Is(err, game.ErrCultivationTreasureCompleted) && !errors.Is(err, game.ErrCultivationTreasureTokens) {
					t.Fatal(err)
				}
			}
			if successes != 1 || exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 0 || exchangeTestQuantity(t, first.TreasureName) != 1 || treasureTestCount(t, &model.AuditLog{}) != 1 {
				t.Fatal("concurrent confirmations duplicated rewards or overdrew tokens")
			}
		})
	}
}

func TestCultivationTreasureExchangeAuditFailureRollsBack(t *testing.T) {
	setupCultivationTreasureTestDB(t)
	exchange := prepareExchangeForTest(t)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_exchange_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'test audit failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); err == nil {
		t.Fatal("audit failure did not abort exchange")
	}
	if exchangeTestQuantity(t, game.SectSecretRealmTokenItemName) != 3 || exchangeTestQuantity(t, exchange.TreasureName) != 0 {
		t.Fatal("audit failure left partial asset changes")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_exchange_audit").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := game.ConfirmCultivationTreasureExchange(storage.DB, 1, exchange.ID); err != nil {
		t.Fatalf("rolled-back confirmation could not be retried: %v", err)
	}
}

func TestCurrentCultivationIntegration(t *testing.T) {
	setupCultivationTreasureTestDB(t)
	for _, row := range telegram.UserMainMenu.Keyboard {
		for _, button := range row {
			if strings.Contains(button.Text, "万灵阁") {
				t.Fatal("retired entry remains in main keyboard")
			}
		}
	}
	// 宗门科技影响 Boss 伤害，听书使用当日统计。
	if err := storage.DB.AutoMigrate(&model.DailyListeningStat{}, &model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []interface{}{
		&model.SectMember{SectID: 1, UserID: 1},
		&model.SectTechnology{SectID: 1, TechKey: game.SectTechBossDamageBonus, Level: 3},
		&model.DailyListeningStat{UserID: 1, DayKey: game.SectDayKey(time.Now()), EffectiveHours: 2},
	} {
		if err := storage.DB.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	damage, multiplier, err := game.ApplyWorldBossDamageBonusesChecked(1, 30)
	if err != nil || damage != 36.9 || multiplier < 1.22999 || multiplier > 1.23001 {
		t.Fatalf("unexpected world boss technology bonus: %f, %f, %v", damage, multiplier, err)
	}
	if hours, ok := game.GetTodaySectListeningHoursFromCache(1, time.Now()); !ok || hours != 2 {
		t.Fatalf("unexpected daily listening hours: %f, %t", hours, ok)
	}
	_, menu := renderFeatureMenu("sect", 1)
	for _, row := range menu.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Text, "护宗神兽") || strings.Contains(button.Text, "宗门运势") || strings.Contains(button.Text, "藏经阁") {
				t.Fatal("retired feature in sect menu")
			}
		}
	}
}

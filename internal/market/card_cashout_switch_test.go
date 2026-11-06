package market

import (
	"errors"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCodeCashoutSwitchBlocksOldQuotesAndSurvivesReopen(t *testing.T) {
	for _, kind := range []string{"invite", "renew"} {
		t.Run(kind, func(t *testing.T) {
			setupCodeOwnershipTest(t)
			raw := "benefit-secret-123456"
			var err error
			if kind == "invite" {
				err = storage.CreateInviteCodeRecord(storage.DB, raw)
			} else {
				err = storage.CreateRenewCodeRecord(storage.DB, raw, 30)
			}
			if err != nil {
				t.Fatal(err)
			}
			quote, err := InspectCodeCashout(raw, 1)
			if err != nil {
				t.Fatal("default should allow cashout", err)
			}
			if err := storage.SetSystemConfigStringChecked(CodeCashoutEnabledKey, "0"); err != nil {
				t.Fatal(err)
			}
			// A fresh connection must see the same closed state, without process state.
			reopened, err := gorm.Open(sqlite.Open(storage.DB.Dialector.(*sqlite.Dialector).DSN), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := reopened.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			storage.DB = reopened
			if _, err := InspectCodeCashout(raw, 1); !errors.Is(err, ErrCodeCashoutDisabled) {
				t.Fatalf("closed quote: %v", err)
			}
			for i := 0; i < 2; i++ {
				if points, err := ExecuteCodeCashout(1, quote); points != 0 || !errors.Is(err, ErrCodeCashoutDisabled) {
					t.Fatalf("old quote bypassed closure: %d %v", points, err)
				}
			}
			var user model.User
			if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 1000 {
				t.Fatalf("closed cashout changed wallet: %+v %v", user, err)
			}
			var used int64
			table := "invite_codes"
			if kind == "renew" {
				table = "renew_codes"
			}
			if err := storage.DB.Table(table).Where("is_used = ? OR cashed_out_at IS NOT NULL", true).Count(&used).Error; err != nil || used != 0 {
				t.Fatalf("closed cashout consumed code: %d %v", used, err)
			}
			if err := storage.SetSystemConfigStringChecked(CodeCashoutEnabledKey, "1"); err != nil {
				t.Fatal(err)
			}
			quote, err = InspectCodeCashout(raw, 1)
			if err != nil {
				t.Fatal("reopened cashout should issue quotes", err)
			}
			if points, err := ExecuteCodeCashout(1, quote); err != nil || points != quote.Points {
				t.Fatalf("cashout did not recover: %d %v", points, err)
			}
			if points, err := ExecuteCodeCashout(1, quote); points != 0 || !errors.Is(err, ErrCodeCashoutInvalid) {
				t.Fatalf("duplicate cashout: %d %v", points, err)
			}
			if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 1000+quote.Points {
				t.Fatalf("cashout credited more than once: %+v %v", user, err)
			}
		})
	}
}

func TestCodeCashoutSwitchReadFailuresBlockCashout(t *testing.T) {
	for _, state := range []string{"invalid", "2", "missing-table"} {
		t.Run(state, func(t *testing.T) {
			setupCodeOwnershipTest(t)
			raw := "benefit-secret-123456"
			if err := storage.CreateInviteCodeRecord(storage.DB, raw); err != nil {
				t.Fatal(err)
			}
			quote, err := InspectCodeCashout(raw, 1)
			if err != nil {
				t.Fatal(err)
			}
			if state == "missing-table" {
				err = storage.DB.Migrator().DropTable(&model.SystemConfig{})
			} else {
				err = storage.SetSystemConfigStringChecked(CodeCashoutEnabledKey, state)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InspectCodeCashout(raw, 1); err == nil {
				t.Fatal("quote ignored config read failure")
			}
			if points, err := ExecuteCodeCashout(1, quote); points != 0 || err == nil {
				t.Fatalf("cashout ignored config read failure: %d %v", points, err)
			}
			var invite model.InviteCode
			if err := storage.DB.First(&invite, quote.RecordID).Error; err != nil || invite.IsUsed || invite.CashedOutAt != nil {
				t.Fatalf("config failure consumed card: %+v %v", invite, err)
			}
			var user model.User
			if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil || user.Points != 1000 {
				t.Fatalf("config failure changed wallet: %+v %v", user, err)
			}
		})
	}
}

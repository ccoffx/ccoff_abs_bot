package market

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
)

func setupCodeOwnershipTest(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ownership.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	oldDB, oldConfig := storage.DB, config.AppConfig
	storage.DB = db
	config.AppConfig = &config.Config{SecurityPepper: strings.Repeat("s", 64), AccountValidDays: 30}
	t.Cleanup(func() { storage.DB = oldDB; config.AppConfig = oldConfig; _ = conn.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.Cultivation{}, &model.InviteCode{}, &model.RenewCode{}, &model.MarketplaceListing{}, &model.MarketplaceSecret{}, &model.MarketplacePurchase{}, &model.SystemConfig{}, &model.PointTransaction{}, &model.AuditLog{}, &model.RedPacket{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX idx_market_active_test ON marketplace_secrets(code_hash) WHERE status = 'available' AND deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 3; id++ {
		if err := db.Create(&model.User{TelegramID: id, Username: fmt.Sprint("user", id), AbsUserID: fmt.Sprint("abs", id), Points: 1000}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Cultivation{UserID: id, MajorRealm: 2}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodeOwnershipExchangeAndBearerCodes(t *testing.T) {
	for _, kind := range []string{"invite", "renew"} {
		t.Run(kind, func(t *testing.T) {
			setupCodeOwnershipTest(t)
			raw := "exchange-secret-12345"
			var err error
			if kind == "invite" {
				err = storage.CreateInviteCodeRecordWithMeta(storage.DB, raw, storage.RenewCodeSourcePointExchange, 1)
			} else {
				err = storage.CreateRenewCodeRecordWithMeta(storage.DB, raw, 30, storage.RenewCodeSourcePointExchange, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			quote, err := InspectCodeCashout(raw, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InspectCodeCashout(raw, 2); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
				t.Fatalf("foreign quote: %v", err)
			}
			if _, err := ExecuteCodeCashout(2, quote); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
				t.Fatalf("foreign confirmation: %v", err)
			}
			if kind == "invite" {
				if _, err := account.ReserveInviteCodeForRegistrationWithAudit(2, security.HashSensitiveToken(raw)); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
					t.Fatalf("foreign invite redemption: %v", err)
				}
			} else {
				if _, err := account.RedeemRenewCodeByHash(2, security.HashSensitiveToken(raw)); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
					t.Fatalf("foreign renew redemption: %v", err)
				}
			}
			if _, err := ExecuteCodeCashout(1, quote); err != nil {
				t.Fatal(err)
			}
			bearer := "bearer-secret-123456"
			if kind == "invite" {
				err = storage.CreateInviteCodeRecord(storage.DB, bearer)
			} else {
				err = storage.CreateRenewCodeRecord(storage.DB, bearer, 30)
			}
			if err != nil {
				t.Fatal(err)
			}
			bearerQuote, err := InspectCodeCashout(bearer, 2)
			if err != nil {
				t.Fatal("untraded bearer code restricted", err)
			}
			if _, err := ExecuteCodeCashout(2, bearerQuote); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarketplaceTransfersOwnershipAndAllowsResale(t *testing.T) {
	for _, kind := range []string{"invite", "renew"} {
		t.Run(kind, func(t *testing.T) {
			setupCodeOwnershipTest(t)
			raw := "traded-secret-123456"
			price := 150
			var err error
			if kind == "invite" {
				price = 300
				err = storage.CreateInviteCodeRecord(storage.DB, raw)
			} else {
				err = storage.CreateRenewCodeRecord(storage.DB, raw, 30)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Even a valid quote issued before a sale must not let the former holder cash out afterwards.
			oldQuote, err := InspectCodeCashout(raw, 1)
			if err != nil {
				t.Fatal(err)
			}
			listing, err := createMarketplaceSecretListing(1, "user1", "系统卡密", price, []string{raw})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := purchaseMarketplaceListing(2, listing, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := ExecuteCodeCashout(1, oldQuote); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
				t.Fatalf("seller cashed out buyer's code: %v", err)
			}
			if _, err := createMarketplaceSecretListing(1, "user1", "系统卡密", price, []string{raw}); !errors.Is(err, storage.ErrMarketplaceSecretOwnerMismatch) {
				t.Fatalf("seller relisted buyer's code: %v", err)
			}
			resale, err := createMarketplaceSecretListing(2, "user2", "系统卡密", price, []string{raw})
			if err != nil {
				t.Fatal("buyer cannot resell", err)
			}
			if _, err := purchaseMarketplaceListing(3, resale, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectCodeCashout(raw, 2); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
				t.Fatalf("previous buyer retained rights: %v", err)
			}
			if _, err := InspectCodeCashout(raw, 3); err != nil {
				t.Fatal(err)
			}
			if kind == "invite" {
				if _, err := account.ReserveInviteCodeForRegistrationWithAudit(2, security.HashSensitiveToken(raw)); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
					t.Fatalf("old buyer redeemed invite: %v", err)
				}
				if _, err := account.ReserveInviteCodeForRegistrationWithAudit(3, security.HashSensitiveToken(raw)); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := account.RedeemRenewCodeByHash(2, security.HashSensitiveToken(raw)); !errors.Is(err, storage.ErrCodeOwnerMismatch) {
					t.Fatalf("old buyer redeemed renew: %v", err)
				}
				if _, err := account.RedeemRenewCodeByHash(3, security.HashSensitiveToken(raw)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

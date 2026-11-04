package account

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
)

func auditAccountSetup(t *testing.T) model.User {
	setupReferralAutoClaimTestDB(t)
	previous := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = previous })
	if err := storage.DB.AutoMigrate(&model.AuditLog{}, &model.Inventory{}, &model.Cultivation{}); err != nil {
		t.Fatal(err)
	}
	for _, asset := range rebindAssetModels {
		if err := storage.DB.AutoMigrate(asset.value); err != nil {
			t.Fatal(err)
		}
	}
	expiry := time.Now().Add(72 * time.Hour)
	u := model.User{TelegramID: 101, Username: "audit_trial", AbsUserID: "audit_abs", AccountType: storage.AccountTypeTrial, ExpireAt: &expiry, Points: 100}
	if err := storage.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAuditRebindMustMigrateInventory(t *testing.T) {
	u := auditAccountSetup(t)
	if err := storage.DB.Create(&model.Inventory{UserID: 101, ItemName: "audit_item", Quantity: 9}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RebindLocalUserWithAudit(202, u.ID, u.AbsUserID); err != nil {
		t.Fatal(err)
	}
	var count int64
	storage.DB.Model(&model.Inventory{}).Where("user_id = ?", 202).Count(&count)
	if count != 1 {
		t.Fatalf("rebind reports success but new owner inventory rows=%d; original asset remains at Telegram ID 101", count)
	}
}

func TestCrossTelegramRebindDoesNotTransferAdministrativeIdentity(t *testing.T) {
	for _, role := range []string{"admin", "super_admin", "configured_admin", "configured_recipient"} {
		t.Run(role, func(t *testing.T) {
			u := auditAccountSetup(t)
			switch role {
			case "configured_admin":
				config.AppConfig.AdminIDs = map[int64]bool{101: true}
			case "configured_recipient":
				config.AppConfig.AdminIDs = map[int64]bool{202: true}
				if err := storage.DB.Create(&model.User{TelegramID: 202, Username: "admin_wallet", Points: 25}).Error; err != nil {
					t.Fatal(err)
				}
			default:
				if err := storage.DB.Model(&u).Update("role", role).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := RebindLocalUserWithAudit(202, u.ID, u.AbsUserID); err == nil {
				t.Fatal("cross-user rebind moved an administrative identity")
			}
			if err := storage.DB.First(&u, u.ID).Error; err != nil || u.TelegramID != 101 || u.Points != 100 {
				t.Fatalf("rejected rebind changed original profile: %+v %v", u, err)
			}
		})
	}
}

func TestAuditRebindMustPreserveTrialType(t *testing.T) {
	u := auditAccountSetup(t)
	if err := RebindLocalUserWithAudit(101, u.ID, u.AbsUserID); err != nil {
		t.Fatal(err)
	}
	var saved model.User
	if err := storage.DB.First(&saved, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.AccountType != storage.AccountTypeTrial {
		t.Fatalf("trial became %q without redeeming an invite code", saved.AccountType)
	}
}

func TestAuditUnbindWalletMustNotBlockRebind(t *testing.T) {
	u := auditAccountSetup(t)
	if err := UnbindLocalUserWithAudit(101, u.AbsUserID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.EnsureUserWallet(&tgbotapi.User{ID: 101, FirstName: "Audit"}); err != nil {
		t.Fatal(err)
	}
	if err := RebindLocalUserWithAudit(101, u.ID, u.AbsUserID); err != nil {
		t.Fatalf("recreated wallet blocks restoration: %v", err)
	}
}

func TestRebindMigratesRelatedAssetsAndEncryptedReward(t *testing.T) {
	u := auditAccountSetup(t)
	config.AppConfig.SecurityPepper = "rebind-regression-pepper"
	for _, asset := range rebindAssetModels {
		if err := storage.DB.AutoMigrate(asset.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX test_inventory_unique ON inventories(user_id,item_name) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	ghost := model.User{TelegramID: 202, Username: "ghost_tg_202", Points: 25}
	winner := model.LotteryWinner{ActivityID: 12, UserID: 101}
	plot := model.GardenPlot{UserID: 101, PlotNo: 1}
	exchangeEncrypted, err := security.EncryptSecretReceipt("exchange-code", "EXC-pending", "test-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&ghost, &winner, &plot, &model.Inventory{UserID: 101, ItemName: "item", Quantity: 9}, &model.Inventory{UserID: 202, ItemName: "item", Quantity: 3}, &model.SpiritFire{OwnerID: 101}, &model.Sect{OwnerID: 101}, &model.MarketplaceSecret{SellerID: 101, BuyerID: 303}, &model.RenewCode{CodeHash: "test", OwnerUserID: 101}, &model.ExchangeCodeReceipt{ID: "EXC-pending", UserID: 101, Kind: "invite", ReplyEncrypted: exchangeEncrypted}} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	plant := model.GardenPlanting{UserID: 101, PlotID: plot.ID}
	if err := storage.DB.Create(&plant).Error; err != nil {
		t.Fatal(err)
	}
	identity := func(id int64) string {
		return fmt.Sprintf("activity=%d:winner=%d:user=%d", winner.ActivityID, winner.ID, id)
	}
	encrypted, err := security.EncryptSecretReceipt("lottery-reward-result", identity(101), "secret-card")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&winner).Update("reward_result_encrypted", encrypted).Error; err != nil {
		t.Fatal(err)
	}
	if err := RebindLocalUserWithAudit(202, u.ID, u.AbsUserID); err != nil {
		t.Fatal(err)
	}
	var rebound model.User
	if err := storage.DB.First(&rebound, u.ID).Error; err != nil || rebound.TelegramID != 202 || rebound.Points != 125 || rebound.AccountType != storage.AccountTypeTrial {
		t.Fatalf("bad merged account: %+v %v", rebound, err)
	}
	var inv model.Inventory
	if err := storage.DB.First(&inv, "user_id = ?", 202).Error; err != nil || inv.Quantity != 12 {
		t.Fatalf("inventory lost: %+v %v", inv, err)
	}
	for _, check := range []struct {
		value any
		field string
	}{{&model.SpiritFire{}, "owner_id"}, {&model.Sect{}, "owner_id"}, {&model.MarketplaceSecret{}, "seller_id"}, {&model.RenewCode{}, "owner_user_id"}, {&model.GardenPlot{}, "user_id"}, {&model.GardenPlanting{}, "user_id"}, {&model.ExchangeCodeReceipt{}, "user_id"}} {
		var count int64
		if err := storage.DB.Model(check.value).Where(check.field+" = ?", 202).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("asset missing (%T): %d %v", check.value, count, err)
		}
	}
	if err := storage.DB.First(&winner, winner.ID).Error; err != nil {
		t.Fatal(err)
	}
	plain, err := security.DecryptSecretReceipt("lottery-reward-result", identity(202), winner.RewardResultEncrypted)
	if err != nil || plain != "secret-card" {
		t.Fatalf("migrated reward cannot be claimed: %q %v", plain, err)
	}
	if _, err := security.DecryptSecretReceipt("lottery-reward-result", identity(101), winner.RewardResultEncrypted); err == nil {
		t.Fatal("old reward identity still authenticates")
	}
	var exchangeReceipt model.ExchangeCodeReceipt
	if err := storage.DB.First(&exchangeReceipt, "id = ? AND user_id = ?", "EXC-pending", 202).Error; err != nil {
		t.Fatal(err)
	}
	if plain, err := security.DecryptSecretReceipt("exchange-code", exchangeReceipt.ID, exchangeReceipt.ReplyEncrypted); err != nil || plain != "test-code" {
		t.Fatalf("pending exchange lost during rebind: %q %v", plain, err)
	}
}

func TestRebindConflictingProfilesRollBackEntireMigration(t *testing.T) {
	u := auditAccountSetup(t)
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX test_cultivation_unique ON cultivations(user_id)`).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&model.User{TelegramID: 202, Username: "ghost_tg_202", Points: 25}, &model.Cultivation{UserID: 101, TotalAudioTime: 10}, &model.Cultivation{UserID: 202, TotalAudioTime: 20}, &model.Inventory{UserID: 101, ItemName: "item", Quantity: 9}} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := RebindLocalUserWithAudit(202, u.ID, u.AbsUserID); !errors.Is(err, ErrRebindAssetConflict) {
		t.Fatalf("expected explicit conflict: %v", err)
	}
	var old, ghost model.User
	if err := storage.DB.First(&old, u.ID).Error; err != nil || old.TelegramID != 101 || old.Points != 100 {
		t.Fatalf("source changed: %+v %v", old, err)
	}
	if err := storage.DB.First(&ghost, "telegram_id = ?", 202).Error; err != nil || ghost.Points != 25 {
		t.Fatalf("recipient wallet lost: %+v %v", ghost, err)
	}
	var inv model.Inventory
	if err := storage.DB.First(&inv, "user_id = ?", 101).Error; err != nil || inv.Quantity != 9 {
		t.Fatal("failed migration lost original inventory")
	}
}

func TestInitialBindCannotConvertUnboundTrial(t *testing.T) {
	u := auditAccountSetup(t)
	if err := UnbindLocalUserWithAudit(101, u.AbsUserID); err != nil {
		t.Fatal(err)
	}
	if err := BindLocalUserWithAudit(101, "another_abs", "another_id", "hash", nil); err == nil {
		t.Fatal("first-bind overwrote existing trial")
	}
}

func TestRebindMissingAssetTableFailsWithoutChangingOwner(t *testing.T) {
	u := auditAccountSetup(t)
	if err := storage.DB.Migrator().DropTable(&model.SpiritFireWallet{}); err != nil {
		t.Fatal(err)
	}
	if err := RebindLocalUserWithAudit(202, u.ID, u.AbsUserID); err == nil {
		t.Fatal("reported full migration with missing asset table")
	}
	var unchanged model.User
	if err := storage.DB.First(&unchanged, u.ID).Error; err != nil || unchanged.TelegramID != 101 {
		t.Fatalf("partial migration committed: %+v %v", unchanged, err)
	}
}

func TestRenameRejectsChangedAccountSnapshot(t *testing.T) {
	u := auditAccountSetup(t)
	for _, expected := range []struct{ username, absID string }{
		{"stale_name", u.AbsUserID}, {u.Username, "different-account"},
	} {
		if err := RenameLocalUsernameWithAudit(u.TelegramID, expected.username, "renamed", expected.absID); err == nil {
			t.Fatal("rename accepted a stale account snapshot")
		}
	}
	var saved model.User
	if err := storage.DB.First(&saved, u.ID).Error; err != nil || saved.Username != u.Username {
		t.Fatalf("rejected rename changed account: %+v %v", saved, err)
	}
	if err := RenameLocalUsernameWithAudit(u.TelegramID, u.Username, "renamed", u.AbsUserID); err != nil {
		t.Fatalf("matching account snapshot failed: %v", err)
	}
}

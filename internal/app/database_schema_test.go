package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/backup"
	configpkg "ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/market"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func TestDatabaseFreshStartAndRestart(t *testing.T) {
	if os.Getenv("SCHEMA_TEST_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDatabaseFreshStartAndRestart$")
		cmd.Env = append(os.Environ(), "SCHEMA_TEST_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("database bootstrap failed: %v\n%s", err, output)
		}
		return
	}
	configpkg.AppConfig = &configpkg.Config{DbURL: filepath.Join(t.TempDir(), "fresh.db"), DatabaseMaxOpenConns: 1, DatabaseMaxIdleConns: 1, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64)}
	InitDB()
	assertCurrentDatabaseIndexes(t)
	if err := storage.DB.Create(&model.User{TelegramID: 1, Username: "player", Points: 50}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.BreakthroughConfig{}).Where("from_major_realm = ?", 4).Update("guarantee_fail_count", 7).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SystemConfig{Key: "test", Value: "preserved"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SystemConfig{Key: "test", Value: "duplicate"}).Error; err == nil {
		t.Error("configuration key uniqueness is missing")
	}
	if err := storage.DB.Create(&model.Inventory{UserID: 1, ItemName: "回气丹", Quantity: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Inventory{UserID: 1, ItemName: "回气丹", Quantity: 3}).Error; err == nil {
		t.Error("inventory uniqueness is missing")
	}
	if err := storage.DB.Where("user_id = ? AND item_name = ?", 1, "回气丹").Delete(&model.Inventory{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Inventory{UserID: 1, ItemName: "回气丹", Quantity: 4}).Error; err != nil {
		t.Fatalf("soft deletion blocks replacement inventory: %v", err)
	}
	connection, err := storage.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	InitDB()
	assertCurrentDatabaseIndexes(t)
	var user model.User
	var config model.SystemConfig
	var breakthrough model.BreakthroughConfig
	if err := storage.DB.First(&user, "telegram_id = ?", 1).Error; err != nil || user.Points != 50 {
		t.Fatalf("restart changed player balance: %+v, %v", user, err)
	}
	if err := storage.DB.First(&config, "key = ?", "test").Error; err != nil || config.Value != "preserved" {
		t.Fatalf("restart changed configuration: %+v, %v", config, err)
	}
	if err := storage.DB.First(&breakthrough, "from_major_realm = ?", 4).Error; err != nil || breakthrough.GuaranteeFailCount != 7 {
		t.Fatalf("restart overwrote custom breakthrough rules: %+v, %v", breakthrough, err)
	}
	var quantity int
	if err := storage.DB.Model(&model.Inventory{}).Select("quantity").Where("user_id = ? AND item_name = ?", 1, "回气丹").Scan(&quantity).Error; err != nil || quantity != 4 {
		t.Fatalf("restart changed inventory: %d, %v", quantity, err)
	}
}

func assertCurrentDatabaseIndexes(t *testing.T) {
	t.Helper()
	namePattern := regexp.MustCompile(`INDEX IF NOT EXISTS (\w+)`)
	normalize := func(sql string) string {
		sql = strings.ToLower(sql)
		sql = strings.ReplaceAll(sql, "if not exists ", "")
		sql = strings.NewReplacer("`", "", "\"", "", ";", "").Replace(sql)
		return strings.Join(strings.Fields(sql), " ")
	}
	for _, expected := range storage.DatabaseIndexes {
		name := namePattern.FindStringSubmatch(expected)[1]
		var actual string
		if err := storage.DB.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&actual).Error; err != nil {
			t.Fatal(err)
		}
		if normalize(actual) != normalize(expected) {
			t.Errorf("index %s has wrong definition: %s; expected %s", name, actual, expected)
		}
	}
}

func TestSensitiveTokenRequiresHash(t *testing.T) {
	previous := configpkg.AppConfig
	configpkg.AppConfig = &configpkg.Config{SecurityPepper: strings.Repeat("s", 64)}
	t.Cleanup(func() { configpkg.AppConfig = previous })
	hashed := security.HashSensitiveToken("secret")
	if !security.VerifySensitiveToken("secret", hashed) || security.VerifySensitiveToken("wrong", hashed) || security.VerifySensitiveToken("secret", "secret") {
		t.Fatal("security token validation accepts an invalid representation")
	}
}

func TestSecretRealmConfiguredValuesSurviveNormalization(t *testing.T) {
	cfg := game.DefaultSectSecretRealmConfig()
	profile := &cfg.Profiles[0]
	profile.MaxRewardPoints = 8
	profile.BaseCost = 80
	profile.CostPerLevel = 20
	profile.MinDeltaHours = 0.2
	game.NormalizeSectSecretRealmProfile(profile)
	if profile.MaxRewardPoints != 8 || profile.BaseCost != 80 || profile.CostPerLevel != 20 || profile.MinDeltaHours != 0.2 {
		t.Fatal("normalization silently overwrites configured reward values")
	}
}

func TestBackupRoundTrip(t *testing.T) {
	payload := []byte("SQLite test payload")
	compressed, err := backup.GzipCompressBytes(payload)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := backup.EncryptBackupData(compressed, "test backup key")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := backup.DecryptBackupData(encrypted, "test backup key")
	if err != nil || string(decoded) != string(payload) {
		t.Fatalf("backup round trip failed: %q, %v", decoded, err)
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := backup.DecryptBackupData(encrypted, "test backup key"); err == nil {
		t.Fatal("backup accepts corrupted ciphertext")
	}
}

func TestSecretRealmRawDeltaStartsAtZero(t *testing.T) {
	now := time.Now()
	detail := game.CalculateSectSecretRealmRawDeltaDetail(0, 1200, now.Add(-20*time.Minute), now.Add(-time.Hour), now.Add(time.Hour), now)
	if detail.DeltaSeconds != 1200 {
		t.Fatalf("zero raw baseline lost new listening time: %+v", detail)
	}
}

func TestSecretRealmMultiplierCommand(t *testing.T) {
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if _, err := game.ExecuteSectSecretRealmAdminWriteCommand(1, "设置秘境倍率 普通 2 130 145", "测试配置变更"); err != nil {
		t.Fatal(err)
	}
	cfg, err := game.LoadSectSecretRealmConfigChecked()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := cfg.Profile(game.SectSecretRealmProfileNormal)
	if !ok {
		t.Fatal("missing normal profile")
	}
	multiplier := game.SectSecretRealmRewardMultiplierForProfile(2, profile)
	if multiplier.ContributionPercent != 130 || multiplier.PrestigePercent != 145 {
		t.Fatalf("wrong multiplier persisted: %+v", multiplier)
	}
	if _, err := game.ExecuteSectSecretRealmAdminWriteCommand(1, "设置秘境倍率 普通 2 100 130 145", "测试配置变更"); err == nil {
		t.Fatal("unexpected parameter accepted")
	}
}

func TestMarketplaceUsesRecordedOrderAmounts(t *testing.T) {
	// Quantity is the count of inventory units per delivered bundle, not the number of paid bundles.
	purchase := model.MarketplacePurchase{Price: 99, Quantity: 10, GrossAmount: 3, FeeAmount: 3, SellerAmount: 0}
	if got := market.MarketplacePurchaseGrossAmount(purchase); got != 3 {
		t.Fatalf("order total recalculated from item count: %d", got)
	}
	if got := market.MarketplacePurchaseSellerAmount(purchase, 3); got != 0 {
		t.Fatalf("zero recorded seller proceeds changed: %d", got)
	}
	purchase.GrossAmount = 0
	if got := market.MarketplacePurchaseGrossAmount(purchase); got != 0 {
		t.Fatalf("missing recorded amount guessed from item count: %d", got)
	}
	if _, err := market.MarketplaceListingTypeForPurchase(""); err == nil {
		t.Fatal("missing listing type accepted")
	}
	if !market.MarketplaceListingExpired(model.MarketplaceListing{}, time.Now()) {
		t.Fatal("missing expiry permits trading")
	}
}

func TestSplashVictimLabelsUseCurrentProfile(t *testing.T) {
	labels := game.ResolveSplashVictimLabels([]string{"name_with_markdown", "ghost_tg_2", ""}, []string{"", "显示名", ""}, []int64{1, 2, 3})
	if labels[0] != textfmt.EscapeMarkdown("name_with_markdown") || labels[1] != "显示名" || labels[2] != "3" {
		t.Fatalf("invalid announcement labels: %v", labels)
	}
}

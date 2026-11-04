package account

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupReferralAutoClaimTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "referral.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	previousDB, previousClient := storage.DB, abs.DefaultClient
	storage.DB, abs.DefaultClient = db, nil
	t.Cleanup(func() {
		storage.DB, abs.DefaultClient = previousDB, previousClient
		_ = conn.Close()
	})
	if err := db.AutoMigrate(&model.User{}, &model.ReferralActivation{}, &model.DailyListeningStat{}, &model.SystemConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX idx_referral_test_config ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
}

func TestReferralAutoClaimBatchAdvancesPastIncompleteFirstPage(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	now := time.Now()
	for id := int64(1); id <= int64(storage.ReferralAutoClaimScanLimit+1); id++ {
		activation := model.ReferralActivation{
			InviteeID: id, InviterID: 999, CodeID: 1, Status: storage.ReferralStatusActive,
			TrialStartedAt: now.Add(-time.Hour), TrialEndsAt: now.Add(48 * time.Hour),
		}
		if err := storage.DB.Create(&activation).Error; err != nil {
			t.Fatal(err)
		}
	}
	first, err := loadReferralAutoClaimBatch(0)
	if err != nil || len(first) != storage.ReferralAutoClaimScanLimit {
		t.Fatalf("first page: count=%d err=%v", len(first), err)
	}
	second, err := loadReferralAutoClaimBatch(first[len(first)-1].ID)
	if err != nil || len(second) != 1 || second[0].InviteeID != int64(storage.ReferralAutoClaimScanLimit+1) {
		t.Fatalf("second page: rows=%v err=%v", second, err)
	}
	RunReferralAutoClaimIfNeeded(&tgbotapi.BotAPI{}, now)
	cursor, err := storage.GetSystemConfigStringChecked(storage.ReferralAutoClaimCursorKey)
	if err != nil || cursor != "50" {
		t.Fatalf("first run cursor=%q err=%v", cursor, err)
	}
	RunReferralAutoClaimIfNeeded(&tgbotapi.BotAPI{}, now.Add(2*storage.ReferralAutoClaimInterval))
	cursor, err = storage.GetSystemConfigStringChecked(storage.ReferralAutoClaimCursorKey)
	if err != nil || cursor != "51" {
		t.Fatalf("second run cursor=%q err=%v", cursor, err)
	}
}

func TestReferralAutoClaimDoesNotExpireWhenListeningStatsUnavailable(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	now := time.Now()
	if err := storage.DB.Create(&model.User{TelegramID: 1, Username: "trial", AbsUserID: "abs1", AccountType: storage.AccountTypeTrial}).Error; err != nil {
		t.Fatal(err)
	}
	activation := model.ReferralActivation{
		InviteeID: 1, InviterID: 999, CodeID: 1, Status: storage.ReferralStatusActive,
		TrialStartedAt: now.Add(-4 * 24 * time.Hour), TrialEndsAt: now.Add(-25 * time.Hour),
	}
	if err := storage.DB.Create(&activation).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := settleReferralTrialTaskIfDue(activation, now, true); !errors.Is(err, storage.ErrDailyListeningStatsReadFailed) {
		t.Fatalf("missing ABS stats returned %v", err)
	}
	RunReferralAutoClaimIfNeeded(&tgbotapi.BotAPI{}, now)
	var saved model.ReferralActivation
	if err := storage.DB.First(&saved, activation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != storage.ReferralStatusActive {
		t.Fatalf("unverified trial was expired: %s", saved.Status)
	}
}

func TestReferralAutoClaimExpiresOnlyAfterVerifiedIncompleteResult(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	now := time.Now()
	if err := storage.DB.Create(&model.User{TelegramID: 1, Username: "trial", AbsUserID: "abs1", AccountType: storage.AccountTypeTrial}).Error; err != nil {
		t.Fatal(err)
	}
	activation := model.ReferralActivation{
		InviteeID: 1, InviterID: 999, CodeID: 1, Status: storage.ReferralStatusActive,
		TrialStartedAt: now.Add(-4 * 24 * time.Hour), TrialEndsAt: now.Add(-25 * time.Hour),
	}
	if err := storage.DB.Create(&activation).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.DailyListeningStat{
		UserID: 1, AbsUserID: "abs1", DayKey: game.SectDayKey(now), LastFetchedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	abs.DefaultClient = &abs.AbsClient{BaseURL: "http://localhost", Token: "test"}
	RunReferralAutoClaimIfNeeded(&tgbotapi.BotAPI{}, now)
	var saved model.ReferralActivation
	if err := storage.DB.First(&saved, activation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != storage.ReferralStatusExpired {
		t.Fatalf("verified incomplete trial status=%s", saved.Status)
	}
}

func TestReferralTaskDoesNotExtendReplacedABSAccount(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	now := time.Now()
	end := now.Add(24 * time.Hour)
	u := model.User{TelegramID: 1, Username: "trial", AbsUserID: "original", AccountType: storage.AccountTypeTrial, ExpireAt: &end}
	activation := model.ReferralActivation{
		InviteeID: 1, InviterID: 999, CodeID: 1, Status: storage.ReferralStatusActive,
		TrialStartedAt: now.Add(-48 * time.Hour), TrialEndsAt: end,
	}
	for _, row := range []any{&u, &activation, &model.DailyListeningStat{UserID: 1, AbsUserID: "original", DayKey: game.SectDayKey(now.Add(-24 * time.Hour)), CappedSeconds: 5 * 3600}} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "replacement").Error; err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	if _, granted, err := settleReferralTrialTaskIfDue(activation, now, true); granted || !errors.Is(err, storage.ErrReferralNoActivation) {
		t.Fatalf("old account listening was settled for replacement: granted=%t err=%v", granted, err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || u.ExpireAt == nil || !u.ExpireAt.Equal(end) {
		t.Fatalf("replacement account expiry changed: %+v %v", u, err)
	}
}

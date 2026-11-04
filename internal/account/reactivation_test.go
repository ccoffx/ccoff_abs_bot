package account

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestRenewalPreservesCardForPermanentOrUnregisteredAccount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		absID       string
		whitelisted bool
		wantError   error
	}{
		{"permanent", "audit_abs", false, storage.ErrPermanentAccountNoRenew},
		{"whitelisted", "audit_abs", true, storage.ErrPermanentAccountNoRenew},
		{"unregistered", "", false, storage.ErrUserNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := suspendedAccountFixture(t, false)
			var expiry *time.Time
			if tc.whitelisted {
				expiry = u.ExpireAt
			}
			if err := storage.DB.Model(&u).Updates(map[string]any{"expire_at": expiry, "abs_user_id": tc.absID, "is_whitelist": tc.whitelisted}).Error; err != nil {
				t.Fatal(err)
			}
			code := model.RenewCode{CodeHash: "retained-renew", Days: 30}
			if err := storage.DB.Create(&code).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := RedeemRenewCodeByHash(u.TelegramID, code.CodeHash); !errors.Is(err, tc.wantError) {
				t.Fatalf("unexpected renewal result: %v", err)
			}
			if err := storage.DB.First(&u, u.ID).Error; err != nil || (expiry == nil && u.ExpireAt != nil) || (expiry != nil && (u.ExpireAt == nil || !u.ExpireAt.Equal(*expiry))) {
				t.Fatalf("renewal changed permanent expiry: %+v %v", u, err)
			}
			if err := storage.DB.First(&code, code.ID).Error; err != nil || code.IsUsed || code.UsedByID != 0 {
				t.Fatalf("rejected renewal consumed card: %+v %v", code, err)
			}
		})
	}
}

func suspendedAccountFixture(t *testing.T, admin bool) model.User {
	t.Helper()
	u := auditAccountSetup(t)
	if err := storage.DB.AutoMigrate(&model.AccountLifecycleClaim{}, &model.ListeningAbuseRecord{}, &model.RenewCode{}, &model.PointTransaction{}, &model.ReferralMonthlyRewardQuota{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&u).Updates(map[string]interface{}{"is_suspended": true, "admin_suspended": admin, "account_type": storage.AccountTypeFormal}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.InitializeAccountLifecycleGuards(storage.DB); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRenewalPreservesAdministrativeBan(t *testing.T) {
	u := suspendedAccountFixture(t, true)
	code := model.RenewCode{CodeHash: "test-renew", Days: 1}
	if err := storage.DB.Create(&code).Error; err != nil {
		t.Fatal(err)
	}
	result, err := RedeemRenewCodeByHash(u.TelegramID, code.CodeHash)
	if err != nil || result.NeedReactivate {
		t.Fatalf("renewal can remove admin ban: %+v %v", result, err)
	}
	if err := ReactivateExpiredAccount(u.TelegramID); err == nil {
		t.Fatal("explicit auto-reactivation accepted admin ban")
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || !u.IsSuspended || !u.AdminSuspended {
		t.Fatalf("admin ban changed: %+v %v", u, err)
	}
}

func TestRenewalRejectsInvalidDaysWithoutConsumingCard(t *testing.T) {
	for _, days := range []int{-30, 0, 366} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			u := suspendedAccountFixture(t, false)
			expiry := *u.ExpireAt
			code := model.RenewCode{CodeHash: "invalid-days", Days: days}
			if err := storage.DB.Create(&code).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := RedeemRenewCodeByHash(u.TelegramID, code.CodeHash); !errors.Is(err, storage.ErrInvalidRenewCode) {
				t.Fatalf("invalid duration accepted: %v", err)
			}
			if err := storage.DB.First(&u, u.ID).Error; err != nil || !u.ExpireAt.Equal(expiry) {
				t.Fatalf("invalid card changed account expiry: %+v %v", u, err)
			}
			if err := storage.DB.First(&code, code.ID).Error; err != nil || code.IsUsed {
				t.Fatalf("invalid card consumed: %+v %v", code, err)
			}
		})
	}
}

func TestExpiryReactivationRetriesABSFailureAndIsIdempotent(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	var failed atomic.Bool
	var patches atomic.Int64
	failed.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			w.WriteHeader(503)
			return
		}
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		fmt.Fprintf(w, `{"id":%q,"isActive":false}`, u.AbsUserID)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	if err := ReactivateExpiredAccount(u.TelegramID); err == nil {
		t.Fatal("503 did not fail restoration")
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || !u.IsSuspended {
		t.Fatal("failure lost persistent retry marker")
	}
	failed.Store(false)
	if err := RetryExpiredAccountReactivations(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || u.IsSuspended {
		t.Fatalf("retry did not restore user: %+v %v", u, err)
	}
	if err := RetryExpiredAccountReactivations(); err != nil || patches.Load() != 1 {
		t.Fatalf("restoration was repeated: patches=%d err=%v", patches.Load(), err)
	}
}

func TestTrialTaskBackfillRestoresExpiredABSAccount(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	now := time.Now()
	end := now.Add(-time.Hour)
	if err := storage.DB.Model(&u).Updates(map[string]interface{}{"account_type": storage.AccountTypeTrial, "expire_at": end}).Error; err != nil {
		t.Fatal(err)
	}
	activation := model.ReferralActivation{InviteeID: u.TelegramID, InviterID: 999, CodeID: 1, Status: storage.ReferralStatusActive, TrialStartedAt: end.Add(-72 * time.Hour), TrialEndsAt: end, TrialEndDaySeconds: 5 * 3600}
	if err := storage.DB.Create(&activation).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec("CREATE UNIQUE INDEX test_referral_monthly ON referral_monthly_reward_quotas(inviter_id, month_key)").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.ReferralMonthlyRewardQuota{InviterID: 999, MonthKey: referralMonthKey(now), RewardPoints: storage.ReferralMonthlyRewardMax}).Error; err != nil {
		t.Fatal(err)
	}
	var patches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "listening-stats") {
			fmt.Fprint(w, `{"days":{}}`)
			return
		}
		if strings.Contains(r.URL.Path, "listening-sessions") {
			fmt.Fprint(w, `{"sessions":[]}`)
			return
		}
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		fmt.Fprintf(w, `{"id":%q,"isActive":false}`, u.AbsUserID)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	_, settled, err := settleReferralTrialTaskIfDue(activation, now, true)
	if err != nil || !settled {
		t.Fatalf("backfill failed: settled=%t err=%v", settled, err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || u.IsSuspended || !u.ExpireAt.After(now) || patches.Load() != 1 {
		t.Fatalf("backfill failed to restore ABS: %+v patches=%d err=%v", u, patches.Load(), err)
	}
}

func TestConcurrentReactivationRetriesSendOneABSActivation(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	var patches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		fmt.Fprintf(w, `{"id":%q}`, u.AbsUserID)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := ReactivateExpiredAccount(u.TelegramID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if patches.Load() != 1 {
		t.Fatalf("concurrent retries activated %d times", patches.Load())
	}
}

func TestExpiredTrialConversionRestoresABS(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	config.AppConfig.AccountValidDays = 30
	if err := storage.DB.Model(&u).Updates(map[string]interface{}{"account_type": storage.AccountTypeTrial, "expire_at": time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.InviteCode{CodeHash: "formal-invite"}).Error; err != nil {
		t.Fatal(err)
	}
	patches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
		}
		fmt.Fprintf(w, `{"id":%q,"isActive":false}`, u.AbsUserID)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	if _, err := ConvertTrialToFormalWithInviteCode(u.TelegramID, "formal-invite"); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || u.IsSuspended || u.AccountType != storage.AccountTypeFormal || patches != 1 {
		t.Fatalf("conversion left account disabled: %+v patches=%d err=%v", u, patches, err)
	}
}

func TestReactivationAuditFailureKeepsClaimForRetry(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q}`, u.AbsUserID)
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	if err := storage.DB.Exec(`CREATE TRIGGER reject_reactivation_audit BEFORE INSERT ON audit_logs
		WHEN NEW.action = 'REACTIVATE_EXPIRED_USER' BEGIN SELECT RAISE(ABORT, 'test audit failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := ReactivateExpiredAccount(u.TelegramID); err == nil {
		t.Fatal("failed audit committed reactivation")
	}
	var claims int64
	if err := storage.DB.Model(&model.AccountLifecycleClaim{}).Where("user_row_id = ?", u.ID).Count(&claims).Error; err != nil || claims != 1 {
		t.Fatalf("finalization lost retry claim: count=%d err=%v", claims, err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || !u.IsSuspended {
		t.Fatalf("failed finalization cleared retry marker: %+v %v", u, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER reject_reactivation_audit").Error; err != nil {
		t.Fatal(err)
	}
	if err := RetryExpiredAccountReactivations(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || u.IsSuspended {
		t.Fatalf("retry did not finish local transaction: %+v %v", u, err)
	}
}

func TestReactivationPreservesActiveListeningAbuseFreeze(t *testing.T) {
	u := suspendedAccountFixture(t, false)
	if err := storage.DB.Create(&model.ListeningAbuseRecord{UserID: u.TelegramID, AbsUserID: u.AbsUserID, Action: storage.ListeningAbuseActionFreeze, Status: storage.ListeningAbuseStatusActive}).Error; err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("frozen account reached ABS activation")
	}))
	defer srv.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	if err := ReactivateExpiredAccount(u.TelegramID); err == nil {
		t.Fatal("active abuse freeze allowed expiry reactivation")
	}
	if err := storage.DB.First(&u, u.ID).Error; err != nil || !u.IsSuspended {
		t.Fatalf("active freeze lost suspension: %+v %v", u, err)
	}
}

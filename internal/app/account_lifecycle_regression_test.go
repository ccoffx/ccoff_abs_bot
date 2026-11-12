package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func lifecycleFixture(t *testing.T, expireAt time.Time, suspended bool, handler http.HandlerFunc) *tgbotapi.BotAPI {
	t.Helper()
	bot := channelOpsFixture(t)
	config.AppConfig.AccountGraceDays = 7
	config.AppConfig.NotifyBeforeDays = 3
	config.AppConfig.SecurityPepper = "lifecycle-regression-pepper"
	if err := storage.DB.AutoMigrate(&model.AccountLifecycleClaim{}, &model.AccountLifecycleNotice{}, &model.OrphanAccountDeletion{}, &model.RenewCode{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.DailyListeningStat{}, &model.ABSCrossDaySessionScanCursor{}, &model.ABSCrossDaySessionAdjustment{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}, &model.WorldBossParticipant{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.InitializeAccountLifecycleGuards(storage.DB); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(&model.Sect{}, &model.SectMember{}, &model.SectCaveRetreat{}, &model.GardenPlot{}, &model.GardenRecipeUnlock{}, &model.Inventory{}, &model.GardenSeedPurchase{}, &model.GardenHerbMarketSale{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.InitializeOrphanAccountDeletionGuards(storage.DB); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(append(spiritfire.Models(), &model.SectOwnerBonusRevision{}, &model.SectOwnerBonusState{})...); err != nil {
		t.Fatal(err)
	}
	if err := spiritfire.Initialize(storage.DB, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Updates(map[string]interface{}{"abs_user_id": "lifecycle_abs", "expire_at": expireAt, "is_suspended": suspended}).Error; err != nil {
		t.Fatal(err)
	}
	old := abs.DefaultClient
	server := httptest.NewServer(handler)
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	t.Cleanup(func() { abs.DefaultClient = old; server.Close() })
	return bot
}

func TestLifecycleRecentlyExpiredAccountSuspends(t *testing.T) {
	bot := lifecycleFixture(t, time.Now().Add(-time.Hour), false, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":"lifecycle_abs","isActive":true}`) })
	if err := runDailyLifecycleOperations(bot); err != nil {
		t.Fatal(err)
	}
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if !u.IsSuspended {
		t.Fatal("one-hour expired account remains active")
	}
}

func TestLifecycleFailureRetriesWithoutClaimingDailySuccess(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	bot := lifecycleFixture(t, time.Now().Add(-time.Hour), false, func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
		}
		fmt.Fprint(w, `{"id":"lifecycle_abs","isActive":true}`)
	})
	runDailyLifecycleIfNeeded(bot, time.Now())
	date, err := storage.GetSystemConfigStringChecked(storage.DailyLifecycleLastSuccessDateKey)
	if err != nil || date != "" {
		t.Fatalf("failed ABS call marked daily success: %q %v", date, err)
	}
	var count int64
	storage.DB.Model(&model.AccountLifecycleClaim{}).Count(&count)
	if count != 1 {
		t.Fatalf("pending operation was lost: %d", count)
	}
	unavailable.Store(false)
	runDailyLifecycleIfNeeded(bot, time.Now())
	date, err = storage.GetSystemConfigStringChecked(storage.DailyLifecycleLastSuccessDateKey)
	if err != nil || date != storage.DailyOperationsDateKey(time.Now()) {
		t.Fatalf("retry did not complete: %q %v", date, err)
	}
}

func TestLifecycleDeleteRejectsConcurrentRenewalAndPreservesCard(t *testing.T) {
	var called atomic.Bool
	bot := lifecycleFixture(t, time.Now().Add(-8*24*time.Hour), true, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected %s", r.Method)
		}
		called.Store(true)
		if _, err := account.RedeemRenewCodeByHash(2, security.HashSensitiveToken("renew-during-delete")); err == nil {
			t.Error("renewal committed after durable deletion claim")
		}
		var code model.RenewCode
		if err := storage.DB.First(&code, "code_hash = ?", security.HashSensitiveToken("renew-during-delete")).Error; err != nil || code.IsUsed {
			t.Errorf("rejected renewal consumed card: %+v %v", code, err)
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if err := storage.CreateRenewCodeRecord(storage.DB, "renew-during-delete", 30); err != nil {
		t.Fatal(err)
	}
	if err := runDailyLifecycleOperations(bot); err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatal("deletion not attempted")
	}
	var count int64
	storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Count(&count)
	if count != 0 {
		t.Fatal("successful idempotent delete did not finalize")
	}
}

func TestLifecycleRechecksRenewalBeforeTakingClaim(t *testing.T) {
	bot := lifecycleFixture(t, time.Now().Add(-8*24*time.Hour), true, func(w http.ResponseWriter, r *http.Request) {
		t.Error("renewed account reached ABS mutation")
		fmt.Fprint(w, `{"id":"lifecycle_abs","isActive":true}`)
	})
	var snapshot model.User
	storage.DB.First(&snapshot, "telegram_id = ?", 2)
	if err := storage.DB.Model(&snapshot).Updates(map[string]interface{}{"expire_at": time.Now().AddDate(0, 0, 30), "is_suspended": false}).Error; err != nil {
		t.Fatal(err)
	}
	_, kind, err := claimAccountLifecycle(snapshot.ID, time.Now())
	if err != nil || kind != "" {
		t.Fatalf("stale snapshot claimed renewed account: %s %v", kind, err)
	}
	if err := runDailyLifecycleOperations(bot); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleGraceUsesExactDeadlineAndProtectsSuperAdmin(t *testing.T) {
	var deletes atomic.Int64
	now := time.Now()
	bot := lifecycleFixture(t, now.AddDate(0, 0, -7).Add(time.Hour), true, func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		fmt.Fprint(w, `{"id":"lifecycle_abs","isActive":true}`)
	})
	if err := runAccountLifecycleAt(bot, now); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 0 {
		t.Fatal("deleted before grace deadline")
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Update("role", "super_admin").Error; err != nil {
		t.Fatal(err)
	}
	if err := runAccountLifecycleAt(bot, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 0 {
		t.Fatal("super admin reached external deletion")
	}
}

func TestLifecycleProtectsConfiguredAdminAndCancelsPendingExpiryOperation(t *testing.T) {
	for _, pending := range []string{"", "suspend", "delete"} {
		t.Run("pending_"+pending, func(t *testing.T) {
			bot := lifecycleFixture(t, time.Now().AddDate(0, 0, -8), true, func(w http.ResponseWriter, r *http.Request) {
				t.Error("configured super admin reached automatic ABS mutation")
			})
			var u model.User
			if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil {
				t.Fatal(err)
			}
			if pending != "" {
				if err := storage.DB.Create(&model.AccountLifecycleClaim{UserRowID: u.ID, Kind: pending, CreatedAt: time.Now()}).Error; err != nil {
					t.Fatal(err)
				}
			}
			config.AppConfig.AdminIDs[2] = true
			if err := runAccountLifecycleAt(bot, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.First(&u, u.ID).Error; err != nil || u.AbsUserID != "lifecycle_abs" {
				t.Fatalf("configured admin lost profile: %+v %v", u, err)
			}
			var count int64
			if err := storage.DB.Model(&model.AccountLifecycleClaim{}).Where("user_row_id = ?", u.ID).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("configured admin retained expiry claim: %d %v", count, err)
			}
		})
	}
}

func TestLifecycleLocalFailureRetainsClaimForIdempotentRetry(t *testing.T) {
	var deletes atomic.Int64
	bot := lifecycleFixture(t, time.Now().Add(-8*24*time.Hour), true, func(w http.ResponseWriter, r *http.Request) { deletes.Add(1); w.WriteHeader(http.StatusNotFound) })
	if err := storage.DB.Exec(`CREATE TRIGGER fail_lifecycle_audit BEFORE INSERT ON audit_logs WHEN NEW.action = 'AUTO_DELETE_EXPIRED_USER' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := runDailyLifecycleOperations(bot); err == nil {
		t.Fatal("local audit failure marked successful")
	}
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil {
		t.Fatalf("local failure deleted user: %v", err)
	}
	var claim model.AccountLifecycleClaim
	if err := storage.DB.First(&claim, "user_row_id = ?", u.ID).Error; err != nil {
		t.Fatal("local rollback lost deletion claim")
	}
	if err := storage.DB.Model(&u).Update("is_whitelist", true).Error; err == nil {
		t.Fatal("local failure removed mutation guard")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_lifecycle_audit").Error; err != nil {
		t.Fatal(err)
	}
	if err := runDailyLifecycleOperations(bot); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 2 {
		t.Fatalf("pending ABS operation not retried: %d", deletes.Load())
	}
}

func TestRequestedDeletionGuardsAccountAndResumesWithOriginalAudit(t *testing.T) {
	for _, action := range []string{"SELF_DELETE_USER", "FORCE_DELETE_USER"} {
		t.Run(action, func(t *testing.T) {
			var unavailable atomic.Bool
			unavailable.Store(true)
			var calls atomic.Int64
			bot := lifecycleFixture(t, time.Now().AddDate(0, 0, 30), false, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodDelete {
					t.Errorf("unexpected ABS method: %s", r.Method)
				}
				if _, err := account.RedeemRenewCodeByHash(2, security.HashSensitiveToken("renew-during-requested-delete")); err == nil {
					t.Error("renewal committed during requested deletion")
				}
				if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Update("telegram_id", 77).Error; err == nil {
					t.Error("rebind committed during requested deletion")
				}
				if unavailable.Load() {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(404)
				}
			})
			if err := storage.CreateRenewCodeRecord(storage.DB, "renew-during-requested-delete", 30); err != nil {
				t.Fatal(err)
			}
			actorID := int64(1)
			if action == "SELF_DELETE_USER" {
				actorID = 2
			}
			if err := deleteAccountWithAudit(actorID, 2, "lifecycle_abs", action, func(model.User) string { return "original confirmed deletion" }); err == nil {
				t.Fatal("503 marked requested deletion complete")
			}
			var u model.User
			if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil {
				t.Fatal(err)
			}
			var claim model.AccountLifecycleClaim
			if err := storage.DB.First(&claim, "user_row_id = ?", u.ID).Error; err != nil || claim.ActorID != actorID || claim.AuditAction != action {
				t.Fatalf("deletion lost durable intent: %+v %v", claim, err)
			}
			if err := storage.DB.Exec(`CREATE TRIGGER fail_requested_delete_audit BEFORE INSERT ON audit_logs
				WHEN NEW.action IN ('SELF_DELETE_USER','FORCE_DELETE_USER') BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`).Error; err != nil {
				t.Fatal(err)
			}
			unavailable.Store(false)
			if err := runAccountLifecycleAt(bot, time.Now()); err == nil {
				t.Fatal("failed audit finalized deletion")
			}
			if err := storage.DB.First(&claim, "user_row_id = ?", u.ID).Error; err != nil {
				t.Fatal("failed finalization lost deletion claim")
			}
			if err := storage.DB.Exec("DROP TRIGGER fail_requested_delete_audit").Error; err != nil {
				t.Fatal(err)
			}
			if err := runAccountLifecycleAt(bot, time.Now()); err != nil {
				t.Fatal(err)
			}
			var remaining int64
			if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Count(&remaining).Error; err != nil || remaining != 0 {
				t.Fatalf("retry retained deleted profile: %d %v", remaining, err)
			}
			var audit model.AuditLog
			if err := storage.DB.First(&audit, "action = ?", action).Error; err != nil || audit.ActorID != actorID || audit.Detail != "original confirmed deletion" {
				t.Fatalf("recovery lost original audit: %+v %v", audit, err)
			}
			var code model.RenewCode
			if err := storage.DB.First(&code).Error; err != nil || code.IsUsed {
				t.Fatalf("competing renewal consumed card: %+v %v", code, err)
			}
			if err := account.BindLocalUserWithAudit(3, "stale-login", "lifecycle_abs", "hash", nil); err == nil {
				t.Fatal("stale login rebound a deleted ABS account")
			}
			if err := runAccountLifecycleAt(bot, time.Now()); err != nil || calls.Load() != 3 {
				t.Fatalf("completed deletion was repeated: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

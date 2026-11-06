package game

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningAbuseScanSkipsAllImmuneUsers(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	for _, u := range []model.User{
		{TelegramID: 1, Username: "configured", AbsUserID: "abs1", Role: "user", Status: "active"},
		{TelegramID: 2, Username: "stored", AbsUserID: "abs2", Role: "super_admin", Status: "active"},
		{TelegramID: 3, Username: "whitelisted", AbsUserID: "abs3", Role: "user", Status: "active", IsWhitelist: true},
	} {
		if err := storage.DB.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	warnings, freezes, _, err := scanListeningAbuseForDay(nil, "2026-11-02", time.Now())
	if err != nil || warnings != 0 || freezes != 0 || calls.Load() != 0 {
		t.Fatalf("immune users reached scan: warnings=%d freezes=%d calls=%d err=%v", warnings, freezes, calls.Load(), err)
	}
}

func TestListeningAbuseRechecksImmunityBeforeWarningAndFreeze(t *testing.T) {
	for _, immunity := range []string{"configured", "stored", "whitelist"} {
		t.Run(immunity, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			previousConfig := config.AppConfig
			config.AppConfig = &config.Config{AdminIDs: map[int64]bool{}}
			t.Cleanup(func() { config.AppConfig = previousConfig })
			u := model.User{TelegramID: 1, Username: "promoted", AbsUserID: "abs1", Role: "user", Status: "active"}
			if err := storage.DB.Create(&u).Error; err != nil {
				t.Fatal(err)
			}
			// Keep the ordinary-user snapshot that a scan took before immunity
			// was granted. Both penalty paths must check the current identity.
			switch immunity {
			case "configured":
				config.AppConfig.AdminIDs[u.TelegramID] = true
			case "stored":
				if err := storage.DB.Model(&u).Update("role", "super_admin").Error; err != nil {
					t.Fatal(err)
				}
				u.Role = "user"
			case "whitelist":
				if err := storage.DB.Model(&u).Update("is_whitelist", true).Error; err != nil {
					t.Fatal(err)
				}
				u.IsWhitelist = false
			}
			var calls atomic.Int64
			bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			now := time.Now()
			if warned, err := createListeningAbuseWarning(nil, u, "2026-11-02", 20, now); err != nil || warned {
				t.Fatalf("immune user warned: warned=%t err=%v", warned, err)
			}
			if frozen, err := startListeningAbuseFreeze(nil, u, "2026-11-02", "2026-11-01", 20, 20, now); err != nil || frozen {
				t.Fatalf("immune user frozen: frozen=%t err=%v", frozen, err)
			}
			for _, table := range []any{&model.ListeningAbuseRecord{}, &model.AuditLog{}} {
				var count int64
				if err := storage.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("immune user left penalty records: table=%T count=%d err=%v", table, count, err)
				}
			}
			if calls.Load() != 0 {
				t.Fatalf("immune user reached ABS mutation: calls=%d", calls.Load())
			}
		})
	}
}

func TestListeningAbuseWarningStillAppliesToOrdinaryUsersAndAdmins(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		t.Run(role, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			previousConfig := config.AppConfig
			config.AppConfig = &config.Config{}
			t.Cleanup(func() { config.AppConfig = previousConfig })
			u := model.User{TelegramID: 1, Username: role, AbsUserID: "abs1", Role: role, Status: "active"}
			if err := storage.DB.Create(&u).Error; err != nil {
				t.Fatal(err)
			}
			if warned, err := createListeningAbuseWarning(nil, u, "2026-11-02", 20, time.Now()); err != nil || !warned {
				t.Fatalf("ordinary %s escaped warning: warned=%t err=%v", role, warned, err)
			}
		})
	}
}

func dueListeningAbuseFreezeFixture(t *testing.T) (model.User, model.ListeningAbuseRecord) {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	u := model.User{TelegramID: 1, Username: "freeze-user", AbsUserID: "abs1"}
	if err := storage.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-time.Minute)
	r := model.ListeningAbuseRecord{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-01", Action: storage.ListeningAbuseActionFreeze, Status: storage.ListeningAbuseStatusActive, FreezeEndAt: &end}
	if err := storage.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	return u, r
}

func TestAbuseReleaseHonorsAccountStateAndWhitelist(t *testing.T) {
	for _, state := range []string{"admin_banned", "unbound", "expired", "whitelist_expired", "not_due"} {
		t.Run(state, func(t *testing.T) {
			u, r := dueListeningAbuseFreezeFixture(t)
			fields := map[string]any{}
			switch state {
			case "admin_banned":
				fields["admin_suspended"] = true
			case "unbound":
				fields["status"] = "unbound"
			case "expired", "whitelist_expired":
				fields["expire_at"] = time.Now().Add(-time.Hour)
				fields["is_whitelist"] = state == "whitelist_expired"
			case "not_due":
				if err := storage.DB.Model(&r).Update("freeze_end_at", time.Now().Add(time.Hour)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if len(fields) > 0 {
				if err := storage.DB.Model(&u).Updates(fields).Error; err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int64
			bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"id":"abs1","isActive":false}`)
			})
			released, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, time.Now())
			want := state == "whitelist_expired"
			if err != nil || released != want || (calls.Load() > 0) != want {
				t.Fatalf("invalid restoration: released=%t calls=%d error=%v", released, calls.Load(), err)
			}
		})
	}
}

func TestAbuseReleaseRetriesFailureAndDoesNotRepeatCompletedRelease(t *testing.T) {
	u, r := dueListeningAbuseFreezeFixture(t)
	var fail atomic.Bool
	fail.Store(true)
	var activations atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		if req.Method == http.MethodPatch {
			activations.Add(1)
		}
		fmt.Fprint(w, `{"id":"abs1","isActive":false}`)
	})
	if _, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, time.Now()); err == nil {
		t.Fatal("ABS failure was ignored")
	}
	var claim model.AccountLifecycleClaim
	if err := storage.DB.First(&claim, "user_row_id = ?", u.ID).Error; err != nil || claim.ReferenceID != r.ID {
		t.Fatalf("failed restoration lost its durable claim: %+v error=%v", claim, err)
	}
	fail.Store(false)
	if released, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, time.Now()); err != nil || !released {
		t.Fatalf("retry failed: released=%t error=%v", released, err)
	}
	if released, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, time.Now()); err != nil || released || activations.Load() != 1 {
		t.Fatalf("completed release repeated: released=%t activations=%d error=%v", released, activations.Load(), err)
	}
	var claims int64
	if err := storage.DB.Model(&model.AccountLifecycleClaim{}).Where("user_row_id = ?", u.ID).Count(&claims).Error; err != nil || claims != 0 {
		t.Fatalf("completed release left a claim: count=%d error=%v", claims, err)
	}
}

func TestFailedAbuseReleaseHandsExpiredAccountBackToLifecycle(t *testing.T) {
	u, r := dueListeningAbuseFreezeFixture(t)
	now := time.Now()
	if err := storage.DB.Model(&u).Update("expire_at", now.Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(503) })
	if _, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, now); err == nil {
		t.Fatal("ABS failure was ignored")
	}
	if released, err := lifecycle.ReleaseListeningAbuseFreeze(r.ID, now.Add(2*time.Minute)); err != nil || released {
		t.Fatalf("expired account restored: released=%t error=%v", released, err)
	}
	var claims int64
	if err := storage.DB.Model(&model.AccountLifecycleClaim{}).Where("user_row_id = ?", u.ID).Count(&claims).Error; err != nil || claims != 0 {
		t.Fatalf("expired account remains locked: count=%d error=%v", claims, err)
	}
	if err := storage.DB.First(&r, r.ID).Error; err != nil || r.Status != storage.ListeningAbuseStatusReleaseBlocked {
		t.Fatalf("expired release not blocked: %+v error=%v", r, err)
	}
}

func TestAbuseFreezeAuditFailureDoesNotActivateOrSuspendABS(t *testing.T) {
	u, r := dueListeningAbuseFreezeFixture(t)
	if err := storage.DB.Delete(&r).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE TRIGGER reject_abuse_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"id":"abs1"}`)
	})
	started, err := startListeningAbuseFreeze(nil, u, "2026-11-02", "2026-11-01", 20, 20, time.Now())
	if err == nil || started || calls.Load() != 0 {
		t.Fatalf("failed freeze touched ABS: started=%t calls=%d error=%v", started, calls.Load(), err)
	}
}

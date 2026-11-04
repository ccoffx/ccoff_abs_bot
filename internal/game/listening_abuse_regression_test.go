package game

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

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

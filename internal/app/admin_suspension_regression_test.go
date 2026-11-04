package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestAdminMutationsProtectConfiguredSuperAdminBeforeABSCall(t *testing.T) {
	lifecycleFixture(t, time.Now().Add(time.Hour), false, func(w http.ResponseWriter, r *http.Request) {
		t.Error("configured super admin reached ABS mutation")
	})
	config.AppConfig.AdminIDs[2] = true
	if err := changeAdminSuspensionWithAudit(1, 2, "lifecycle_abs", true, "SUSPEND_USER", "test"); !errors.Is(err, storage.ErrTargetIsSuperAdmin) {
		t.Fatalf("configured admin suspension accepted: %v", err)
	}
	if err := deleteAccountWithAudit(1, 2, "lifecycle_abs", "FORCE_DELETE_USER", nil); !errors.Is(err, storage.ErrTargetIsSuperAdmin) {
		t.Fatalf("configured admin deletion accepted: %v", err)
	}
	if status, err := promoteAdminWithAudit(1, 2, "test"); err != nil || status != adminMutationTargetSuperAdmin {
		t.Fatalf("configured admin role changed: %s %v", status, err)
	}
	if status, err := setWhitelistWithAudit(1, 2, "test"); err != nil || status != adminMutationTargetSuperAdmin {
		t.Fatalf("configured admin whitelist changed: %s %v", status, err)
	}
	if status, _, err := simulateExpireWithAudit(1, 2, "test"); err != nil || status != adminMutationTargetSuperAdmin {
		t.Fatalf("configured admin expiry changed: %s %v", status, err)
	}
}

func TestAdminSuspensionRecordsReasonAndManualUnlockClearsIt(t *testing.T) {
	var patches atomic.Int64
	var active atomic.Bool
	lifecycleFixture(t, time.Now().Add(time.Hour), false, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var payload struct {
				IsActive bool `json:"isActive"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			active.Store(payload.IsActive)
			patches.Add(1)
		}
		fmt.Fprint(w, `{"id":"lifecycle_abs"}`)
	})
	for _, suspended := range []bool{true, false} {
		if err := changeAdminSuspensionWithAudit(1, 2, "lifecycle_abs", suspended, "ADMIN_SUSPEND_TEST", "test"); err != nil {
			t.Fatal(err)
		}
		var u model.User
		if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil || u.IsSuspended != suspended || u.AdminSuspended != suspended || active.Load() == suspended {
			t.Fatalf("admin state not synchronized: suspended=%t user=%+v active=%t err=%v", suspended, u, active.Load(), err)
		}
	}
	if patches.Load() != 2 {
		t.Fatalf("expected suspension and unlock; patches=%d", patches.Load())
	}
}

func TestAdminSuspensionPendingReactivationRejectsBeforeABSCall(t *testing.T) {
	var calls atomic.Int64
	lifecycleFixture(t, time.Now().Add(time.Hour), true, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"id":"lifecycle_abs"}`)
	})
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.AccountLifecycleClaim{UserRowID: u.ID, Kind: "reactivate", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := changeAdminSuspensionWithAudit(1, 2, u.AbsUserID, true, "ADMIN_SUSPEND_TEST", "test"); err == nil {
		t.Fatal("admin suspension bypassed pending reactivation")
	}
	if calls.Load() != 0 {
		t.Fatalf("guard rejected after ABS was changed: calls=%d", calls.Load())
	}
}

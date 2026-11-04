package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func orphanCleanupFixture(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	channelOpsFixture(t)
	if err := storage.DB.AutoMigrate(&model.OrphanAccountDeletion{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.InitializeOrphanAccountDeletionGuards(storage.DB); err != nil {
		t.Fatal(err)
	}
	old := abs.DefaultClient
	srv := httptest.NewServer(handler)
	abs.DefaultClient = &abs.AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
	t.Cleanup(func() { abs.DefaultClient = old; srv.Close() })
}

func TestOrphanCleanupSkipsAccountBoundAfterPreview(t *testing.T) {
	orphanCleanupFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("bound account reached ABS deletion") })
	expiry := time.Now().Add(24 * time.Hour)
	if err := account.BindLocalUserWithAudit(2, "new_binding", "preview_abs", "hash", &expiry); err != nil {
		t.Fatal(err)
	}
	deleted, err := deleteOrphanAccount(1, "preview_abs")
	if err != nil || deleted {
		t.Fatalf("deleted bound account: deleted=%t err=%v", deleted, err)
	}
	var count int64
	storage.DB.Model(&model.OrphanAccountDeletion{}).Count(&count)
	if count != 0 {
		t.Fatal("skipping a bound account retained a deletion claim")
	}
}

func TestOrphanCleanupBlocksStaleBindingDuringAndAfterDeletion(t *testing.T) {
	calls := 0
	orphanCleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"id":"preview_abs","username":"alice","type":"user"}`)
			return
		}
		calls++
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s", r.Method)
		}
		if err := account.BindLocalUserWithAudit(2, "stale_login", "preview_abs", "hash", nil); err == nil {
			t.Error("binding raced past durable deletion claim")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	if _, err := deleteOrphanAccount(1, "preview_abs"); err == nil {
		t.Fatal("ABS failure treated as success")
	}
	if err := account.BindLocalUserWithAudit(2, "stale_login", "preview_abs", "hash", nil); err == nil {
		t.Fatal("uncertain deletion allowed binding")
	}
	if deleted, err := deleteOrphanAccount(1, "preview_abs"); err != nil || !deleted {
		t.Fatalf("retry failed: %t %v", deleted, err)
	}
	if err := account.BindLocalUserWithAudit(2, "stale_login", "preview_abs", "hash", nil); err == nil {
		t.Fatal("stale login bound deleted ABS identity")
	}
	if deleted, err := deleteOrphanAccount(1, "preview_abs"); err != nil || !deleted || calls != 2 {
		t.Fatalf("completed deletion not idempotent: %t %v calls=%d", deleted, err, calls)
	}
}

func TestOrphanCleanupRechecksRoleAndProtectsTemplate(t *testing.T) {
	for _, role := range []string{"admin", "root", "", "unknown"} {
		t.Run(role, func(t *testing.T) {
			orphanCleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("protected or unverified account reached deletion")
				}
				fmt.Fprintf(w, `{"id":"preview_abs","username":"alice","type":%q}`, role)
			})
			deleted, err := deleteOrphanAccount(1, "preview_abs")
			if deleted || ((role == "admin" || role == "root") != (err == nil)) {
				t.Fatalf("deleted=%t err=%v", deleted, err)
			}
			if err == nil {
				var count int64
				if err := storage.DB.Model(&model.OrphanAccountDeletion{}).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("protected account kept deletion claim: %d %v", count, err)
				}
			}
		})
	}
	t.Run("template", func(t *testing.T) {
		orphanCleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("template account reached ABS cleanup")
		})
		config.AppConfig.AbsTemplateID = "preview_abs"
		if deleted, err := deleteOrphanAccount(1, "preview_abs"); err != nil || deleted {
			t.Fatalf("template account not protected: %t %v", deleted, err)
		}
	})
}

func TestOrphanCleanupVerifiesIdentityAndHandlesAlreadyDeletedAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		deleted bool
	}{
		{"missing", 404, `{}`, true},
		{"unavailable", 503, `{}`, false},
		{"wrong_identity", 200, `{"id":"another","username":"alice","type":"user"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orphanCleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("failed verification reached deletion")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			deleted, err := deleteOrphanAccount(1, "preview_abs")
			if deleted != tc.deleted || ((err == nil) != tc.deleted) {
				t.Fatalf("deleted=%t err=%v", deleted, err)
			}
		})
	}
}

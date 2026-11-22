package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestStartSendDoesNotBlockUpdateDispatch(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{}
	defer func() { config.AppConfig = previous }()
	var sends atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bottest/getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		sends.Add(1)
		<-release
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	jobs := make(chan telegramMessageJob, 2)
	defer func() {
		for len(jobs) > 0 {
			(<-jobs).turn.finish()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchTelegramUpdate(bot, jobs, tgbotapi.Update{Message: &tgbotapi.Message{From: &tgbotapi.User{ID: 88210}, Chat: &tgbotapi.Chat{ID: 88210, Type: "private"}, Text: "/start"}})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		once.Do(func() { close(release) })
		<-done
		t.Fatal("poll dispatch waited for Telegram")
	}
	if sends.Load() != 0 || len(jobs) != 1 {
		t.Fatalf("/start performed network work in receive loop: sends=%d jobs=%d", sends.Load(), len(jobs))
	}
}

func TestAdminStatusNetworkDoesNotHoldSQLiteWriter(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	lifecycleFixture(t, time.Now().Add(time.Hour), false, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		fmt.Fprint(w, `{"id":"lifecycle_abs"}`)
	})
	done := make(chan error, 1)
	go func() {
		done <- changeAdminSuspensionWithAudit(1, 2, "lifecycle_abs", true, "SUSPEND_USER", "writer probe")
	}()
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("status request did not finish")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ABS request not started")
	}
	wrote := make(chan error, 1)
	go func() {
		wrote <- storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).UpdateColumn("points", 123).Error
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow ABS held SQLite's writer")
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).UpdateColumn("abs_user_id", "replacement").Error; err == nil {
		t.Fatal("in-flight claim did not protect the target binding")
	}
}

func TestAdminStatusFailureRetainsIntentForRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	lifecycleFixture(t, time.Now().Add(time.Hour), false, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"id":"lifecycle_abs"}`)
	})
	if err := changeAdminSuspensionWithAudit(1, 2, "lifecycle_abs", true, "SUSPEND_USER", "retry probe"); err == nil {
		t.Fatal("failure was hidden")
	}
	var user model.User
	var claim model.AccountLifecycleClaim
	if err := storage.DB.First(&user, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&claim, "user_row_id = ?", user.ID).Error; err != nil || claim.Kind != "admin_suspend" {
		t.Fatalf("lost durable status intent: %+v %v", claim, err)
	}
	if err := runAccountLifecycleAt(nil, time.Now()); err != nil {
		t.Fatalf("expiry scanner mishandled a pending administrative intent: %v", err)
	}
	fail.Store(false)
	completed, err := completeAdminStatusChange(2)
	if err != nil || completed == nil {
		t.Fatalf("persisted intent did not resume: %v", err)
	}
	if err := storage.DB.First(&user, user.ID).Error; err != nil || !user.AdminSuspended || !user.IsSuspended {
		t.Fatalf("retry did not finalize local state: %+v %v", user, err)
	}
	if completed, err = completeAdminStatusChange(2); err != nil || completed != nil {
		t.Fatal("completed intent was replayed", err)
	}
}

func TestAdminStatusRetriesRotatePastFailingBatch(t *testing.T) {
	lifecycleFixture(t, time.Now().Add(time.Hour), false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	now := time.Now()
	for id := int64(10000); id < 10025; id++ {
		user := model.User{TelegramID: id, Username: fmt.Sprintf("retry_user_%d", id), AbsUserID: fmt.Sprintf("retry_abs_%d", id), Status: "active"}
		if err := storage.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := prepareAdminStatusChange(1, id, user.AbsUserID, true, "SUSPEND_USER", "rotation"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := dueAdminStatusUsers(now.Add(time.Second))
	if err != nil || len(first) != 20 {
		t.Fatalf("first batch: %d %v", len(first), err)
	}
	seen := map[int64]bool{}
	for _, user := range first {
		seen[user.TelegramID] = true
		if _, err := completeAdminStatusChange(user.TelegramID); err == nil {
			t.Fatal("expected ABS failure")
		}
		var claim model.AccountLifecycleClaim
		if err := storage.DB.First(&claim, "user_row_id = ?", user.ID).Error; err != nil || claim.LastAttemptAt == nil || claim.NextRetryAt == nil || claim.RetryAttempts != 1 {
			t.Fatalf("missing durable retry metadata: %+v %v", claim, err)
		}
	}
	remaining, err := dueAdminStatusUsers(time.Now())
	if err != nil || len(remaining) != 5 {
		t.Fatalf("remaining batch: %d %v", len(remaining), err)
	}
	for _, user := range remaining {
		if seen[user.TelegramID] {
			t.Fatal("failed batch blocked unattempted work")
		}
	}
	// Once backoff expires, never-attempted work still sorts before retries.
	later, err := dueAdminStatusUsers(time.Now().Add(2 * time.Minute))
	if err != nil || len(later) != 20 {
		t.Fatalf("later batch: %d %v", len(later), err)
	}
	for _, user := range later[:5] {
		if seen[user.TelegramID] {
			t.Fatal("retry overtook unattempted work")
		}
	}
	for attempts, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if got := adminStatusRetryDelay(attempts); got != want {
			t.Fatalf("backoff %d: %v != %v", attempts, got, want)
		}
	}
}

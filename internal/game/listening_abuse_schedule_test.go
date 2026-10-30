package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningAbuseStartsAt0010Beijing(t *testing.T) {
	for _, tc := range []struct{ at, want string }{
		{"2026-10-28T00:00:00+08:00", ""},
		{"2026-10-28T00:09:59+08:00", ""},
		{"2026-10-28T00:10:00+08:00", "2026-10-27"},
		{"2026-10-27T16:10:00Z", "2026-10-27"},
		{"2026-10-28T23:59:59+08:00", "2026-10-27"},
		{"2026-10-29T00:09:59+08:00", ""},
	} {
		at, err := time.Parse(time.RFC3339, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := listeningAbuseScanDayKey(at); got != tc.want {
			t.Errorf("at %s: got %q, want %q", tc.at, got, tc.want)
		}
	}
}

func setupListeningAbuseScheduleDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "abuse.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	previousDB, previousClient := storage.DB, abs.DefaultClient
	storage.DB = db
	t.Cleanup(func() {
		storage.DB, abs.DefaultClient = previousDB, previousClient
		_ = conn.Close()
	})
	if err := db.AutoMigrate(&model.User{}, &model.DailyListeningStat{}, &model.SystemConfig{},
		&model.ListeningAbuseRecord{}, &model.AuditLog{}, &model.ABSCrossDaySessionAdjustment{}, &model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE UNIQUE INDEX idx_test_config ON system_configs(key) WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX idx_test_daily ON daily_listening_stats(user_id, day_key) WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX idx_test_abuse ON listening_abuse_records(user_id, day_key, action) WHERE deleted_at IS NULL`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestListeningAbuseRefreshesBeforeScanAndRetriesIncompleteBatch(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	for id := int64(1); id <= 2; id++ {
		if err := storage.DB.Create(&model.User{
			TelegramID: id, Username: fmt.Sprint("user", id), AbsUserID: fmt.Sprint("abs", id),
			Status: "active", Role: "user",
		}).Error; err != nil {
			t.Fatal(err)
		}
		// A fresh today-only cache must not bypass the ABS request for closed days.
		if err := storage.DB.Create(&model.DailyListeningStat{
			UserID: id, DayKey: SectDayKey(time.Now()), LastFetchedAt: time.Now(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	requests := map[string]int{}
	freezeRequests := 0
	var requestsMu sync.Mutex
	requestCount := func(path string) int {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		return requests[path]
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			previousHours := 1
			if strings.Contains(r.URL.Path, "/abs1/") {
				previousHours = 16
			}
			payload := abuseTestSessions(previousHours)
			if strings.Contains(r.URL.Path, "/abs2/") {
				payload = strings.ReplaceAll(payload, "abs1", "abs2")
			}
			fmt.Fprint(w, `{"sessions":`+payload+`}`)
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			requestsMu.Lock()
			requests[r.URL.Path]++
			attempt := requests[r.URL.Path]
			requestsMu.Unlock()
			if strings.Contains(r.URL.Path, "/abs2/") && attempt == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{}`)
				return
			}
			if strings.Contains(r.URL.Path, "/abs1/") {
				fmt.Fprint(w, `{"days":{"2026-10-27":57600,"2026-10-26":57600}}`)
			} else {
				fmt.Fprint(w, `{"days":{"2026-10-27":57600,"2026-10-26":3600}}`)
			}
		case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"id":"abs1","username":"user1","type":"user","permissions":{},"librariesAccessible":[]}`)
		case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodPatch:
			requestsMu.Lock()
			freezeRequests++
			requestsMu.Unlock()
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected ABS request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(-time.Second))
	if requestCount("/api/users/abs1/listening-stats") != 0 || requestCount("/api/users/abs2/listening-stats") != 0 {
		t.Fatal("scan ran before 00:10")
	}
	RunListeningAbuseMonitorIfNeeded(nil, now)
	assertScanDay := func(want string) {
		t.Helper()
		got, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
		if err != nil || got != want {
			t.Fatalf("scan day=%q err=%v, want %q", got, err, want)
		}
	}
	assertScanDay("")
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(time.Minute))
	if requestCount("/api/users/abs2/listening-stats") != 1 {
		t.Fatal("retry did not respect five-minute interval")
	}
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(5*time.Minute))
	assertScanDay("2026-10-27")
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(10*time.Minute))
	if requestCount("/api/users/abs1/listening-stats") != 1 || requestCount("/api/users/abs2/listening-stats") != 2 {
		t.Fatal("successful user or completed batch was repeated")
	}
	var warnings int64
	if err := storage.DB.Model(&model.ListeningAbuseRecord{}).
		Where("action = ?", storage.ListeningAbuseActionWarning).Count(&warnings).Error; err != nil || warnings != 1 {
		t.Fatalf("warnings=%d err=%v, want one warning", warnings, err)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if freezeRequests != 1 {
		t.Fatalf("freeze requests=%d, want one", freezeRequests)
	}
}

func TestListeningAbuseDoesNotCompleteWithoutOfficialDays(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"totalTime":100000}`)
	}))
	t.Cleanup(server.Close)
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
	RunListeningAbuseMonitorIfNeeded(nil, now)
	if day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || day != "" {
		t.Fatalf("missing days marked scan complete: day=%q err=%v", day, err)
	}
}

func TestListeningAbuseRetriesCrossDayCorrectionFailures(t *testing.T) {
	for _, failure := range []string{"sessions_unavailable", "storage_unavailable"} {
		t.Run(failure, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			sessionsUnavailable := failure == "sessions_unavailable"
			if failure == "storage_unavailable" {
				if err := storage.DB.Migrator().DropTable(&model.ABSCrossDaySessionAdjustment{}); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					fmt.Fprint(w, `{"days":{"2026-10-27":57600,"2026-10-26":3600}}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/listening-sessions") {
					mu.Lock()
					unavailable := sessionsUnavailable
					mu.Unlock()
					if unavailable {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(w, `{"sessions":`+abuseTestSessions(1)+`}`)
					return
				}
				t.Errorf("unexpected ABS request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
			now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
			RunListeningAbuseMonitorIfNeeded(nil, now)
			var count int64
			if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("failed correction produced penalties: %d, %v", count, err)
			}
			if day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || day != "" {
				t.Fatalf("failed correction completed scan: %q, %v", day, err)
			}
			// Normal views retain the official fallback even while punitive scans wait.
			if days, ok := RefreshDailyListeningStatsFromABS(1, "abs1"); !ok || days["2026-10-27"] != 57600 {
				t.Fatalf("official fallback unavailable: %v, %t", days, ok)
			}
			mu.Lock()
			sessionsUnavailable = false
			mu.Unlock()
			if failure == "storage_unavailable" {
				if err := storage.DB.AutoMigrate(&model.ABSCrossDaySessionAdjustment{}); err != nil {
					t.Fatal(err)
				}
			}
			RunListeningAbuseMonitorIfNeeded(nil, now.Add(listeningAbuseRetryInterval))
			if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("retry did not warn exactly once: %d, %v", count, err)
			}
			if day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || day != "2026-10-27" {
				t.Fatalf("retry did not complete: %q, %v", day, err)
			}
		})
	}
}

func abuseTestSessions(previousHours int) string {
	return fmt.Sprintf(`[{"id":"target","userId":"abs1","date":"2026-10-27","startedAt":"2026-10-27T00:00:00+08:00","updatedAt":"2026-10-27T16:00:00+08:00","timeListening":57600},{"id":"previous","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T00:00:00+08:00","updatedAt":"2026-10-26T%02d:00:00+08:00","timeListening":%d}]`, previousHours, previousHours*3600)
}

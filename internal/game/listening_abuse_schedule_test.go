package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
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

func TestListeningAbuseCatchesUpOneDayAtATime(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey, "2026-10-25"); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
	RunListeningAbuseMonitorIfNeeded(nil, now)
	day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
	if err != nil || day != "2026-10-26" {
		t.Fatalf("first catch-up day=%q err=%v", day, err)
	}
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(listeningAbuseRetryInterval))
	day, err = storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
	if err != nil || day != "2026-10-27" {
		t.Fatalf("second catch-up day=%q err=%v", day, err)
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
	if err := db.AutoMigrate(&model.User{}, &model.AccountLifecycleClaim{}, &model.DailyListeningStat{}, &model.ListeningReportSnapshot{}, &model.SystemConfig{},
		&model.Cultivation{}, &model.CultivationUpgradeEvent{}, &model.Sect{}, &model.SectMember{}, &model.SectContributionLog{}, &model.ListeningAbuseRecord{}, &model.AuditLog{}, &model.ABSCrossDaySessionAdjustment{}, &model.SectCaveRetreat{}); err != nil {
		t.Fatal(err)
	}
	contributionMust(t, db.AutoMigrate(&model.GardenPlanting{}, &model.GardenAlchemyRecord{}))
	contributionMust(t, storage.InitializeGardenProductionTotals(db))
	contributionMust(t, storage.InitializeListeningInputCache(db))
	if err := storage.InitializeAccountLifecycleGuards(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(append(spiritfire.Models(), &model.SectOwnerBonusRevision{}, &model.SectOwnerBonusState{})...); err != nil {
		t.Fatal(err)
	}
	if err := spiritfire.Initialize(db, time.Now()); err != nil {
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

func saveAbuseReport(t *testing.T, userID int64, absID, day string, seconds float64) model.DailyListeningStat {
	t.Helper()
	stat := model.DailyListeningStat{UserID: userID, AbsUserID: absID, DayKey: day,
		RawSeconds: seconds, LastFetchedAt: time.Now()}
	if err := storage.DB.Create(&stat).Error; err != nil {
		t.Fatal(err)
	}
	return stat
}

func TestListeningAbuseUsesSavedReportWithoutABSRequests(t *testing.T) {
	for _, tc := range []struct {
		name             string
		target, previous float64
		wantAction       string
	}{
		{"reported incident", 52218, 74468, ""},
		{"exactly fifteen hours", 54000, 57600, ""},
		{"one second over", 54001, 54000, storage.ListeningAbuseActionWarning},
		{"previous exactly fifteen", 57600, 54000, storage.ListeningAbuseActionWarning},
		{"two reports over limit", 57600, 57600, storage.ListeningAbuseActionFreeze},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Status: "active", Role: "user"}).Error; err != nil {
				t.Fatal(err)
			}
			saveAbuseReport(t, 1, "abs1", "2026-10-27", tc.target)
			saveAbuseReport(t, 1, "abs1", "2026-10-26", tc.previous)
			// Deliberately different credited seconds must not become the basis.
			if err := storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("capped_seconds", 3600).Error; err != nil {
				t.Fatal(err)
			}
			var patches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodGet:
					fmt.Fprint(w, `{"id":"abs1","username":"user1","type":"user","permissions":{},"librariesAccessible":[]}`)
				case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodPatch:
					patches.Add(1)
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("scan must not fetch or recalculate reports: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
			now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
			RunListeningAbuseMonitorIfNeeded(nil, now.Add(-time.Second))
			var count int64
			if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("scan ran before 00:10: %d %v", count, err)
			}
			RunListeningAbuseMonitorIfNeeded(nil, now)
			RunListeningAbuseMonitorIfNeeded(nil, now.Add(10*time.Minute))
			var records []model.ListeningAbuseRecord
			if err := storage.DB.Find(&records).Error; err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.wantAction != "" {
				wantCount = 1
			}
			if len(records) != wantCount {
				t.Fatalf("records=%+v; want %d", records, wantCount)
			}
			if wantCount > 0 && (records[0].Action != tc.wantAction || records[0].RawHours != tc.target/3600) {
				t.Fatalf("penalty differs from saved report: %+v", records)
			}
			wantPatches := 0
			if tc.wantAction == storage.ListeningAbuseActionFreeze {
				wantPatches = 1
				if records[0].PreviousRawHours != tc.previous/3600 {
					t.Fatalf("previous report differs: %+v", records[0])
				}
			}
			if patches.Load() != int64(wantPatches) {
				t.Fatalf("freeze requests=%d want %d", patches.Load(), wantPatches)
			}
			day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
			if err != nil || day != "2026-10-27" {
				t.Fatalf("scan day=%q err=%v", day, err)
			}
			var report model.DailyListeningStat
			if err := storage.DB.First(&report, "user_id = ? AND day_key = ?", 1, "2026-10-27").Error; err != nil || report.RawSeconds != tc.target {
				t.Fatalf("scan changed saved report: %+v %v", report, err)
			}
		})
	}
}

func TestListeningAbuseSkipsUnavailableSavedReports(t *testing.T) {
	for _, state := range []string{"missing", "other_binding", "deleted", "never_fetched", "missing_previous"} {
		t.Run(state, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
				t.Fatal(err)
			}
			if state != "missing" {
				absID := "abs1"
				if state == "other_binding" {
					absID = "old_abs"
				}
				stat := saveAbuseReport(t, 1, absID, "2026-10-27", 57600)
				if state == "deleted" {
					if err := storage.DB.Delete(&stat).Error; err != nil {
						t.Fatal(err)
					}
				}
				if state == "never_fetched" {
					if err := storage.DB.Model(&stat).Update("last_fetched_at", time.Time{}).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if state != "missing_previous" {
				saveAbuseReport(t, 1, "abs1", "2026-10-26", 57600)
			}
			abs.DefaultClient = nil
			warnings, freezes, _, err := scanListeningAbuseForDay(nil, "2026-10-27", time.Now())
			wantWarnings := 0
			if state == "missing_previous" {
				wantWarnings = 1
			}
			if err != nil || warnings != wantWarnings || freezes != 0 {
				t.Fatalf("unavailable report caused penalty: warnings=%d freezes=%d err=%v", warnings, freezes, err)
			}
		})
	}
}

func TestListeningAbuseRetriesSavedReportReadFailures(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	abs.DefaultClient = nil
	if err := storage.DB.Migrator().DropTable(&model.DailyListeningStat{}); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
	RunListeningAbuseMonitorIfNeeded(nil, now)
	if day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || day != "" {
		t.Fatalf("failed read completed scan: %q %v", day, err)
	}
	if err := storage.DB.AutoMigrate(&model.DailyListeningStat{}); err != nil {
		t.Fatal(err)
	}
	saveAbuseReport(t, 1, "abs1", "2026-10-27", 57600)
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(time.Minute))
	var count int64
	if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("retry ignored backoff: %d %v", count, err)
	}
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(listeningAbuseRetryInterval))
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(2*listeningAbuseRetryInterval))
	if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("retry did not warn exactly once: %d %v", count, err)
	}
	if day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey); err != nil || day != "2026-10-27" {
		t.Fatalf("retry did not complete: %q %v", day, err)
	}
}

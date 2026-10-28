package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupRetreatBackfillDB(t *testing.T) {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.ABSCrossDaySessionScanCursor{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalBackfillRecordsRetreatBeyondRecentFivePages(t *testing.T) {
	setupRetreatBackfillDB(t)
	start, _ := time.Parse(time.RFC3339, "2026-10-26T04:00:00+08:00")
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(4 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	if err := storage.DB.Create(&retreat).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			fmt.Fprint(w, `{"days":{"2026-10-26":28800}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/listening-sessions") {
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Error(err)
			return
		}
		if page == 5 {
			// Same-day sessions do not create cross-day adjustments, but still prove
			// retreat playback and must not be discarded by the historical scanner.
			fmt.Fprint(w, `[{"id":"old-retreat","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T04:00:00+08:00","updatedAt":"2026-10-26T08:00:00+08:00","timeListening":14400}]`)
			return
		}
		if page < 0 || page > 5 {
			t.Errorf("unexpected page %d", page)
			return
		}
		sessions := make([]map[string]interface{}, dailyListeningSessionPageSize)
		for i := range sessions {
			sessions[i] = map[string]interface{}{"id": fmt.Sprintf("recent-%d-%d", page, i), "userId": "abs1", "date": "2026-10-27", "startedAt": "2026-10-27T10:00:00+08:00", "updatedAt": "2026-10-27T10:01:00+08:00", "timeListening": 60}
		}
		_ = json.NewEncoder(w).Encode(sessions)
	}))
	defer server.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	if _, ok := RefreshDailyListeningStatsFromABS(1, "abs1"); !ok {
		t.Fatal("normal refresh failed")
	}
	assertTotal := func(want float64) {
		t.Helper()
		total, ok := totalCultivationEffectiveHoursFromDailyStats(1, start.AddDate(0, 0, 2))
		if !ok || math.Abs(total-want) > 1e-9 {
			t.Fatalf("cumulative=%g ok=%t; want %g", total, ok, want)
		}
	}
	assertTotal(6.4)
	if err := EnsureABSCrossDaySessionScanCursor(1, "abs1"); err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	var cursor model.ABSCrossDaySessionScanCursor
	if err := storage.DB.First(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	if cursor.NextPage != 5 || cursor.Completed {
		t.Fatalf("unexpected first batch: %+v", cursor)
	}
	assertTotal(6.4)
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	assertTotal(7.2)
	if err := storage.DB.First(&cursor, cursor.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cursor.NextPage != 6 || !cursor.Completed {
		t.Fatalf("last historical page not committed: %+v", cursor)
	}
	// Replaying an already observed page must retain one piece of evidence.
	if err := storage.DB.Model(&cursor).Updates(map[string]interface{}{"next_page": 5, "completed": false}).Error; err != nil {
		t.Fatal(err)
	}
	if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
		t.Fatal(err)
	}
	assertTotal(7.2)
	if err := storage.DB.First(&retreat, retreat.ID).Error; err != nil {
		t.Fatal(err)
	}
	evidence, err := decodeSectRetreatEvidence(retreat)
	if err != nil || len(evidence) != 1 || evidence["abs1:old-retreat"]["2026-10-26"] != 14400 {
		t.Fatalf("evidence=%v err=%v", evidence, err)
	}
}

func TestHistoricalBackfillRollsBackEvidenceAndCursorTogether(t *testing.T) {
	for _, failure := range []string{"evidence_write", "cursor_write"} {
		t.Run(failure, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start, _ := time.Parse(time.RFC3339, "2026-10-26T22:00:00+08:00")
			retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(4 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
			if err := storage.DB.Create(&retreat).Error; err != nil {
				t.Fatal(err)
			}
			cursor := model.ABSCrossDaySessionScanCursor{UserID: 1, AbsUserID: "abs1", NextPage: 5}
			if err := storage.DB.Create(&cursor).Error; err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") != "5" {
					t.Errorf("cursor skipped failed page: %s", r.URL.RawQuery)
				}
				fmt.Fprint(w, `[{"id":"cross-retreat","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T22:00:00+08:00","updatedAt":"2026-10-27T02:00:00+08:00","timeListening":14400}]`)
			}))
			defer server.Close()
			abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
			table, column := "sect_cave_retreats", "listening_evidence_json"
			if failure == "cursor_write" {
				table, column = "abs_cross_day_session_scan_cursors", "next_page"
			}
			trigger := fmt.Sprintf("CREATE TRIGGER reject_backfill BEFORE UPDATE OF %s ON %s BEGIN SELECT RAISE(ABORT, 'injected failure'); END", column, table)
			if err := storage.DB.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			if err := advanceABSCrossDaySessionBackfill(time.Now()); err == nil {
				t.Fatal("injected failure did not abort page")
			}
			if err := storage.DB.First(&cursor, cursor.ID).Error; err != nil {
				t.Fatal(err)
			}
			if cursor.NextPage != 5 || cursor.Completed || cursor.PagesScanned != 0 {
				t.Fatalf("failed page advanced cursor: %+v", cursor)
			}
			if err := storage.DB.First(&retreat, retreat.ID).Error; err != nil {
				t.Fatal(err)
			}
			if retreat.ListeningEvidenceJSON != "" {
				t.Fatalf("failed page committed evidence: %s", retreat.ListeningEvidenceJSON)
			}
			var count int64
			if err := storage.DB.Model(&model.ABSCrossDaySessionAdjustment{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("failed page committed adjustments: %d %v", count, err)
			}
			if err := storage.DB.Exec("DROP TRIGGER reject_backfill").Error; err != nil {
				t.Fatal(err)
			}
			if err := advanceABSCrossDaySessionBackfill(time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.First(&cursor, cursor.ID).Error; err != nil {
				t.Fatal(err)
			}
			if cursor.NextPage != 6 || !cursor.Completed || cursor.PagesScanned != 1 {
				t.Fatalf("retry did not commit cursor: %+v", cursor)
			}
			if err := storage.DB.First(&retreat, retreat.ID).Error; err != nil {
				t.Fatal(err)
			}
			evidence, err := decodeSectRetreatEvidence(retreat)
			if err != nil || len(evidence) != 1 {
				t.Fatalf("retry evidence=%v %v", evidence, err)
			}
			if err := storage.DB.Model(&model.ABSCrossDaySessionAdjustment{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("retry adjustments=%d %v", count, err)
			}
		})
	}
}

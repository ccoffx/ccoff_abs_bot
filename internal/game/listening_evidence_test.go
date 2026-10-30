package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningAbuseDoesNotPunishEstimatedMidnightPlayback(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			fmt.Fprint(w, `{"days":{"2026-10-26":58200,"2026-10-27":54000}}`)
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			fmt.Fprint(w, `{"sessions":[
    {"id":"previous","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T00:00:00+08:00","updatedAt":"2026-10-26T16:00:00+08:00","timeListening":57600},
    {"id":"cross","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T23:50:00+08:00","updatedAt":"2026-10-27T00:10:00+08:00","timeListening":600},
    {"id":"target","userId":"abs1","date":"2026-10-27","startedAt":"2026-10-27T08:00:00+08:00","updatedAt":"2026-10-27T23:00:00+08:00","timeListening":54000}]}`)
		default:
			t.Errorf("unexpected request (possible freeze): %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	estimated, ok := refreshDailyListeningStatsFromABS(1, "abs1", true)
	if !ok || math.Abs(estimated["2026-10-27"]-54300) > 1e-9 {
		t.Fatalf("expected display estimate: %v %t", estimated, ok)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
	RunListeningAbuseMonitorIfNeeded(nil, now)
	var penalties int64
	if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&penalties).Error; err != nil || penalties != 0 {
		t.Fatalf("estimated playback was punished: %d %v", penalties, err)
	}
	day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
	if err != nil || day != "2026-10-27" {
		t.Fatalf("scan did not complete: %q %v", day, err)
	}
}

func TestGuaranteedPlaybackWindowBounds(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-10-28T10:00:00+08:00")
	for _, tc := range []struct{ wall, listen, want float64 }{{4, 2, 0}, {4, 3, 3600}, {4, 4, 7200}, {2, 2, 7200}, {2, 3, 0}} {
		session := absListeningSession{SessionKey: "s", StartedAt: start, UpdatedAt: start.Add(time.Duration(tc.wall) * time.Hour), ListeningSeconds: tc.listen * 3600, HasListeningTime: true}
		if got := guaranteedSessionSeconds(session, start, start.Add(2*time.Hour)); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("wall/listen=%g/%g got %g want %g", tc.wall, tc.listen, got, tc.want)
		}
	}
}

func TestRetreatIgnoresLatePlaybackAndPersistsProvenEvidence(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	start, _ := time.Parse(time.RFC3339, "2026-10-28T10:00:00+08:00")
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(2 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	if err := storage.DB.Create(&retreat).Error; err != nil {
		t.Fatal(err)
	}
	// No playback in the retreat; all two hours could have happened afterward.
	late := absListeningSession{SessionKey: "late", StartedAt: start, UpdatedAt: start.Add(4 * time.Hour), ListeningSeconds: 2 * 3600, HasListeningTime: true}
	if err := recordSectRetreatListeningSessions(1, "abs1", []absListeningSession{late}); err != nil {
		t.Fatal(err)
	}
	stat := model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(start), CappedSeconds: 6 * 3600, EffectiveHours: 5.2}
	check := func(want float64) {
		t.Helper()
		if err := storage.DB.First(&retreat, retreat.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got := dailySectCaveRetreatBonusHours(stat, []model.SectCaveRetreat{retreat}); math.Abs(got-want) > 1e-9 {
			t.Fatalf("bonus %g want %g", got, want)
		}
	}
	check(0)
	// An in-window observation proves one hour. A later snapshot with pauses
	// cannot erase that earlier evidence or count the same session again.
	observed := absListeningSession{SessionKey: "observed", StartedAt: start, UpdatedAt: start.Add(time.Hour), ListeningSeconds: 3600, HasListeningTime: true}
	if err := recordSectRetreatListeningSessions(1, "abs1", []absListeningSession{observed, observed}); err != nil {
		t.Fatal(err)
	}
	check(0.2)
	observed.UpdatedAt = start.Add(4 * time.Hour)
	if err := recordSectRetreatListeningSessions(1, "abs1", []absListeningSession{observed, late}); err != nil {
		t.Fatal(err)
	}
	check(0.2)
	// No evidence is awarded to a retreat just because the whole-day total grew.
	empty := retreat
	empty.ListeningEvidenceJSON = ""
	if got := dailySectCaveRetreatBonusHours(stat, []model.SectCaveRetreat{empty}); got != 0 {
		t.Fatalf("unobserved bonus %g", got)
	}
}

func TestSecretRealmUsesHistoricalRetreatTotalAndFailsClosed(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.Cultivation{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	start, _ := sectDayRange(time.Now().AddDate(0, 0, -1))
	day := SectDayKey(start)
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start.Add(4 * time.Hour), EndAt: start.Add(8 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	retreat.ListeningEvidenceJSON = retreatTestEvidence(retreat.StartAt, retreat.EndAt, 4*3600)
	if err := storage.DB.Create(&retreat).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"days": map[string]float64{day: 8 * 3600}})
			return
		}
		fmt.Fprint(w, `{"sessions":[]}`)
	}))
	defer server.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	snapshot, err := fetchSectSecretRealmListeningSnapshotFromABS("abs1")
	if err != nil || math.Abs(snapshot.EffectiveHours-7.2) > 1e-9 {
		t.Fatalf("ABS snapshot lost history: %+v %v", snapshot, err)
	}
	local, ok := freshSectSecretRealmSnapshotFromLocalDailyStats("abs1")
	if !ok || math.Abs(local.EffectiveHours-7.2) > 1e-9 {
		t.Fatalf("local snapshot lost history: %+v %t", local, ok)
	}
	if err := storage.DB.Create(&model.Cultivation{UserID: 1, TotalAudioTime: 7.2}).Error; err != nil {
		t.Fatal(err)
	}
	updateCultivationAudioHours(1, snapshot.EffectiveHours)
	var cultivation model.Cultivation
	if err := storage.DB.Where("user_id = ?", 1).First(&cultivation).Error; err != nil || math.Abs(cultivation.TotalAudioTime-7.2) > 1e-9 {
		t.Fatalf("realm sync reduced cultivation: %+v %v", cultivation, err)
	}
	if err := storage.DB.Model(&retreat).UpdateColumn("listening_evidence_json", "invalid").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fetchSectSecretRealmListeningSnapshotFromABS("abs1"); err == nil {
		t.Fatal("broken evidence silently returned a smaller cumulative value")
	}
	if _, ok := freshSectSecretRealmSnapshotFromLocalDailyStats("abs1"); ok {
		t.Fatal("local fallback discarded broken historical evidence")
	}
}

func TestListeningAbuseProvesMissingOfficialDates(t *testing.T) {
	for _, tc := range []struct {
		name, stats, sessions string
		wantAction            string
		wantPatches           int
	}{
		{
			name:       "yesterday_missing",
			stats:      `{"days":{"2026-10-26":61200}}`,
			sessions:   `[{"id":"cross","userId":"abs1","date":"2026-10-26","startedAt":"2026-10-26T23:00:00+08:00","updatedAt":"2026-10-27T16:00:00+08:00","timeListening":61200}]`,
			wantAction: storage.ListeningAbuseActionWarning,
		},
		{
			name:  "previous_day_missing",
			stats: `{"days":{"2026-10-27":57600}}`,
			sessions: `[
    {"id":"cross","userId":"abs1","date":"2026-10-25","startedAt":"2026-10-25T23:00:00+08:00","updatedAt":"2026-10-26T16:00:00+08:00","timeListening":61200},
    {"id":"target","userId":"abs1","date":"2026-10-27","startedAt":"2026-10-27T00:00:00+08:00","updatedAt":"2026-10-27T16:00:00+08:00","timeListening":57600}]`,
			wantAction:  storage.ListeningAbuseActionFreeze,
			wantPatches: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			if err := storage.DB.Create(&model.User{TelegramID: 1, AbsUserID: "abs1", Role: "user", Status: "active"}).Error; err != nil {
				t.Fatal(err)
			}
			var patches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/listening-stats"):
					fmt.Fprint(w, tc.stats)
				case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
					fmt.Fprint(w, `{"sessions":`+tc.sessions+`}`)
				case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodGet:
					fmt.Fprint(w, `{"id":"abs1","username":"user1","type":"user","permissions":{},"librariesAccessible":[]}`)
				case r.URL.Path == "/api/users/abs1" && r.Method == http.MethodPatch:
					patches.Add(1)
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
			now, _ := time.Parse(time.RFC3339, "2026-10-28T00:10:00+08:00")
			RunListeningAbuseMonitorIfNeeded(nil, now)
			var records []model.ListeningAbuseRecord
			if err := storage.DB.Find(&records).Error; err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("records=%v; want one %s", records, tc.wantAction)
			}
			record := records[0]
			if record.Action != tc.wantAction || math.Abs(record.RawHours-16) > 1e-9 {
				t.Fatalf("incorrect action or target hours: %+v", record)
			}
			if tc.wantPatches > 0 && math.Abs(record.PreviousRawHours-16) > 1e-9 {
				t.Fatalf("missing previous-day evidence: %+v", record)
			}
			if int(patches.Load()) != tc.wantPatches {
				t.Fatalf("freeze requests=%d want %d", patches.Load(), tc.wantPatches)
			}
			day, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseLastScanDayKey)
			if err != nil || day != "2026-10-27" {
				t.Fatalf("scan not completed after processing evidence: %q %v", day, err)
			}
			RunListeningAbuseMonitorIfNeeded(nil, now.Add(10*time.Minute))
			var count int64
			if err := storage.DB.Model(&model.ListeningAbuseRecord{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("completed scan repeated: %d %v", count, err)
			}
		})
	}
}

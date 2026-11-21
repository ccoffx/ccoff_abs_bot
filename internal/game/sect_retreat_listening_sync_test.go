package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupRetreatListeningSync(t *testing.T) (model.SectCaveRetreat, time.Time) {
	t.Helper()
	setupRetreatBackfillDB(t)
	now := time.Now()
	start := sectStartOfDay(now).AddDate(0, 0, -1).Add(10 * time.Hour)
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(2 * time.Hour), BaseRawSeconds: 14400, Status: "expired"}
	contributionMust(t, storage.DB.Create(&retreat).Error)
	writeOfficial(t, map[string]float64{SectDayKey(start): 21600})
	// Reproduce the production state: the one-time history scan has finished.
	contributionMust(t, storage.DB.Create(&model.ABSCrossDaySessionScanCursor{UserID: 1, AbsUserID: "abs1", Completed: true, DailyValidationVersion: dailyListeningValidationVersion}).Error)
	return retreat, now
}

func retreatListeningSyncStat(t *testing.T, retreat model.SectCaveRetreat) model.SectCaveRetreat {
	t.Helper()
	var saved model.SectCaveRetreat
	contributionMust(t, storage.DB.First(&saved, retreat.ID).Error)
	return saved
}

func TestRetreatListeningSyncAfterHistoryCompleted(t *testing.T) {
	retreat, now := setupRetreatListeningSync(t)
	requests := 0
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/listening-sessions") {
			t.Errorf("evidence sync must not request ordinary stats: %s", r.URL.Path)
			w.WriteHeader(503)
			return
		}
		requests++
		inside := bossSession("inside", retreat.StartAt, retreat.EndAt, 7200)
		late := bossSession("late", retreat.EndAt, retreat.EndAt.Add(2*time.Hour), 7200)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{inside, inside, late}})
	})
	day := SectDayKey(retreat.StartAt)
	before := officialStat(t, day)
	_, _, _, err := scanListeningAbuseForDay(nil, day, now)
	contributionMust(t, err)
	if requests != 0 {
		t.Fatal("abuse scan fetched sessions")
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now))
	if got := officialTotal(t); math.Abs(got-5.6) > 1e-9 {
		t.Fatalf("retreat bonus missing or duplicated: total=%g want=5.6", got)
	}
	after := officialStat(t, day)
	if after.RawSeconds != before.RawSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) {
		t.Fatal("evidence sync changed the saved ordinary report")
	}
	saved := retreatListeningSyncStat(t, retreat)
	evidence, err := decodeSectRetreatEvidence(saved)
	contributionMust(t, err)
	if len(evidence) != 1 || evidence["abs1:inside"][day] != 7200 {
		t.Fatalf("duplicate or out-of-window playback credited: %v", evidence)
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)))
	if got := officialTotal(t); math.Abs(got-5.6) > 1e-9 {
		t.Fatalf("replay duplicated bonus: %g", got)
	}
}

func TestRetreatListeningSyncResumesBeyondFivePagesAndRetries(t *testing.T) {
	retreat, now := setupRetreatListeningSync(t)
	var pages []int
	fail := true
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Error(err)
		}
		pages = append(pages, page)
		if page == 5 {
			if fail {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{bossSession("inside", retreat.StartAt, retreat.EndAt, 7200)}})
			return
		}
		sessions := make([]any, dailyListeningSessionPageSize)
		for i := range sessions {
			sessions[i] = bossSession(fmt.Sprintf("empty-%d-%d", page, i), retreat.EndAt, retreat.EndAt.Add(time.Hour), 0)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now))
	saved := retreatListeningSyncStat(t, retreat)
	if saved.ListeningScanPage != 5 || saved.ListeningScanCompletedAt != nil || len(pages) != 5 {
		t.Fatalf("first batch not bounded: saved=%+v pages=%v", saved, pages)
	}
	if err := RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)); err == nil {
		t.Fatal("failed page was accepted")
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval+time.Minute)))
	if len(pages) != 6 || retreatListeningSyncStat(t, retreat).ListeningScanPage != 5 {
		t.Fatalf("retry ignored backoff or advanced the failed page: %v", pages)
	}
	fail = false
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(2*ListeningSyncInterval)))
	saved = retreatListeningSyncStat(t, retreat)
	if saved.ListeningScanPage != 6 || saved.ListeningScanCompletedAt == nil || fmt.Sprint(pages) != "[0 1 2 3 4 5 5]" || math.Abs(officialTotal(t)-5.6) > 1e-9 {
		t.Fatalf("resume missed retreat playback: saved=%+v pages=%v total=%g", saved, pages, officialTotal(t))
	}
}

func TestRetreatListeningSyncRollsBackPageWithSettlement(t *testing.T) {
	retreat, now := setupRetreatListeningSync(t)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{bossSession("inside", retreat.StartAt, retreat.EndAt, 7200)}})
	})
	contributionMust(t, storage.DB.Exec(`CREATE TRIGGER reject_retreat_cursor BEFORE UPDATE OF listening_scan_page ON sect_cave_retreats BEGIN SELECT RAISE(ABORT, 'injected failure'); END`).Error)
	if err := RunSectRetreatListeningSyncIfNeeded(now); err == nil {
		t.Fatal("cursor failure was accepted")
	}
	saved := retreatListeningSyncStat(t, retreat)
	if saved.ListeningEvidenceJSON != "" || saved.ListeningScanPage != 0 || math.Abs(officialTotal(t)-5.2) > 1e-9 {
		t.Fatalf("failed page committed proof or settlement: saved=%+v total=%g", saved, officialTotal(t))
	}
	if saved.ListeningScanAttemptAt == nil || !saved.ListeningScanAttemptAt.Equal(now) {
		t.Fatal("rolled-back page lost its retry backoff")
	}
	contributionMust(t, storage.DB.Exec("DROP TRIGGER reject_retreat_cursor").Error)
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now))
	if math.Abs(officialTotal(t)-5.2) > 1e-9 {
		t.Fatal("failed page retried before the backoff elapsed")
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)))
	if math.Abs(officialTotal(t)-5.6) > 1e-9 {
		t.Fatal("retry lost the rolled-back bonus")
	}
}

func TestRetreatListeningSyncWaitsForBatchPublication(t *testing.T) {
	retreat, now := setupRetreatListeningSync(t)
	var requests atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{bossSession("inside", retreat.StartAt, retreat.EndAt, 7200)}})
	})
	listeningRefreshMu.Lock()
	locked := true
	defer func() {
		if locked {
			listeningRefreshMu.Unlock()
		}
	}()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- RunSectRetreatListeningSyncIfNeeded(now)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("retreat sync bypassed the full-refresh barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if requests.Load() != 0 || math.Abs(officialTotal(t)-5.2) > 1e-9 || retreatListeningSyncStat(t, retreat).ListeningScanAttemptAt != nil {
		t.Fatal("retreat sync fetched or changed data during batch publication")
	}
	listeningRefreshMu.Unlock()
	locked = false
	select {
	case err := <-done:
		contributionMust(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("retreat sync did not resume after batch publication")
	}
	if requests.Load() != 1 || math.Abs(officialTotal(t)-5.6) > 1e-9 {
		t.Fatal("resumed retreat sync lost its evidence or settlement")
	}
}

func TestRetreatListeningSyncFailedPagesYieldToOtherUsers(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	start := now.Add(-48 * time.Hour)
	for i := 0; i < 20; i++ {
		retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(time.Hour), Status: "expired"}
		if i == 0 {
			retreat.ListeningEvidenceJSON = "broken-json"
		}
		contributionMust(t, storage.DB.Create(&retreat).Error)
	}
	contributionMust(t, storage.DB.Create(&model.User{TelegramID: 2, Username: "healthy", AbsUserID: "abs2", Role: "user", Status: "active"}).Error)
	healthy := model.SectCaveRetreat{UserID: 2, StartAt: start, EndAt: start.Add(time.Hour), Status: "expired"}
	contributionMust(t, storage.DB.Create(&healthy).Error)
	var failedRequests, healthyRequests atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/abs2/") {
			healthyRequests.Add(1)
		} else {
			failedRequests.Add(1)
		}
		fmt.Fprint(w, `{"sessions":[]}`)
	})
	if err := RunSectRetreatListeningSyncIfNeeded(now); err == nil {
		t.Fatal("broken evidence was accepted")
	}
	if failedRequests.Load() != 20 || healthyRequests.Load() != 0 {
		t.Fatal("test did not fill the first batch with failing retreats")
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(time.Minute)))
	if failedRequests.Load() != 20 || healthyRequests.Load() != 1 || retreatListeningSyncStat(t, healthy).ListeningScanCompletedAt == nil {
		t.Fatal("failed retreats ignored backoff or starved the healthy user")
	}
	if err := RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)); err == nil || failedRequests.Load() != 40 {
		t.Fatal("failed retreats did not retry after the backoff elapsed")
	}
}

func TestRetreatListeningSyncRechecksDelayedUploadsAndFinishes(t *testing.T) {
	retreat, _ := setupRetreatListeningSync(t)
	// The first completed pass happens shortly after the retreat ends.
	now := retreat.EndAt.Add(time.Minute)
	requests := 0
	uploaded := false
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		sessions := []any{}
		if uploaded {
			sessions = append(sessions, bossSession("delayed", retreat.StartAt, retreat.EndAt, 7200))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now))
	uploaded = true
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)))
	if math.Abs(officialTotal(t)-5.6) > 1e-9 || requests != 2 {
		t.Fatal("completed pass prevented delayed playback from being collected")
	}
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(retreat.EndAt.Add(24*time.Hour)))
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(retreat.EndAt.Add(25*time.Hour)))
	if requests != 3 {
		t.Fatalf("final pass did not stop polling: requests=%d", requests)
	}
}

func TestRetreatListeningSyncRejectsRebindDuringFetch(t *testing.T) {
	retreat, now := setupRetreatListeningSync(t)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "abs2").Error)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{bossSession("old", retreat.StartAt, retreat.EndAt, 7200)}})
	})
	if err := RunSectRetreatListeningSyncIfNeeded(now); err == nil {
		t.Fatal("old account response accepted after rebind")
	}
	saved := retreatListeningSyncStat(t, retreat)
	if saved.ListeningEvidenceJSON != "" || saved.ListeningScanPage != 0 || math.Abs(officialTotal(t)-5.2) > 1e-9 {
		t.Fatalf("rebind committed stale proof: %+v", saved)
	}
}

func TestRetreatListeningSyncFinalPassStartsAfterUploadGrace(t *testing.T) {
	retreat, _ := setupRetreatListeningSync(t)
	// An old pass can finish after the grace period without having seen
	// sessions uploaded to its already-consumed first page.
	started := retreat.EndAt.Add(23 * time.Hour)
	contributionMust(t, storage.DB.Model(&retreat).UpdateColumns(map[string]any{
		"listening_scan_abs_user_id": "abs1", "listening_scan_page": 5,
		"listening_scan_started_at": started,
	}).Error)
	now := retreat.EndAt.Add(24 * time.Hour)
	var pages []string
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		sessions := []any{}
		if page == "0" {
			sessions = append(sessions, bossSession("delayed", retreat.StartAt, retreat.EndAt, 7200))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now))
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(ListeningSyncInterval)))
	contributionMust(t, RunSectRetreatListeningSyncIfNeeded(now.Add(2*ListeningSyncInterval)))
	if fmt.Sprint(pages) != "[5 0]" || math.Abs(officialTotal(t)-5.6) > 1e-9 {
		t.Fatalf("final pass missed a late upload: pages=%v total=%g", pages, officialTotal(t))
	}
}

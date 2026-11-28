package game

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestPreviousListeningDayClosesOnceWithoutAdvancingToday(t *testing.T) {
	setupRetreatBackfillDB(t)
	contributionMust(t, storage.InitializeListeningInputCache(storage.DB))
	now := time.Now()
	midnight := sectStartOfDay(now)
	yesterday, older := SectDayKey(midnight.AddDate(0, 0, -1)), SectDayKey(midnight.AddDate(0, 0, -2))
	olderAt := midnight.AddDate(0, 0, -1).Add(-5 * time.Minute)
	_, err := recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{older: 600}, olderAt, olderAt)
	contributionMust(t, err)
	last := midnight.Add(-5 * time.Minute)
	_, err = recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{yesterday: 3600}, last, last)
	contributionMust(t, err)
	_, err = recordCurrentListeningDay(1, "abs1", 0, now)
	contributionMust(t, err)
	var before model.ListeningReportSnapshot
	contributionMust(t, storage.DB.First(&before, "user_id = ?", 1).Error)
	pendingTodayAt := time.Now()

	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		day := r.URL.Query().Get("day")
		if day != yesterday && day != older {
			t.Errorf("unexpected refresh: %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seconds := 600
		if day == yesterday {
			seconds = 4200
		}
		requests.Add(1)
		fmt.Fprintf(w, `{"days":{%q:%d}}`, day, seconds)
	})
	contributionMust(t, ClosePreviousListeningDay(time.Now()))
	contributionMust(t, ClosePreviousListeningDay(time.Now()))

	closed := officialStat(t, yesterday)
	if !closed.Archived || closed.OfficialRawSeconds != 4200 || closed.CappedSeconds != 4200 || requests.Load() != 2 {
		t.Fatalf("previous day not closed exactly once: row=%+v requests=%d", closed, requests.Load())
	}
	if row := officialStat(t, older); !row.Archived || row.OfficialRawSeconds != 600 {
		t.Fatalf("older pending day was not closed: %+v", row)
	}
	var after model.ListeningReportSnapshot
	contributionMust(t, storage.DB.First(&after, "user_id = ?", 1).Error)
	if !after.StatsFetchedAt.Equal(before.StatsFetchedAt) || after.OfficialInputJSON != "" {
		t.Fatalf("yesterday changed today's freshness or left stale input cache: %+v", after)
	}
	if got, want := officialTotal(t), calculateSectEffectiveHoursFromSeconds(4200)+calculateSectEffectiveHoursFromSeconds(600); math.Abs(got-want) > 1e-9 {
		t.Fatalf("unexpected accrued total: got=%v want=%v", got, want)
	}
	// A request for today started before yesterday's final response still writes
	// once its response arrives. Freshness ordering is scoped to the source day.
	_, err = recordCurrentListeningDay(1, "abs1", 0, pendingTodayAt)
	contributionMust(t, err)
}

func TestPreviousListeningDayRetriesOnlyUnclosedRows(t *testing.T) {
	setupRetreatBackfillDB(t)
	contributionMust(t, storage.DB.Create(&model.User{TelegramID: 2, Username: "close-test-user", AbsUserID: "abs2", Status: "active"}).Error)
	last := sectStartOfDay(time.Now()).Add(-time.Minute)
	yesterday := SectDayKey(last)
	for id := int64(1); id <= 2; id++ {
		_, err := recordOfficialDailyListeningStatsAt(id, fmt.Sprint("abs", id), map[string]float64{yesterday: 600}, last, last)
		contributionMust(t, err)
	}
	var first, second atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("day") != yesterday {
			t.Errorf("unexpected refresh: %s", r.URL)
		}
		if strings.Contains(r.URL.Path, "/abs1/") {
			first.Add(1)
		} else if second.Add(1) == 1 {
			fmt.Fprint(w, `{"days":{}}`)
			return
		}
		fmt.Fprintf(w, `{"days":{%q:0}}`, yesterday)
	})
	if err := ClosePreviousListeningDay(time.Now()); err == nil {
		t.Fatal("missing date unexpectedly accepted")
	}
	var pending model.DailyListeningStat
	contributionMust(t, storage.DB.First(&pending, "user_id = ? AND day_key = ?", 2, yesterday).Error)
	if pending.Archived {
		t.Fatal("failed date was archived")
	}
	contributionMust(t, ClosePreviousListeningDay(time.Now()))
	contributionMust(t, storage.DB.First(&pending, pending.ID).Error)
	if first.Load() != 1 || second.Load() != 2 || !pending.Archived || pending.OfficialRawSeconds != 0 || pending.CappedSeconds != 600 {
		t.Fatalf("retry did not preserve the high-water mark or skip successes: first=%d second=%d row=%+v", first.Load(), second.Load(), pending)
	}
}

func TestDayCloseCreatesMissingYesterdayAndRetriesOlderRequests(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	midnight := sectStartOfDay(now)
	yesterday := SectDayKey(midnight.AddDate(0, 0, -1))
	older := SectDayKey(midnight.AddDate(0, 0, -2))
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).UpdateColumn("created_at", midnight.AddDate(0, 0, -3)).Error)
	// This request was persisted during an earlier midnight and survived restart.
	contributionMust(t, storage.DB.Create(&model.ListeningDayClose{UserID: 1, AbsUserID: "abs1", DayKey: older, Status: "pending"}).Error)
	var retry atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		day := r.URL.Query().Get("day")
		if day == older && !retry.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if day != older && day != yesterday {
			t.Errorf("unexpected closing date %s", day)
		}
		fmt.Fprintf(w, `{"days":{%q:600}}`, day)
	})
	if err := ClosePreviousListeningDay(now); err == nil {
		t.Fatal("expected the older request to remain pending")
	}
	if row := officialStat(t, yesterday); !row.Archived || row.CappedSeconds != 600 {
		t.Fatalf("missing yesterday was not created and closed: %+v", row)
	}
	contributionMust(t, seedListeningDayCloses(now.AddDate(0, 0, 1)))
	// Model the new current date's task as already completed. The old pending
	// request must remain eligible even after its seed day moved forward.
	contributionMust(t, storage.DB.Model(&model.ListeningDayClose{}).Where("day_key = ?", SectDayKey(now)).Update("status", "completed").Error)
	retry.Store(true)
	contributionMust(t, ClosePreviousListeningDay(now))
	var pending int64
	contributionMust(t, storage.DB.Model(&model.ListeningDayClose{}).Where("status = ?", "pending").Count(&pending).Error)
	if pending != 0 || !officialStat(t, older).Archived || math.Abs(officialTotal(t)-1.0/3) > 1e-9 {
		t.Fatalf("older request was lost or counted twice: pending=%d total=%v", pending, officialTotal(t))
	}
}

func TestAbuseWaitsForClosingWhileOtherUsersProceed(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	last := sectStartOfDay(now).Add(-time.Minute)
	day := SectDayKey(last)
	_, err := recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{day: 53900}, last, last)
	contributionMust(t, err)
	contributionMust(t, storage.DB.Create(&model.User{TelegramID: 2, Username: "ready-report", AbsUserID: "abs2", Status: "active", Role: "user"}).Error)
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: day, RawSeconds: 55000, Archived: true, OfficialPolicy: true, LastFetchedAt: time.Now()}).Error)
	contributionMust(t, seedListeningDayCloses(now))
	warnings, _, _, err := scanListeningAbuseForDay(nil, day, now)
	if err == nil || warnings != 1 {
		t.Fatalf("pending account blocked a ready account or completed early: warnings=%d err=%v", warnings, err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"days":{%q:55000}}`, r.URL.Query().Get("day"))
	})
	contributionMust(t, ClosePreviousListeningDay(now))
	warnings, _, _, err = scanListeningAbuseForDay(nil, day, now)
	contributionMust(t, err)
	if warnings != 1 {
		t.Fatalf("late closing was not scanned or a previous warning repeated: %d", warnings)
	}
	var records int64
	contributionMust(t, storage.DB.Model(&model.ListeningAbuseRecord{}).Where("day_key = ?", day).Count(&records).Error)
	if records != 2 {
		t.Fatalf("expected exactly one warning per account: %d", records)
	}
}

func TestDayCloseCancelsOldBindingBeforeABS(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(sectStartOfDay(time.Now()).AddDate(0, 0, -2))
	request := model.ListeningDayClose{UserID: 1, AbsUserID: "former-account", DayKey: day, Status: "pending"}
	contributionMust(t, storage.DB.Create(&request).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("stale binding requested ABS: %s", r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	contributionMust(t, ClosePreviousListeningDay(time.Now()))
	contributionMust(t, storage.DB.First(&request, request.ID).Error)
	if request.Status != "cancelled" {
		t.Fatalf("stale binding remained queued: %+v", request)
	}
}

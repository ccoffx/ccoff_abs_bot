package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestBossSnapshotUsesCurrentDayDeltaAndFreezesFinalInput(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	event.Status = "settling"
	contributionMust(t, storage.DB.Save(&event).Error)
	baseline := dailyListeningCreditSeconds(SectDayKey(event.StartAt), 7200, event.StartAt)
	p.ListeningSnapshot = model.ActivityListeningSnapshot{BaselineDay: SectDayKey(event.StartAt), BaselineSeconds: baseline}
	contributionMust(t, storage.DB.Save(&p).Error)
	var calls atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/listening-days") {
			t.Error("activity requested history")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 9999999, "days": map[string]float64{SectDayKey(event.StartAt): baseline + 1800, "2000-01-01": 999999}})
	})
	hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
	if err != nil || !complete || math.Abs(hours-.5) > 1e-9 {
		t.Fatalf("hours=%v complete=%v err=%v", hours, complete, err)
	}
	contributionMust(t, storage.DB.First(&p, p.ID).Error)
	hours, complete, err = worldBossListeningHours(&p, event, p.AbsUserID, time.Now().Add(time.Hour))
	if err != nil || !complete || math.Abs(hours-.5) > 1e-9 || calls.Load() != 1 {
		t.Fatalf("final replay changed input: hours=%v calls=%d err=%v", hours, calls.Load(), err)
	}
	if _, _, err = worldBossListeningHours(&p, event, "another-account", time.Now()); err == nil {
		t.Fatal("binding change accepted")
	}
}

func TestActivityFinalFailureOrLateRestartUsesSavedProgress(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "ABS unavailable", true: "late restart"}[late], func(t *testing.T) {
			event, p := bossEvidenceFixture(t)
			event.Status = "settling"
			if late {
				event.EndAt = time.Now().Add(-10 * time.Minute)
				event.StartAt = event.EndAt.Add(-time.Hour)
				p.CreatedAt = event.StartAt
			}
			contributionMust(t, storage.DB.Save(&event).Error)
			p.DeltaHours = .25
			contributionMust(t, storage.DB.Save(&p).Error)
			var calls atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
			hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
			if !late {
				if !errors.Is(err, ErrActivitySnapshotPending) || complete || p.ListeningSnapshot.Finalized {
					t.Fatalf("failure did not stay pending: %+v %v", p, err)
				}
				contributionMust(t, storage.DB.First(&p, p.ID).Error)
				hours, complete, err = worldBossListeningHours(&p, event, p.AbsUserID, event.EndAt.Add(activityFinalSnapshotGrace))
			}
			if err != nil || !complete || math.Abs(hours-.25) > 1e-9 || !p.ListeningSnapshot.Finalized || p.ListeningSnapshot.FinalFallbackReason == "" {
				t.Fatalf("saved progress lost: %v %v %v", hours, complete, err)
			}
			if late && calls.Load() != 0 {
				t.Fatal("late restart requested post-cutoff statistics")
			}
		})
	}
}

func TestActivityFinalRetriesMissingTodayAndFreezesPreviousDay(t *testing.T) {
	for _, crossMidnight := range []bool{false, true} {
		t.Run(fmt.Sprint("cross_midnight=", crossMidnight), func(t *testing.T) {
			event, p := bossEvidenceFixture(t)
			if crossMidnight {
				event.StartAt = event.StartAt.AddDate(0, 0, -1)
				p.CreatedAt = event.StartAt
			}
			event.Status = "settling"
			day := SectDayKey(event.StartAt)
			p.DeltaHours = .1
			baseline := dailyListeningCreditSeconds(day, 7200, event.StartAt)
			p.ListeningSnapshot = model.ActivityListeningSnapshot{BaselineDay: day, BaselineSeconds: baseline}
			contributionMust(t, storage.DB.Save(&event).Error)
			contributionMust(t, storage.DB.Save(&p).Error)
			var healthy atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				days := map[string]float64{}
				today := SectDayKey(time.Now())
				if r.URL.Query().Get("day") != today {
					t.Errorf("unexpected listening date: %s", r.URL)
				}
				if crossMidnight {
					days[today] = 0
				} else if healthy.Load() {
					days[day] = baseline + 600
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"days": days})
			})
			hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
			if crossMidnight {
				if err != nil || !complete || hours != .1 {
					t.Fatalf("saved progress should settle after midnight: hours=%v complete=%v err=%v", hours, complete, err)
				}
				return
			}
			if !errors.Is(err, ErrActivitySnapshotPending) || complete || p.ListeningSnapshot.Finalized {
				t.Fatalf("missing day finalized incomplete input: %+v err=%v", p.ListeningSnapshot, err)
			}
			healthy.Store(true)
			p.ListeningSnapshot.FinalNextRetryAt = nil
			hours, complete, err = worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
			if err != nil || !complete || math.Abs(hours-600.0/3600) > 1e-9 {
				t.Fatalf("complete retry lost growth: hours=%v complete=%v err=%v", hours, complete, err)
			}
		})
	}
}

func TestActivityFinalAfterCutoffDayPreservesSavedSampleWithoutABS(t *testing.T) {
	for _, beforeMidnight := range []time.Duration{0, time.Minute} {
		t.Run(beforeMidnight.String(), func(t *testing.T) {
			event, p := bossEvidenceFixture(t)
			midnight := time.Date(2026, 11, 28, 0, 0, 0, 0, storage.DailyOperationsLocation)
			event.EndAt = midnight.Add(-beforeMidnight)
			event.StartAt = event.EndAt.Add(-time.Hour)
			event.Status = "settling"
			p.CreatedAt = event.StartAt
			p.DeltaHours = .25
			sampledAt := event.EndAt.Add(-3 * time.Minute)
			nextRetryAt := midnight.Add(time.Minute)
			p.ListeningSnapshot = model.ActivityListeningSnapshot{
				BaselineDay: SectDayKey(event.StartAt), FetchedAt: &sampledAt, FinalNextRetryAt: &nextRetryAt,
			}
			contributionMust(t, storage.DB.Save(&event).Error)
			contributionMust(t, storage.DB.Save(&p).Error)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("closed activity requested %s", r.URL)
				w.WriteHeader(http.StatusBadGateway)
			})
			hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, midnight.Add(30*time.Second))
			if err != nil || !complete || hours != .25 {
				t.Fatalf("saved progress should settle immediately: hours=%v complete=%v err=%v", hours, complete, err)
			}
			contributionMust(t, storage.DB.First(&p, p.ID).Error)
			snapshot := p.ListeningSnapshot
			if !snapshot.Finalized || snapshot.FetchedAt == nil || !snapshot.FetchedAt.Equal(sampledAt) ||
				snapshot.FinalFallbackReason != "day_closed" || snapshot.FinalNextRetryAt != nil ||
				!strings.Contains(activityListeningSnapshotNotice(snapshot), "活动截止日已结束") {
				t.Fatalf("incorrect fallback snapshot: %+v", snapshot)
			}
		})
	}
}

func TestActivityFinalDailyInputAllowsArchivedAndZeroDays(t *testing.T) {
	snapshot := model.ActivityListeningSnapshot{DailyInputJSON: `{"2026-11-20":{"s":3600,"a":true},"2026-11-21":{"s":0,"a":false},"2026-11-22":{"s":600,"a":false}}`}
	if err := validateActivityFinalDailyInput(map[string]float64{"2026-11-22": 600}, snapshot); err != nil {
		t.Fatalf("closed history or absent zero day required another sample: %v", err)
	}
	if err := validateActivityFinalDailyInput(map[string]float64{}, snapshot); err == nil {
		t.Fatal("missing unarchived playback day accepted")
	}
}

func TestBossLiveSnapshotPersistsWithoutAwardingAndFinalOnlyAddsDelta(t *testing.T) {
	event := worldBossLiveFixture(t, 1)
	var p model.WorldBossParticipant
	contributionMust(t, storage.DB.First(&p, "boss_id = ?", event.BossID).Error)
	baseline := int64(dailyListeningCreditSeconds(SectDayKey(event.StartAt), 3600, event.StartAt))
	p.ListeningSnapshot = model.ActivityListeningSnapshot{BaselineDay: SectDayKey(event.StartAt), BaselineSeconds: float64(baseline)}
	contributionMust(t, storage.DB.Save(&p).Error)
	var amount atomic.Int64
	amount.Store(baseline + 600)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]int64{SectDayKey(event.StartAt): amount.Load()}})
	})
	if _, _, err := refreshWorldBossLiveDamageLocked(event); err != nil {
		t.Fatal(err)
	}
	contributionMust(t, storage.DB.First(&p, p.ID).Error)
	if math.Abs(p.DeltaHours-600.0/3600) > 1e-9 || p.IsRewarded || p.ListeningSnapshot.Seconds != 600 {
		t.Fatalf("live snapshot=%+v", p)
	}
	amount.Store(baseline + 900)
	cutoff := time.Now()
	event.Status = "settling"
	event.ListeningCutoffAt = &cutoff
	contributionMust(t, storage.DB.Save(&event).Error)
	hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
	if err != nil || !complete || math.Abs(hours-.25) > 1e-9 {
		t.Fatalf("final delta=%v complete=%v err=%v", hours, complete, err)
	}
}

func TestActivitySnapshotSpansMidnightWithoutImportingOtherHistory(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	event.StartAt = time.Now().Add(-24 * time.Hour)
	event.Status = "settling"
	p.CreatedAt = event.StartAt
	baseline := dailyListeningCreditSeconds(SectDayKey(event.StartAt), 7200, event.StartAt)
	priorDay := SectDayKey(event.StartAt)
	inputs, err := json.Marshal(map[string]activityDailyListeningInput{priorDay: {Seconds: baseline + 600}})
	contributionMust(t, err)
	p.ListeningSnapshot = model.ActivityListeningSnapshot{BaselineDay: priorDay, BaselineSeconds: baseline, DailyInputJSON: string(inputs)}
	contributionMust(t, storage.DB.Save(&event).Error)
	contributionMust(t, storage.DB.Save(&p).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("day") != SectDayKey(event.EndAt) {
			t.Errorf("unexpected listening date: %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 999999, "days": map[string]float64{
			priorDay: baseline + 1800, SectDayKey(event.EndAt): 300,
			SectDayKey(event.StartAt.AddDate(0, 0, -1)): 86400, SectDayKey(event.EndAt.AddDate(0, 0, 1)): 86400,
		}})
	})
	hours, complete, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
	if err != nil || !complete || math.Abs(hours-.25) > 1e-9 {
		t.Fatalf("cross-midnight snapshot=%v complete=%v err=%v", hours, complete, err)
	}
}

// A fractional elapsed-day baseline reproduces the release failure at any
// wall-clock time. Arithmetic over float64 seconds can leave sub-nanosecond
// rounding differences after subtracting the baseline and converting to hours.
func TestActivitySnapshotFractionalBaselineAcrossMidnight(t *testing.T) {
	start := time.Date(2026, time.November, 25, 0, 55, 18, 123456789, storage.DailyOperationsLocation)
	cutoff := start.Add(24 * time.Hour)
	snapshot := newActivityListeningSnapshot(map[string]float64{SectDayKey(start): 7200}, start)
	days := map[string]float64{
		SectDayKey(start):                   snapshot.BaselineSeconds + 600,
		SectDayKey(cutoff):                  300,
		SectDayKey(start.AddDate(0, 0, -1)): 86400,
		SectDayKey(cutoff.AddDate(0, 0, 1)): 86400,
	}
	seconds, err := advanceActivityListeningDays(days, cutoff, cutoff, start, 0, &snapshot)
	if err != nil || math.Abs(seconds-900) > 1e-9 {
		t.Fatalf("cross-midnight seconds=%v err=%v", seconds, err)
	}
	if math.Abs(seconds/3600-.25) > 1e-9 {
		t.Fatalf("cross-midnight hours=%v", seconds/3600)
	}
	if err := validateActivityFinalDailyInput(days, snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestBossClosingSnapshotBoundsConcurrencyAndPaysOnce(t *testing.T) {
	event := worldBossLiveFixture(t, 7)
	contributionMust(t, storage.DB.Model(&event).Update("end_at", time.Now().Add(-time.Second)).Error)
	var active, maximum, calls atomic.Int32
	started, release := make(chan struct{}, 7), make(chan struct{})
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 1800, "days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	done := make(chan struct{})
	go func() { defer close(done); settleWorldBoss(nil, event.BossID) }()
	var once sync.Once
	defer func() {
		once.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("closing workers did not finish")
		}
	}()
	for range listeningSyncWorkers - 1 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("closing snapshots were serialized")
		}
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing snapshots did not finish")
	}
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "settled" || calls.Load() != 7 || maximum.Load() != listeningSyncWorkers-1 {
		t.Fatalf("closing batch=%+v calls=%d max=%d", event, calls.Load(), maximum.Load())
	}
	var participants []model.WorldBossParticipant
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Find(&participants).Error)
	for _, p := range participants {
		if !p.ListeningSnapshot.Finalized || p.DeltaHours != .5 || !p.IsRewarded {
			t.Fatalf("closing progress lost: %+v", p)
		}
	}
	settleWorldBoss(nil, event.BossID)
	if calls.Load() != 7 {
		t.Fatal("settled event requested ABS again")
	}
}

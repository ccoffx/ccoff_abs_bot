package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestActivityFinalCapacityWaitDoesNotFreezeInput(t *testing.T) {
	event, participant := bossEvidenceFixture(t)
	event.Status = "settling"
	contributionMust(t, storage.DB.Save(&event).Error)
	participant.DeltaHours = .25
	contributionMust(t, storage.DB.Save(&participant).Error)
	oldGate := listeningSyncSlots
	gate := newListeningRequestGate(4, 8)
	listeningSyncSlots = gate
	t.Cleanup(func() { listeningSyncSlots = oldGate })
	for range 4 {
		gate.acquire(listeningRequestNormal, "", 0)
	}
	var calls atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	_, complete, err := worldBossListeningHours(&participant, event, participant.AbsUserID, time.Now())
	for range 4 {
		gate.release("")
	}
	if !errors.Is(err, ErrActivitySnapshotPending) || complete || participant.ListeningSnapshot.Finalized || calls.Load() != 0 || participant.ListeningSnapshot.FinalLastError != "capacity_timeout" {
		t.Fatalf("capacity wait froze the final input: %+v complete=%t err=%v calls=%d", participant, complete, err, calls.Load())
	}
	contributionMust(t, storage.DB.First(&participant, participant.ID).Error)
	participant.ListeningSnapshot.FinalNextRetryAt = nil
	contributionMust(t, storage.DB.Save(&participant).Error)
	hours, complete, err := worldBossListeningHours(&participant, event, participant.AbsUserID, time.Now())
	if err != nil || !complete || hours != .5 || calls.Load() != 1 || participant.ListeningSnapshot.FinalFallbackReason != "" {
		t.Fatalf("capacity recovery lost new progress: hours=%v complete=%t err=%v calls=%d", hours, complete, err, calls.Load())
	}
}

func TestActivitySettlementRetriesDurablePendingInputBeforePaying(t *testing.T) {
	for _, kind := range []string{"boss", "realm"} {
		for _, failure := range []string{"ABS", "capacity"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				var settle func()
				var table, eventID, eventColumn, eventTable string
				if kind == "boss" {
					event := worldBossLiveFixture(t, 1)
					contributionMust(t, storage.DB.Model(&event).Update("end_at", time.Now().Add(-time.Second)).Error)
					eventID, eventColumn, eventTable, table = event.BossID, "boss_id", "world_boss_events", "world_boss_participants"
					settle = func() { settleExpiredWorldBosses(nil, time.Now()) }
				} else {
					event := secretRealmSettlementFixture(t, 1)
					eventID, eventColumn, eventTable, table = event.RealmID, "realm_id", "sect_secret_realm_events", "sect_secret_realm_participants"
					settle = func() { settleExpiredSectSecretRealms(nil, time.Now()) }
				}
				var calls atomic.Int32
				var available atomic.Bool
				bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if !available.Load() {
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 1800}})
				})
				oldGate := listeningSyncSlots
				t.Cleanup(func() { listeningSyncSlots = oldGate })
				if failure == "capacity" {
					listeningSyncSlots = newListeningRequestGate(4, 0)
				}
				settle()
				var snapshot struct {
					ListeningSnapshot model.ActivityListeningSnapshot `gorm:"embedded;embeddedPrefix:listening_snapshot_"`
					IsRewarded        bool
				}
				contributionMust(t, storage.DB.Table(table).Where(eventColumn+" = ?", eventID).First(&snapshot).Error)
				var event struct{ Status string }
				contributionMust(t, storage.DB.Table(eventTable).Where(eventColumn+" = ?", eventID).First(&event).Error)
				if event.Status != "settling" || snapshot.IsRewarded || snapshot.ListeningSnapshot.Finalized || snapshot.ListeningSnapshot.FinalNextRetryAt == nil || snapshot.ListeningSnapshot.FinalAttempts != 1 {
					t.Fatalf("temporary failure froze or paid input: event=%+v participant=%+v", event, snapshot)
				}
				before := calls.Load()
				settle() // A new pass reads retry eligibility from the database, as after restart.
				if calls.Load() != before {
					t.Fatal("retry ignored durable backoff")
				}
				if failure == "capacity" && before != 0 {
					t.Fatal("capacity failure unexpectedly requested ABS")
				}
				listeningSyncSlots = oldGate
				available.Store(true)
				contributionMust(t, storage.DB.Table(table).Where(eventColumn+" = ?", eventID).Update("listening_snapshot_final_next_retry_at", nil).Error)
				settle()
				snapshot = struct {
					ListeningSnapshot model.ActivityListeningSnapshot `gorm:"embedded;embeddedPrefix:listening_snapshot_"`
					IsRewarded        bool
				}{}
				contributionMust(t, storage.DB.Table(table).Where(eventColumn+" = ?", eventID).First(&snapshot).Error)
				contributionMust(t, storage.DB.Table(eventTable).Where(eventColumn+" = ?", eventID).First(&event).Error)
				if event.Status != "settled" || !snapshot.IsRewarded || !snapshot.ListeningSnapshot.Finalized || snapshot.ListeningSnapshot.Seconds != 1800 || snapshot.ListeningSnapshot.FinalAttempts != 2 || snapshot.ListeningSnapshot.FinalFallbackReason != "" {
					t.Fatalf("recovery did not finish with fresh input: event=%+v participant=%+v", event, snapshot)
				}
				before = calls.Load()
				var user model.User
				contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 1).Error)
				points := user.Points
				settle()
				contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 1).Error)
				if calls.Load() != before || user.Points != points {
					t.Fatal("settled input was refetched or paid twice")
				}
			})
		}
	}
}

func TestBossAndRealmRefreshConcurrentlyWithoutMixingBaselines(t *testing.T) {
	for _, scenario := range []struct {
		phase    string
		crossDay bool
	}{{"live", false}, {"final", false}, {"live", true}, {"final", true}} {
		t.Run(fmt.Sprintf("%s/crossDay=%t", scenario.phase, scenario.crossDay), func(t *testing.T) {
			phase := scenario.phase
			realm := secretRealmLiveFixture(t, 7)
			// Entry must allow the full 600-second baseline even near 01:00,
			// when a one-hour-old fixture starts just after midnight.
			today := SectDayKey(time.Now())
			if scenario.crossDay || SectDayKey(realm.StartAt) != today || realm.StartAt.Sub(sectStartOfDay(realm.StartAt)) < 30*time.Minute {
				realm.StartAt = sectStartOfDay(time.Now()).Add(-30 * time.Minute)
				contributionMust(t, storage.DB.Save(&realm).Error)
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", realm.RealmID).Updates(map[string]any{
					"created_at":                      realm.StartAt,
					"listening_snapshot_baseline_day": SectDayKey(realm.StartAt),
				}).Error)
			}
			if phase == "final" {
				realm.EndAt = time.Now().Add(-time.Second)
				contributionMust(t, storage.DB.Model(&realm).Update("end_at", realm.EndAt).Error)
			}
			contributionMust(t, storage.DB.AutoMigrate(&model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.SectTechnology{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}))
			boss := model.WorldBossEvent{BossID: "parallel-boss", Status: "active", StartAt: realm.StartAt, EndAt: realm.EndAt}
			contributionMust(t, storage.DB.Create(&boss).Error)
			contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", realm.RealmID).Update("listening_snapshot_baseline_seconds", 600).Error)
			bossSnapshot := newActivityListeningSnapshot(nil, boss.StartAt)
			secondsToday := 1800
			if priorDay := SectDayKey(boss.StartAt); priorDay != today {
				input, err := json.Marshal(map[string]activityDailyListeningInput{priorDay: {Seconds: 1800}})
				contributionMust(t, err)
				bossSnapshot.DailyInputJSON = string(input)
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", realm.RealmID).
					Update("listening_snapshot_daily_input_json", string(input)).Error)
				secondsToday = 0
			}
			for id := 1; id <= 7; id++ {
				contributionMust(t, storage.DB.Create(&model.WorldBossParticipant{Model: gorm.Model{CreatedAt: boss.StartAt}, BossID: boss.BossID, UserID: int64(id), AbsUserID: fmt.Sprint("abs", id), ListeningSnapshot: bossSnapshot}).Error)
			}
			var active, maximum, calls atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
				}
				time.Sleep(20 * time.Millisecond)
				if r.URL.Query().Get("day") != today {
					t.Errorf("unexpected listening date: %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]int{today: secondsToday}})
			})
			results := make(chan error, 2)
			go func() {
				if phase == "final" {
					settleWorldBoss(nil, boss.BossID)
					results <- nil
					return
				}
				_, _, err := tryRefreshWorldBossLiveDamage(boss)
				results <- err
			}()
			go func() {
				if phase == "final" {
					settleSectSecretRealm(nil, realm.RealmID, 0)
					results <- nil
					return
				}
				_, _, err := tryRefreshSectSecretRealmLiveProgress(realm)
				results <- err
			}()
			for range 2 {
				select {
				case err := <-results:
					contributionMust(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("combined refresh blocked")
				}
			}
			if calls.Load() != 14 || maximum.Load() > 4 || maximum.Load() < 2 {
				t.Fatalf("requests=%d peak=%d", calls.Load(), maximum.Load())
			}
			var bp []model.WorldBossParticipant
			var rp []model.SectSecretRealmParticipant
			contributionMust(t, storage.DB.Where("boss_id = ?", boss.BossID).Find(&bp).Error)
			contributionMust(t, storage.DB.Where("realm_id = ?", realm.RealmID).Find(&rp).Error)
			for _, p := range bp {
				if p.DeltaHours != .5 || p.IsRewarded != (phase == "final") || p.ListeningSnapshot.Finalized != (phase == "final") {
					t.Fatalf("incorrect Boss input: %+v", p)
				}
			}
			for _, p := range rp {
				if p.RawDeltaSeconds != 1200 || p.IsRewarded != (phase == "final") || p.ListeningSnapshot.Finalized != (phase == "final") {
					t.Fatalf("incorrect realm input: %+v", p)
				}
			}
			if phase == "final" {
				settleWorldBoss(nil, boss.BossID)
				settleSectSecretRealm(nil, realm.RealmID, 0)
				if calls.Load() != 14 {
					t.Fatal("settled activities refetched input")
				}
			}
		})
	}
}

func TestSlowRealmDoesNotBlockAnotherClosingRealm(t *testing.T) {
	first := secretRealmSettlementFixture(t, 2)
	pool, err := storage.DB.DB()
	contributionMust(t, err)
	pool.SetMaxOpenConns(1)
	second := first
	second.Model, second.RealmID = gorm.Model{}, "second-realm"
	contributionMust(t, storage.DB.Create(&second).Error)
	var participant model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.Where("realm_id = ? AND user_id = ?", first.RealmID, 2).First(&participant).Error)
	contributionMust(t, storage.DB.Model(&participant).Update("realm_id", second.RealmID).Error)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/abs1/") {
			once.Do(func() { close(started) })
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	done := make(chan struct{})
	go func() { defer close(done); settleExpiredSectSecretRealms(nil, time.Now()) }()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("settlements did not drain")
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first realm did not begin")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		contributionMust(t, storage.DB.First(&second, second.ID).Error)
		if second.Status == "settled" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("second realm waited for the slow first realm")
}

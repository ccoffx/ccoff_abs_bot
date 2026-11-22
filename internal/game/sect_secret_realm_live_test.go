package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func secretRealmLiveFixture(t *testing.T, users int) model.SectSecretRealmEvent {
	t.Helper()
	event := secretRealmSettlementFixture(t, users)
	pool, err := storage.DB.DB()
	contributionMust(t, err)
	pool.SetMaxOpenConns(1)
	event.StartAt, event.EndAt = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	contributionMust(t, storage.DB.Save(&event).Error)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Updates(map[string]any{"created_at": event.StartAt, "listening_snapshot_baseline_day": SectDayKey(event.StartAt)}).Error)
	for id := int64(1); id <= int64(users); id++ {
		_, err := recordOfficialDailyListeningStats(id, fmt.Sprint("abs", id), map[string]float64{SectDayKey(time.Now()): 3600}, time.Now())
		contributionMust(t, err)
	}
	return event
}

func secretRealmQueryBot(t *testing.T) (*tgbotapi.BotAPI, func() string) {
	t.Helper()
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.Form.Get("text"))
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	contributionMust(t, err)
	return bot, func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(texts, "\n") }
}

func startSecretRealmLiveScheduler(t *testing.T) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshActiveSectSecretRealms(nil, time.Now())
	}()
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("background scheduler did not finish")
		}
	}
}

func waitSecretRealmLiveRefresh(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { sectSecretRealmLiveRefreshMu.Lock(); sectSecretRealmLiveRefreshMu.Unlock(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh did not finish")
	}
}

func TestSecretRealmQueriesOnlyReadSavedData(t *testing.T) {
	for _, age := range []string{"fresh", "expired", "never refreshed"} {
		t.Run(age, func(t *testing.T) {
			event := secretRealmLiveFixture(t, 1)
			at := time.Now()
			if age != "never refreshed" {
				if age == "expired" {
					at = at.Add(-3 * time.Minute)
				}
				contributionMust(t, storage.DB.Model(&event).Update("live_refreshed_at", at).Error)
			}
			contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("last_fetched_at", time.Now().Add(-11*time.Minute)).Error)
			contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Updates(map[string]any{"delta_hours": .75, "reward_points": 4}).Error)
			other := model.SectSecretRealmEvent{RealmID: "other", SectID: 999, Status: "active", StartAt: at.Add(-2 * time.Hour), EndAt: at.Add(-time.Hour), ProfileKey: event.ProfileKey, ConfigSnapshot: event.ConfigSnapshot}
			contributionMust(t, storage.DB.Create(&other).Error)
			var requests, writes atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
			contributionMust(t, storage.DB.Callback().Update().Before("gorm:update").Register("track_query_writes", func(tx *gorm.DB) {
				switch tx.Statement.Table {
				case "sect_secret_realm_events", "sect_secret_realm_participants", "cultivations", "daily_listening_stats":
					writes.Add(1)
				}
			}))
			bot, replies := secretRealmQueryBot(t)
			msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
			for range 2 {
				for _, command := range []string{"宗门秘境排行", "宗门秘境", "宗门秘境明细"} {
					if !HandleSectSecretRealmCommand(bot, msg, command) {
						t.Fatal("command not handled")
					}
				}
			}
			waitSecretRealmLiveRefresh(t)
			if requests.Load() != 0 || writes.Load() != 0 {
				t.Fatalf("%s queries caused ABS=%d writes=%d", age, requests.Load(), writes.Load())
			}
			contributionMust(t, storage.DB.First(&other, other.ID).Error)
			if other.Status != "active" {
				t.Fatal("query settled an unrelated realm")
			}
			want := "最近刷新"
			if age == "never refreshed" {
				want = "首次刷新尚未完成"
			}
			if strings.Count(replies(), want) != 6 || !strings.Contains(replies(), "最终秘境净修为：`0.75`") {
				t.Fatalf("queries did not show the same saved data and freshness: %s", replies())
			}
		})
	}
}

func TestSecretRealmQueriesDoNotWaitForScheduledRefresh(t *testing.T) {
	secretRealmLiveFixture(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		once.Do(func() { close(started) })
		<-release
		fmt.Fprint(w, `{"days":{}}`)
	})
	// Start without Telegram board delivery, which is tested independently.
	waitScheduler := startSecretRealmLiveScheduler(t)
	defer func() { close(release); waitScheduler() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh never started")
	}
	bot, replies := secretRealmQueryBot(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
		for range 5 {
			HandleSectSecretRealmCommand(bot, msg, "宗门秘境排行")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("query waited for ABS")
	}
	if requests.Load() != 1 {
		t.Fatalf("queries duplicated refreshes: %d", requests.Load())
	}
	if !strings.Contains(replies(), "首次刷新尚未完成") {
		t.Fatalf("uninitialized board claimed fresh data: %s", replies())
	}
}

func TestSecretRealmLiveRefreshUsesOneStatisticsRequestAndBoundsConcurrency(t *testing.T) {
	event := secretRealmLiveFixture(t, 7)
	var stats, sessions, active, maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 7)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "listening-stats") {
			stats.Add(1)
			w.WriteHeader(503)
			return
		}
		sessions.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")[0]
		session := bossSession("inside-"+id, event.StartAt.Add(10*time.Minute), event.StartAt.Add(40*time.Minute), 1800)
		session["userId"] = id
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	waitScheduler := startSecretRealmLiveScheduler(t)
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }); waitScheduler() }()
	for range listeningSyncWorkers - 1 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("participant requests did not run concurrently")
		}
	}
	releaseOnce.Do(func() { close(release) })
	waitScheduler()
	if stats.Load() != 0 || sessions.Load() != 7 || maximum.Load() != listeningSyncWorkers-1 {
		t.Fatalf("stats=%d sessions=%d max concurrency=%d", stats.Load(), sessions.Load(), maximum.Load())
	}
	var saved model.SectSecretRealmEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	if saved.LiveRefreshedAt == nil || saved.LiveRefreshIncomplete {
		t.Fatalf("successful refresh not recorded: %+v", saved)
	}
	var participants []model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.Where("realm_id = ?", event.RealmID).Find(&participants).Error)
	for _, p := range participants {
		if p.RawDeltaSeconds != 1800 || p.DeltaHours <= 0 || p.IsRewarded {
			t.Fatalf("lost evidence or paid a live reward: %+v", p)
		}
	}
	// A second scheduler pass skips fresh data, even with an old event copy.
	_, ran, err := tryRefreshSectSecretRealmLiveProgress(event)
	if ran || err != nil || sessions.Load() != 7 {
		t.Fatalf("stale event triggered duplicate refresh: ran=%t err=%v requests=%d", ran, err, sessions.Load())
	}
	refreshActiveSectSecretRealms(nil, time.Now())
	if sessions.Load() != 7 {
		t.Fatal("scheduler refreshed before the two-minute interval elapsed")
	}
	// A recent board/message write does not hide expired progress data.
	old := time.Now().Add(-3 * time.Minute)
	contributionMust(t, storage.DB.Model(&saved).Update("live_refreshed_at", old).Error)
	refreshActiveSectSecretRealms(nil, time.Now())
	if sessions.Load() != 14 {
		t.Fatalf("scheduler did not refresh expired data: requests=%d", sessions.Load())
	}
}

func TestSecretRealmLiveFailureIsVisibleAndExpiredQueryDoesNotFetch(t *testing.T) {
	event := secretRealmLiveFixture(t, 1)
	contributionMust(t, storage.DB.Model(&event).Updates(map[string]any{"total_delta_hours": .75, "total_reward_points": 4}).Error)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Updates(map[string]any{"delta_hours": .75, "reward_points": 4}).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	refreshed, ran, err := tryRefreshSectSecretRealmLiveProgress(event)
	if !ran || err != nil || !refreshed.LiveRefreshIncomplete || !strings.Contains(sectSecretRealmLiveRefreshNotice(refreshed), "其余榜单已更新") {
		t.Fatalf("partial failure hidden: ran=%t event=%+v err=%v", ran, refreshed, err)
	}
	var saved model.SectSecretRealmEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	var participant model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.First(&participant, "realm_id = ?", event.RealmID).Error)
	if saved.TotalDeltaHours != .75 || saved.TotalRewardPoints != 4 || participant.DeltaHours != .75 || participant.RewardPoints != 4 {
		t.Fatal("failed refresh erased the previous saved results")
	}
	// Final payout must still fetch ABS and fail safely even with a fresh daily cache.
	event.EndAt = time.Now().Add(-time.Second)
	contributionMust(t, storage.DB.Model(&event).Update("end_at", event.EndAt).Error)
	before := requests.Load()
	bot, replies := secretRealmQueryBot(t)
	HandleSectSecretRealmCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, "宗门秘境排行")
	waitSecretRealmLiveRefresh(t)
	if requests.Load() != before || !strings.Contains(replies(), "结算中") {
		t.Fatal("expired query waited for final settlement")
	}
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if requests.Load() <= before || event.Status != "settling" || event.SettledAt != nil {
		t.Fatal("final payout trusted the daily cache")
	}
}

func TestSecretRealmLiveRefreshUpdatesExpiredDailyCache(t *testing.T) {
	event := secretRealmLiveFixture(t, 1)
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("last_fetched_at", time.Now().Add(-11*time.Minute)).Error)
	var stats, sessions atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "listening-stats") {
			stats.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 3600}})
			return
		}
		sessions.Add(1)
		fmt.Fprint(w, `{"days":{}}`)
	})
	refreshed, ran, err := tryRefreshSectSecretRealmLiveProgress(event)
	if !ran || err != nil || refreshed.LiveRefreshIncomplete || stats.Load() != 1 || sessions.Load() != 0 {
		t.Fatalf("expired cache did not refresh once: ran=%t err=%v incomplete=%t stats=%d sessions=%d", ran, err, refreshed.LiveRefreshIncomplete, stats.Load(), sessions.Load())
	}
	if state := CachedListeningFreshness(1); state.Fresh {
		t.Fatal("activity refresh unexpectedly published a daily cultivation refresh")
	}
}

func TestSecretRealmBackgroundEvidenceCannotOverwriteFinalScan(t *testing.T) {
	event := secretRealmLiveFixture(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"days":{}}`)
	})
	waitScheduler := startSecretRealmLiveScheduler(t)
	var once sync.Once
	defer func() { once.Do(func() { close(release) }); waitScheduler() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh never started")
	}
	contributionMust(t, storage.DB.Model(&event).Update("status", "settling").Error)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Updates(map[string]any{
		"listening_evidence_json": `{"final":900}`, "listening_scan_page": 9, "final_listening_scan_started": true, "final_listening_scan_complete": true,
	}).Error)
	once.Do(func() { close(release) })
	waitScheduler()
	var p model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.First(&p, "realm_id = ?", event.RealmID).Error)
	if p.ListeningEvidenceJSON != `{"final":900}` || p.ListeningScanPage != 9 || !p.FinalListeningScanComplete {
		t.Fatalf("late background response overwrote settlement evidence: %+v", p)
	}
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.LiveRefreshedAt != nil {
		t.Fatal("late background response published a new live timestamp")
	}
}

func TestSecretRealmLivePreviewPublicationRollsBackTogether(t *testing.T) {
	event := secretRealmLiveFixture(t, 2)
	contributionMust(t, storage.DB.Model(&event).Updates(map[string]any{"total_delta_hours": 1.5, "total_reward_points": 8}).Error)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Updates(map[string]any{"delta_hours": .75, "reward_points": 4}).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"days":{}}`) })
	var writes atomic.Int32
	contributionMust(t, storage.DB.Callback().Update().Before("gorm:update").Register("fail_second_preview_write", func(tx *gorm.DB) {
		if tx.Statement.Table != "sect_secret_realm_participants" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		if _, preview := values["delta_hours"]; preview && writes.Add(1) == 2 {
			tx.AddError(errors.New("simulated preview write failure"))
		}
	}))
	_, ran, err := tryRefreshSectSecretRealmLiveProgress(event)
	if !ran || err == nil || writes.Load() != 2 {
		t.Fatalf("publication failure not injected: ran=%t writes=%d err=%v", ran, writes.Load(), err)
	}
	var saved model.SectSecretRealmEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	var participants []model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.Where("realm_id = ?", event.RealmID).Find(&participants).Error)
	if saved.TotalDeltaHours != 1.5 || saved.TotalRewardPoints != 8 || saved.LiveRefreshedAt != nil {
		t.Fatalf("failed round published its summary: %+v", saved)
	}
	for _, p := range participants {
		if p.DeltaHours != .75 || p.RewardPoints != 4 {
			t.Fatalf("failed round partially published a participant: %+v", p)
		}
	}
}

func TestSecretRealmPartialRefreshPublishesHealthyParticipants(t *testing.T) {
	event := secretRealmLiveFixture(t, 2)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = ?", event.RealmID, 1).Updates(map[string]any{"delta_hours": .25, "raw_delta_seconds": 900, "reward_points": 4}).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "abs1") {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"days":{"%s":1800}}`, SectDayKey(time.Now()))
	})
	refreshed, ran, err := tryRefreshSectSecretRealmLiveProgress(event)
	if !ran || err != nil || !refreshed.LiveRefreshIncomplete {
		t.Fatalf("partial round failed: %+v %t %v", refreshed, ran, err)
	}
	var participants []model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.Where("realm_id = ?", event.RealmID).Order("user_id").Find(&participants).Error)
	failed, healthy := participants[0], participants[1]
	if failed.RawDeltaSeconds != 900 || failed.DeltaHours != .25 || failed.RewardPoints != 4 || healthy.RawDeltaSeconds != 1800 || healthy.DeltaHours <= 0 {
		t.Fatalf("incorrect partial progress: %+v", participants)
	}
	if refreshed.TotalDeltaHours != failed.DeltaHours+healthy.DeltaHours || refreshed.TotalRewardPoints != failed.RewardPoints+healthy.RewardPoints || failed.IsRewarded || healthy.IsRewarded {
		t.Fatalf("incorrect totals or premature payout: %+v", refreshed)
	}
}

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

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func worldBossLiveFixture(t *testing.T, users int) model.WorldBossEvent {
	t.Helper()
	event, _ := bossEvidenceFixture(t)
	old := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = old })
	pool, err := storage.DB.DB()
	contributionMust(t, err)
	pool.SetMaxOpenConns(1)
	event.Name, event.StartAt, event.EndAt = "测试Boss", time.Now().Add(-40*time.Minute), time.Now().Add(20*time.Minute)
	event.MaxHP, event.CurrentHP, event.ParticipantCount = calculateWorldBossMaxHP(users), float64(calculateWorldBossMaxHP(users)), users
	contributionMust(t, storage.DB.Save(&event).Error)
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Updates(map[string]any{"created_at": event.StartAt, "listening_snapshot_baseline_day": SectDayKey(event.StartAt)}).Error)
	contributionMust(t, storage.DB.Exec("CREATE UNIQUE INDEX ux_test_boss_participant ON world_boss_participants(boss_id, user_id) WHERE deleted_at IS NULL").Error)
	expires := time.Now().Add(time.Hour)
	for id := 1; id <= users; id++ {
		contributionMust(t, storage.DB.Create(&model.User{TelegramID: int64(id), Username: fmt.Sprint("boss", id), AbsUserID: fmt.Sprint("abs", id), Status: "active", ExpireAt: &expires}).Error)
		if id > 1 {
			contributionMust(t, storage.DB.Create(&model.WorldBossParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, BossID: event.BossID, UserID: int64(id), AbsUserID: fmt.Sprint("abs", id), ListeningSnapshot: newActivityListeningSnapshot(nil, event.StartAt)}).Error)
		}
	}
	return event
}

func TestWorldBossQueriesOnlyReadSavedDataWithoutWaitingForRefreshLock(t *testing.T) {
	for _, state := range []string{"fresh", "expired", "never refreshed", "ended", "killed", "settling", "settled"} {
		t.Run(state, func(t *testing.T) {
			event := worldBossLiveFixture(t, 1)
			updates := map[string]any{"current_hp": 108}
			at := time.Now()
			if state != "never refreshed" {
				if state == "expired" {
					at = at.Add(-3 * time.Minute)
				}
				updates["live_refreshed_at"] = at
			}
			switch state {
			case "ended":
				updates["end_at"] = time.Now().Add(-time.Minute)
			case "killed":
				updates["is_killed"] = true
			case "settling", "settled":
				updates["status"] = state
			}
			contributionMust(t, storage.DB.Model(&event).Updates(updates).Error)
			contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Updates(map[string]any{"base_damage": 10, "multiplier": 1.2, "damage": 12, "delta_hours": .4}).Error)
			var requests, writes atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
			contributionMust(t, storage.DB.Callback().Update().Before("gorm:update").Register("track_boss_query_writes", func(tx *gorm.DB) {
				switch tx.Statement.Table {
				case "world_boss_events", "world_boss_participants", "cultivations":
					writes.Add(1)
				}
			}))
			bot, replies := secretRealmQueryBot(t)
			msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
			worldBossLiveRefreshMu.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range 2 {
					for _, command := range []string{"世界Boss", "Boss状态", "Boss排行", "Boss明细"} {
						HandleWorldBossCommand(bot, msg, command)
					}
				}
			}()
			select {
			case <-done:
				worldBossLiveRefreshMu.Unlock()
			case <-time.After(2 * time.Second):
				worldBossLiveRefreshMu.Unlock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
				t.Fatal("query waited for the background refresh or settlement lock")
			}
			if requests.Load() != 0 || writes.Load() != 0 {
				t.Fatalf("query caused ABS=%d writes=%d", requests.Load(), writes.Load())
			}
			reply := replies()
			if strings.Count(reply, "伤害：`12.00`") != 4 || strings.Contains(reply, "查询太频繁") {
				t.Fatalf("saved board was not returned on every query: %s", reply)
			}
			if state != "settled" && !strings.Contains(reply, "已保存") {
				t.Fatalf("missing saved-data notice: %s", reply)
			}
		})
	}
}

func TestWorldBossSchedulerBoundsConcurrencyAndRefreshesWithoutQueries(t *testing.T) {
	event := worldBossLiveFixture(t, 7)
	var active, maximum, requests atomic.Int32
	started, release := make(chan struct{}, 7), make(chan struct{})
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
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
		session := bossSession("inside-"+id, event.StartAt.Add(5*time.Minute), event.StartAt.Add(35*time.Minute), 1800)
		session["userId"] = id
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 1800, "days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	done := make(chan struct{})
	go func() { defer close(done); refreshActiveWorldBosses(nil, time.Now()) }()
	var once sync.Once
	wait := func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler did not finish")
		}
	}
	defer func() { once.Do(func() { close(release) }); wait() }()
	for range listeningSyncWorkers - 1 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("sessions were not fetched concurrently")
		}
	}
	// Another refresh or payout must skip instead of joining a waiting queue.
	if _, ran, err := tryRefreshWorldBossLiveDamage(event); ran || err != nil {
		t.Fatalf("overlapping refresh ran: %t %v", ran, err)
	}
	settleWorldBoss(nil, event.BossID)
	once.Do(func() { close(release) })
	wait()
	if maximum.Load() != listeningSyncWorkers-1 || requests.Load() != 7 {
		t.Fatalf("maximum=%d requests=%d", maximum.Load(), requests.Load())
	}
	var saved model.WorldBossEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	if saved.LiveRefreshedAt == nil || saved.LiveRefreshIncomplete || saved.CurrentHP != 79 || saved.Status != "active" {
		t.Fatalf("wrong saved progress: %+v", saved)
	}
	var participants []model.WorldBossParticipant
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Find(&participants).Error)
	for _, p := range participants {
		if p.DeltaHours != .5 || p.Damage != 15 || p.IsRewarded {
			t.Fatalf("wrong live damage or premature payout: %+v", p)
		}
	}
	refreshActiveWorldBosses(nil, time.Now())
	if requests.Load() != 7 {
		t.Fatal("scheduler repeated a fresh round")
	}
	contributionMust(t, storage.DB.Model(&saved).Update("live_refreshed_at", time.Now().Add(-3*time.Minute)).Error)
	refreshActiveWorldBosses(nil, time.Now())
	if requests.Load() != 14 {
		t.Fatal("board UpdatedAt hid expired damage data")
	}
}

func TestWorldBossFailedRefreshRetainsSavedDamageAndHP(t *testing.T) {
	event := worldBossLiveFixture(t, 2)
	contributionMust(t, storage.DB.Model(&event).Update("current_hp", 96).Error)
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Updates(map[string]any{"damage": 12, "delta_hours": .4, "base_damage": 12, "multiplier": 1}).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "abs1") {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"days":{"%s":1800}}`, SectDayKey(time.Now()))
	})
	refreshed, ran, err := tryRefreshWorldBossLiveDamage(event)
	if !ran || err != nil || !refreshed.LiveRefreshIncomplete || !strings.Contains(worldBossLiveRefreshNotice(refreshed), "其余战况已更新") {
		t.Fatalf("failure hidden: ran=%t err=%v event=%+v", ran, err, refreshed)
	}
	var saved model.WorldBossEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	var participants []model.WorldBossParticipant
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Find(&participants).Error)
	if saved.CurrentHP != float64(saved.MaxHP)-27 {
		t.Fatalf("partial refresh did not combine saved and fresh damage: %+v", saved)
	}
	for _, p := range participants {
		expectedDamage, expectedHours := 12.0, .4
		if p.UserID != 1 {
			expectedDamage, expectedHours = 15, .5
		}
		if p.Damage != expectedDamage || p.DeltaHours != expectedHours || p.IsRewarded {
			t.Fatalf("incorrect partial progress: %+v", p)
		}
	}
	// Even with the same failed account, saved + healthy damage can kill the Boss.
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", event.BossID, 1).Update("damage", saved.MaxHP).Error)
	contributionMust(t, storage.DB.Model(&saved).Update("live_refreshed_at", time.Now().Add(-3*time.Minute)).Error)
	killed, ran, err := tryRefreshWorldBossLiveDamage(event)
	if !ran || err != nil || !killed.IsKilled || killed.CurrentHP != 0 || !killed.LiveRefreshIncomplete {
		t.Fatalf("partial refresh did not recognize kill: %+v %t %v", killed, ran, err)
	}

}

func TestWorldBossPreviewWriteFailureRollsBackWholeRound(t *testing.T) {
	event := worldBossLiveFixture(t, 2)
	contributionMust(t, storage.DB.Model(&event).Update("current_hp", 96).Error)
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Update("damage", 12).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"totalTime":0,"days":{}}`) })
	var writes atomic.Int32
	contributionMust(t, storage.DB.Callback().Update().Before("gorm:update").Register("fail_boss_preview", func(tx *gorm.DB) {
		if tx.Statement.Table != "world_boss_participants" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		if _, preview := values["damage"]; preview && writes.Add(1) == 2 {
			tx.AddError(errors.New("simulated Boss preview write failure"))
		}
	}))
	_, ran, err := tryRefreshWorldBossLiveDamage(event)
	if !ran || err == nil || writes.Load() != 2 {
		t.Fatalf("failure not injected: %t %v writes=%d", ran, err, writes.Load())
	}
	var saved model.WorldBossEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	var participants []model.WorldBossParticipant
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Find(&participants).Error)
	if saved.CurrentHP != 96 || saved.LiveRefreshedAt != nil {
		t.Fatalf("failed round published HP: %+v", saved)
	}
	for _, p := range participants {
		if p.Damage != 12 {
			t.Fatalf("failed round partially wrote damage: %+v", p)
		}
	}
}

func TestWorldBossSchedulerRetriesEarlyKillWithoutWaitingForRefreshTTL(t *testing.T) {
	event := worldBossLiveFixture(t, 1)
	contributionMust(t, storage.DB.Model(&event).Updates(map[string]any{"is_killed": true, "live_refreshed_at": time.Now()}).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	refreshActiveWorldBosses(nil, time.Now())
	var saved model.WorldBossEvent
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	if saved.ListeningCutoffAt == nil || saved.Status != "settling" || requests.Load() != 1 {
		t.Fatalf("early-kill payout did not run: %+v requests=%d", saved, requests.Load())
	}
	cutoff := *saved.ListeningCutoffAt
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Update("listening_snapshot_final_next_retry_at", nil).Error)
	settleExpiredWorldBosses(nil, time.Now())
	contributionMust(t, storage.DB.First(&saved, event.ID).Error)
	if saved.Status != "settled" || requests.Load() != 2 || saved.ListeningCutoffAt == nil || !saved.ListeningCutoffAt.Equal(cutoff) {
		t.Fatal("settled event retried or moved the cutoff")
	}
}

func TestWorldBossRepeatedJoinDoesNotFetchABSOrWaitForRefresh(t *testing.T) {
	worldBossLiveFixture(t, 1)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	bot, replies := secretRealmQueryBot(t)
	HandleWorldBossCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, "参加Boss")
	waitActivityEntries(t)
	if requests.Load() != 0 || !strings.Contains(replies(), "你当前已经加入过本期世界Boss讨伐") {
		t.Fatalf("repeat join made a remote call or lost eligibility: requests=%d %s", requests.Load(), replies())
	}
}

func TestWorldBossFirstJoinOnlyFetchesOwnBaselineOnce(t *testing.T) {
	event := worldBossLiveFixture(t, 1)
	expires := time.Now().Add(time.Hour)
	contributionMust(t, storage.DB.Create(&model.User{TelegramID: 2, Username: "new-boss-user", AbsUserID: "abs2", Status: "active", ExpireAt: &expires}).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/abs2/listening-stats") {
			t.Errorf("join fetched another account or a session page: %s", r.URL.Path)
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"totalTime":3600,"days":{}}`)
	})
	bot, replies := secretRealmQueryBot(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}
	for range 2 {
		HandleWorldBossCommand(bot, msg, "参加Boss")
		waitActivityEntries(t)
	}
	var p model.WorldBossParticipant
	contributionMust(t, storage.DB.First(&p, "boss_id = ? AND user_id = ?", event.BossID, 2).Error)
	var count int64
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", event.BossID, 2).Count(&count).Error)
	if requests.Load() != 1 || count != 1 || p.ListeningSnapshot.BaselineSeconds != 0 || strings.Count(replies(), "已加入世界Boss讨伐") != 1 || strings.Count(replies(), "你当前已经加入过本期世界Boss讨伐") != 1 {
		t.Fatalf("join duplicated requests/entries or changed the baseline: requests=%d count=%d p=%+v replies=%s", requests.Load(), count, p, replies())
	}
}

func TestWorldBossLiveRefreshExcludesChangedBinding(t *testing.T) {
	event := worldBossLiveFixture(t, 2)
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "replacement").Error)
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Update("damage", 40).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, "abs2") {
			t.Errorf("queried an excluded binding: %s", r.URL.Path)
		}
		session := bossSession("valid", event.StartAt, event.StartAt.Add(30*time.Minute), 1800)
		session["userId"] = "abs2"
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 1800, "days": map[string]float64{SectDayKey(time.Now()): 1800}})
	})
	refreshed, ran, err := tryRefreshWorldBossLiveDamage(event)
	if !ran || err != nil || refreshed.LiveRefreshIncomplete || refreshed.CurrentHP != 109 || requests.Load() != 1 {
		t.Fatalf("excluded account changed live HP: ran=%t err=%v event=%+v requests=%d", ran, err, refreshed, requests.Load())
	}
	var excluded model.WorldBossParticipant
	contributionMust(t, storage.DB.First(&excluded, "boss_id = ? AND user_id = ?", event.BossID, 1).Error)
	if excluded.Damage != 0 {
		t.Fatal("excluded participant retained live damage")
	}
}

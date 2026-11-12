package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func bossEvidenceFixture(t *testing.T) (model.WorldBossEvent, model.WorldBossParticipant) {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	event := model.WorldBossEvent{BossID: "test-boss", Status: "active", StartAt: end.Add(-time.Hour), EndAt: end}
	p := model.WorldBossParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, BossID: event.BossID, UserID: 1, AbsUserID: "abs1"}
	if err := storage.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return event, p
}

func bossSession(id string, start, end time.Time, seconds float64) map[string]interface{} {
	return map[string]interface{}{"id": id, "userId": "abs1", "startedAt": start.Format(time.RFC3339Nano), "updatedAt": end.Format(time.RFC3339Nano), "timeListening": seconds}
}

func bossEvidenceServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	old := abs.DefaultClient
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, Token: "test", HttpClient: server.Client()}
	t.Cleanup(func() { abs.DefaultClient = old; server.Close() })
}

func TestWorldBossCountsOnlyProvenPlaybackInsideParticipationWindow(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		startOffset, endOffset time.Duration
		seconds, want          float64
		lateJoin               bool
	}{
		{"after event", time.Hour, 2 * time.Hour, 3600, 0, false},
		{"before event", -2 * time.Hour, -time.Hour, 3600, 0, false},
		{"inside event", 0, 30 * time.Minute, 1800, 0.5, false},
		{"small client clock skew", 0, 30 * time.Minute, 1819, 0.5, false},
		{"cross start", -30 * time.Minute, 30 * time.Minute, 3600, 0.5, false},
		{"ambiguous pauses", -time.Hour, 2 * time.Hour, 3600, 0, false},
		{"invalid duration", 0, 30 * time.Minute, 3600, 0, false},
		{"joined halfway", 0, time.Hour, 3600, 0.5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, p := bossEvidenceFixture(t)
			if tc.lateJoin {
				p.CreatedAt = event.StartAt.Add(30 * time.Minute)
			}
			session := bossSession("session", event.StartAt.Add(tc.startOffset), event.StartAt.Add(tc.endOffset), tc.seconds)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"totalTime": 9999999, "sessions": []any{session, session}})
			})
			hours, complete, err := worldBossListeningHours(&p, event, "abs1", time.Now())
			if err != nil || !complete || hours != tc.want {
				t.Fatalf("got %.4f complete=%t error=%v; want %.4f", hours, complete, err, tc.want)
			}
		})
	}
}

func TestWorldBossHistoricalScanResumesAfterFailureAndRestart(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	var fail atomic.Bool
	fail.Store(true)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 5 {
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []any{bossSession("old-in-window", event.StartAt, event.EndAt, 3600)}})
			return
		}
		if page > 5 {
			t.Errorf("unexpected page %d", page)
		}
		sessions := make([]any, dailyListeningSessionPageSize)
		for i := range sessions {
			sessions[i] = bossSession(fmt.Sprintf("outside-%d-%d", page, i), event.EndAt.Add(time.Hour), event.EndAt.Add(2*time.Hour), 3600)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
	})
	if hours, complete, err := worldBossListeningHours(&p, event, "abs1", time.Now()); err != nil || complete || hours != 0 {
		t.Fatalf("first scan: %.2f %t %v", hours, complete, err)
	}
	var restored model.WorldBossParticipant
	if err := storage.DB.First(&restored, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if restored.ListeningScanPage != 5 {
		t.Fatalf("cursor lost: %d", restored.ListeningScanPage)
	}
	if _, complete, err := worldBossListeningHours(&restored, event, "abs1", time.Now()); err == nil || complete {
		t.Fatal("failed history page was treated as complete")
	}
	fail.Store(false)
	if hours, complete, err := worldBossListeningHours(&restored, event, "abs1", time.Now()); err != nil || !complete || hours != 1 {
		t.Fatalf("resumed scan lost older session: %.2f %t %v", hours, complete, err)
	}
}

func TestWorldBossRetainsPreviouslyProvenSessionEvidence(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	var extended atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		end, seconds := event.StartAt.Add(30*time.Minute), 1800.0
		if extended.Load() {
			end = event.EndAt.Add(2 * time.Hour)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []any{bossSession("same-session", event.StartAt, end, seconds)}})
	})
	if hours, _, err := worldBossListeningHours(&p, event, "abs1", event.StartAt.Add(30*time.Minute)); err != nil || hours != 0.5 {
		t.Fatalf("live evidence: %v %.2f", err, hours)
	}
	extended.Store(true)
	if hours, complete, err := worldBossListeningHours(&p, event, "abs1", time.Now()); err != nil || !complete || hours != 0.5 {
		t.Fatalf("later snapshot erased known playback: %v %.2f", err, hours)
	}
	if _, _, err := worldBossListeningHours(&p, event, "different-abs", time.Now()); err == nil {
		t.Fatal("accepted another account's playback")
	}
}

func TestWorldBossEarlyKillUsesFrozenCutoff(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	event.StartAt = time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	event.EndAt = event.StartAt.Add(time.Hour)
	cutoff := event.StartAt.Add(20 * time.Minute)
	event.ListeningCutoffAt = &cutoff
	p.CreatedAt = event.StartAt
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []any{bossSession("before-kill", event.StartAt, cutoff, 1200), bossSession("after-kill", cutoff, cutoff.Add(10*time.Minute), 600)}})
	})
	hours, complete, err := worldBossListeningHours(&p, event, "abs1", time.Now())
	if err != nil || !complete || hours != 1.0/3.0 {
		t.Fatalf("early kill did not finalize at cutoff: %.4f %t %v", hours, complete, err)
	}
	if canJoinWorldBossAt(event, time.Now()) {
		t.Fatal("settlement retry allowed late join after kill")
	}
}

func TestWorldBossFailedSettlementPreservesRewardsAndEarlyCutoff(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	now := time.Now()
	if err := storage.DB.Model(&event).Updates(map[string]interface{}{"start_at": now.Add(-30 * time.Minute), "end_at": now.Add(30 * time.Minute), "is_killed": true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 1, Username: "boss-user", AbsUserID: "abs1", Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	settleWorldBoss(nil, event.BossID)
	var failed model.WorldBossEvent
	if err := storage.DB.First(&failed, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "active" || failed.SettledAt != nil || failed.ListeningCutoffAt == nil || !failed.ListeningCutoffAt.Before(failed.EndAt) {
		t.Fatalf("failed settlement cannot safely retry: %+v", failed)
	}
	cutoff := *failed.ListeningCutoffAt
	settleWorldBoss(nil, event.BossID)
	if err := storage.DB.First(&failed, event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.ListeningCutoffAt == nil || !failed.ListeningCutoffAt.Equal(cutoff) {
		t.Fatal("retry advanced early kill cutoff")
	}
	if err := storage.DB.First(&p, p.ID).Error; err != nil || p.IsRewarded {
		t.Fatalf("API failure finalized participant: %+v %v", p, err)
	}
}

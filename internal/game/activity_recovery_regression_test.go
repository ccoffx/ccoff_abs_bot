package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestWorldBossKillPublicationPersistsCutoff(t *testing.T) {
	for _, source := range []string{"live", "stored"} {
		t.Run(source, func(t *testing.T) {
			event := worldBossLiveFixture(t, 20)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"days":{"%s":2400}}`, SectDayKey(event.StartAt))
			})
			startedAt := time.Now()
			if source == "live" {
				_, ran, err := tryRefreshWorldBossLiveDamage(event)
				contributionMust(t, err)
				if !ran {
					t.Fatal("live refresh did not run")
				}
			} else {
				contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Update("damage", 20).Error)
				_, err := refreshWorldBossStoredHPByParticipants(event.BossID)
				contributionMust(t, err)
			}
			contributionMust(t, storage.DB.First(&event, event.ID).Error)
			if !event.IsKilled || event.CurrentHP != 0 || event.ListeningCutoffAt == nil ||
				event.ListeningCutoffAt.Before(startedAt) || event.ListeningCutoffAt.After(time.Now()) {
				t.Fatalf("kill published without its cutoff: %+v", event)
			}
			cutoff := *event.ListeningCutoffAt
			_, err := refreshWorldBossStoredHPByParticipants(event.BossID)
			contributionMust(t, err)
			contributionMust(t, storage.DB.First(&event, event.ID).Error)
			if !event.ListeningCutoffAt.Equal(cutoff) {
				t.Fatal("later HP refresh moved the kill cutoff")
			}
		})
	}
}

func TestWorldBossKillRecoveryDoesNotReopenFinalWindow(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			event := worldBossLiveFixture(t, 1)
			killedAt := time.Now().Add(-10 * time.Minute)
			updates := map[string]any{"is_killed": true, "current_hp": 0, "live_refreshed_at": killedAt}
			if !legacy {
				updates["listening_cutoff_at"] = killedAt
			}
			contributionMust(t, storage.DB.Model(&event).Updates(updates).Error)
			contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).
				Updates(map[string]any{"delta_hours": .25, "damage": 7.5}).Error)
			var requests atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				fmt.Fprintf(w, `{"days":{"%s":1800}}`, SectDayKey(time.Now()))
			})
			// Recovery discovers only persisted state, without the pre-kill event.
			settleExpiredWorldBosses(nil, time.Now())
			contributionMust(t, storage.DB.First(&event, event.ID).Error)
			var participant model.WorldBossParticipant
			contributionMust(t, storage.DB.First(&participant, "boss_id = ?", event.BossID).Error)
			if event.Status != "settled" || event.ListeningCutoffAt == nil || !event.ListeningCutoffAt.Equal(killedAt) ||
				requests.Load() != 0 || !participant.ListeningSnapshot.Finalized || participant.DeltaHours != .25 ||
				participant.ListeningSnapshot.FinalFallbackReason == "" {
				t.Fatalf("recovery reopened the kill window: event=%+v participant=%+v requests=%d", event, participant, requests.Load())
			}
		})
	}
}

func TestActivityEntryNotificationRecoversAfterRepeatedFailures(t *testing.T) {
	for _, status := range []string{"joined", "failed"} {
		t.Run(status, func(t *testing.T) {
			eventID, userID, _, _ := entryTestFixture(t, activityEntryBoss)
			request := model.ActivityEntryRequest{Kind: activityEntryBoss, EventID: eventID, UserID: userID, AbsUserID: "abs2", Status: status,
				Generation: 1, ChatID: userID, ResultText: "入场结果"}
			contributionMust(t, storage.DB.Create(&request).Error)
			var sends atomic.Int32
			var healthy atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				sends.Add(1)
				if !healthy.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `{"ok":false,"error_code":503,"description":"temporarily unavailable"}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			contributionMust(t, err)
			for attempt := 1; attempt <= 4; attempt++ {
				// Read each retry from durable state, as after a process restart.
				startedAt := time.Now()
				dispatchActivityEntries(bot, startedAt)
				waitActivityEntries(t)
				contributionMust(t, storage.DB.First(&request, request.ID).Error)
				wantDelay := 30 * time.Second * time.Duration(1<<(attempt-1))
				if request.NotifiedAt != nil || request.NotificationAttempts != attempt || sends.Load() != int32(attempt) ||
					request.NextNotificationAt == nil || request.NextNotificationAt.Before(startedAt.Add(wantDelay)) {
					t.Fatalf("lost retry state: %+v sends=%d", request, sends.Load())
				}
				dispatchActivityEntries(bot, time.Now())
				waitActivityEntries(t)
				if sends.Load() != int32(attempt) {
					t.Fatal("dispatcher ignored persisted notification backoff")
				}
				contributionMust(t, storage.DB.Model(&request).Update("next_notification_at", time.Now().Add(-time.Second)).Error)
			}
			healthy.Store(true)
			dispatchActivityEntries(bot, time.Now())
			waitActivityEntries(t)
			contributionMust(t, storage.DB.First(&request, request.ID).Error)
			if request.NotifiedAt == nil || request.NotificationAttempts != 5 || sends.Load() != 5 {
				t.Fatalf("result did not recover after Telegram recovered: %+v sends=%d", request, sends.Load())
			}
			dispatchActivityEntries(bot, time.Now().Add(24*time.Hour))
			waitActivityEntries(t)
			if sends.Load() != 5 {
				t.Fatal("delivered result was sent again")
			}
		})
	}
}

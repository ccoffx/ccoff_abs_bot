package game

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func waitSectRetreatPreparations(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sectRetreatPreparations.WaitIdle(ctx); err != nil {
		t.Fatal("retreat preparation did not finish", err)
	}
}

func setupRetreatPreparation(t *testing.T, personal bool) (*tgbotapi.BotAPI, *tgbotapi.Message, func()) {
	t.Helper()
	bot := setupSectRegressionTest(t)
	now := time.Now()
	contributionMust(t, storage.DB.Model(&model.Sect{}).Where("id = 1").Updates(map[string]any{"level": 4, "prestige": 1000}).Error)
	contributionMust(t, storage.DB.Model(&model.SectMember{}).Where("user_id = 1").Updates(map[string]any{"role": SectRoleOwner, "cave_unlocked_at": now, "personal_prestige": 100}).Error)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "道友"}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	return bot, msg, func() {
		if personal {
			handleStartPersonalSectCaveRetreat(bot, msg, true)
		} else {
			handleStartSectCaveRetreat(bot, msg, true)
		}
	}
}

func TestRetreatPreparationDoesNotOccupyCommandOrChargeBeforeReady(t *testing.T) {
	for _, personal := range []bool{true, false} {
		t.Run(fmt.Sprint("personal=", personal), func(t *testing.T) {
			_, _, submit := setupRetreatPreparation(t, personal)
			started, release := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			var once sync.Once
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				once.Do(func() { close(started) })
				<-release
				fmt.Fprintf(w, `{"days":{"%s":1800}}`, SectDayKey(time.Now()))
			})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); waitSectRetreatPreparations(t) })
			done := make(chan struct{})
			go func() { defer close(done); submit() }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("command waited for ABS")
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("background preparation did not request baseline")
			}
			submit() // The in-flight key must merge repeat confirmations.
			var count int64
			contributionMust(t, storage.DB.Model(&model.SectCaveRetreat{}).Count(&count).Error)
			var member model.SectMember
			var sect model.Sect
			contributionMust(t, storage.DB.First(&member, "user_id = 1").Error)
			contributionMust(t, storage.DB.First(&sect, 1).Error)
			if count != 0 || member.PersonalPrestige != 100 || sect.Prestige != 1000 || requests.Load() != 1 {
				t.Fatal("preparation charged, started, or duplicated before ready")
			}
			releasedAt := time.Now()
			releaseOnce.Do(func() { close(release) })
			waitSectRetreatPreparations(t)
			var retreat model.SectCaveRetreat
			contributionMust(t, storage.DB.First(&retreat, "user_id = 1").Error)
			contributionMust(t, storage.DB.Model(&model.SectCaveRetreat{}).Count(&count).Error)
			contributionMust(t, storage.DB.First(&member, "user_id = 1").Error)
			contributionMust(t, storage.DB.First(&sect, 1).Error)
			if count != 1 || retreat.StartAt.Before(releasedAt) {
				t.Fatal("retreat duplicated or was backdated to queue time")
			}
			if personal {
				if member.PersonalPrestige != 100-sectCavePersonalRetreatCost || sect.Prestige != 1000 {
					t.Fatal("personal retreat charged wrong balance")
				}
			} else if sect.Prestige != 1000-sectCaveSectRetreatCost(4) || member.PersonalPrestige != 100 {
				t.Fatal("sect retreat charged wrong balance")
			}
		})
	}
}

func TestRetreatPreparationUsesSavedBaselineWithoutWaitingForFullBatch(t *testing.T) {
	for _, personal := range []bool{true, false} {
		t.Run(fmt.Sprint("personal=", personal), func(t *testing.T) {
			_, _, submit := setupRetreatPreparation(t, personal)
			contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(time.Now()), CappedSeconds: 14400, OfficialRawSeconds: 14400, LastFetchedAt: time.Now()}).Error)
			var requests atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
			t.Cleanup(func() { waitSectRetreatPreparations(t) })
			listeningRefreshMu.Lock()
			submit()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := sectRetreatPreparations.WaitIdle(ctx)
			cancel()
			listeningRefreshMu.Unlock()
			if err != nil {
				t.Fatal("retreat waited behind the full batch", err)
			}
			var retreat model.SectCaveRetreat
			contributionMust(t, storage.DB.First(&retreat, "user_id = 1").Error)
			if requests.Load() != 0 || retreat.BaseRawSeconds != 14400 {
				t.Fatalf("saved baseline was not reused: %+v requests=%d", retreat, requests.Load())
			}
		})
	}
}

func TestQueuedRetreatRechecksMembershipBeforePreparation(t *testing.T) {
	for _, personal := range []bool{true, false} {
		t.Run(fmt.Sprint("personal=", personal), func(t *testing.T) {
			_, _, submit := setupRetreatPreparation(t, personal)
			release := make(chan struct{})
			for _, key := range []string{"hold-a", "hold-b"} {
				sectRetreatPreparations.Submit(key, func() { <-release }, false)
			}
			var requests atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }); waitSectRetreatPreparations(t) })
			submit()
			contributionMust(t, storage.DB.Where("user_id = 1").Delete(&model.SectMember{}).Error)
			once.Do(func() { close(release) })
			waitSectRetreatPreparations(t)
			var count int64
			contributionMust(t, storage.DB.Model(&model.SectCaveRetreat{}).Count(&count).Error)
			var sect model.Sect
			contributionMust(t, storage.DB.First(&sect, 1).Error)
			if requests.Load() != 0 || count != 0 || sect.Prestige != 1000 {
				t.Fatal("queued retreat used obsolete membership")
			}
		})
	}
}

func TestRetreatWithoutUsableBaselineDoesNotCharge(t *testing.T) {
	for _, personal := range []bool{true, false} {
		t.Run(fmt.Sprint("personal=", personal), func(t *testing.T) {
			_, _, submit := setupRetreatPreparation(t, personal)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
			t.Cleanup(func() { waitSectRetreatPreparations(t) })
			submit()
			waitSectRetreatPreparations(t)
			var count int64
			contributionMust(t, storage.DB.Model(&model.SectCaveRetreat{}).Count(&count).Error)
			var member model.SectMember
			var sect model.Sect
			contributionMust(t, storage.DB.First(&member, "user_id = 1").Error)
			contributionMust(t, storage.DB.First(&sect, 1).Error)
			if count != 0 || member.PersonalPrestige != 100 || sect.Prestige != 1000 {
				t.Fatal("unknown baseline was treated as zero and charged")
			}
		})
	}
}

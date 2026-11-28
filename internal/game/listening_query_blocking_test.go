package game

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestExpiredQueriesDoNotWaitForListeningBatch(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 11*time.Minute, .5)
	contributionMust(t, storage.DB.AutoMigrate(&model.RankTitleHolder{}))
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	bot, replies := secretRealmQueryBot(t)
	listeningRefreshMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		report := GetPersonalReport(nil, "abs1")
		if !strings.Contains(report, "展示旧数据") {
			t.Error("report did not disclose stale data")
		}
		HandleCultivationRank(bot, 1)
		state := EnsureListeningFresh(1)
		if state.Fresh {
			t.Error("strict operation accepted expired data")
		}
	}()
	select {
	case <-done:
		listeningRefreshMu.Unlock()
	case <-time.After(time.Second):
		listeningRefreshMu.Unlock()
		<-done
		t.Fatal("query or strict operation waited behind the full batch")
	}
	if requests.Load() != 0 || !strings.Contains(replies(), "已保存修为") {
		t.Fatalf("read-only query refreshed ABS: requests=%d replies=%s", requests.Load(), replies())
	}
}

func TestStrictListeningDoesNotWaitForAnotherAccountFlight(t *testing.T) {
	setupListeningFreshnessTest(t)
	seedListeningFreshnessUser(t, 1, 11*time.Minute, .5)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	key := "stats:1:\"abs1\""
	call := &listeningFetchCall{done: make(chan struct{})}
	listeningFetches.Lock()
	listeningFetches.calls[key] = call
	listeningFetches.Unlock()
	defer func() {
		listeningFetches.Lock()
		delete(listeningFetches.calls, key)
		close(call.done)
		listeningFetches.Unlock()
	}()
	state := EnsureListeningFresh(1)
	if state.Fresh || requests.Load() != 0 {
		t.Fatal("strict operation waited on or duplicated an in-flight synchronization")
	}
}

func TestSectTaskViewUsesSavedDataWithoutABS(t *testing.T) {
	bot := setupSectRegressionTest(t)
	contributionMust(t, storage.DB.AutoMigrate(&model.SectDailyTaskClaim{}, &model.SectWeeklyTaskSettlement{}, &model.SignInLog{}, &model.SectTechnology{}))
	contributionMust(t, storage.DB.Model(&model.SectMember{}).Where("user_id = ?", 1).Update("role", SectRoleOwner).Error)
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) })
	// A full batch may be active while the owner and personal task pages are read.
	listeningRefreshMu.Lock()
	defer listeningRefreshMu.Unlock()
	// The test helper's user is in sect 1; no query needs to contact ABS.
	msg := secretRealmTaskMessage()
	HandleSectCommand(bot, msg, "宗门任务")
	HandleSectCommand(bot, msg, "我的宗门任务")
	if requests.Load() != 0 {
		t.Fatal("task view fetched member statistics")
	}
}

func secretRealmTaskMessage() *tgbotapi.Message {
	return &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
}

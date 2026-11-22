package game

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func waitBreakthroughPanels(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contributionMust(t, background.Panels.WaitIdle(ctx))
}

func TestBreakthroughPanelRetryYieldsFailedBatch(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprint("permanent=", permanent), func(t *testing.T) {
			setupGuardTestDB(t)
			var mu sync.Mutex
			attempts := make(map[int]int)
			healthy := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				_ = r.ParseForm()
				id, _ := strconv.Atoi(r.Form.Get("message_id"))
				mu.Lock()
				attempts[id]++
				fail := id <= 32 && !healthy
				mu.Unlock()
				if fail {
					if permanent {
						fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
					} else {
						fmt.Fprint(w, `{"ok":false,"error_code":503,"description":"temporary failure"}`)
					}
					return
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":-1,"type":"supergroup"}}}`)
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			contributionMust(t, err)
			for i := 1; i <= 33; i++ {
				contributionMust(t, storage.DB.Create(&model.BreakthroughRitual{UserID: int64(i), ChatID: -1, MessageID: i,
					PanelDirty: true, PanelVersion: 1, Status: BreakthroughRitualCancelled}).Error)
			}
			for range 3 {
				retryBreakthroughPanels(bot)
				waitBreakthroughPanels(t)
			}
			mu.Lock()
			for i := 1; i <= 33; i++ {
				if attempts[i] != 1 {
					t.Errorf("panel %d attempted %d times; want one attempt without starvation or immediate retries", i, attempts[i])
				}
			}
			healthy = true
			mu.Unlock()
			var first model.BreakthroughRitual
			contributionMust(t, storage.DB.First(&first, 1).Error)
			if permanent {
				if first.PanelDirty || first.PanelNextRetryAt != nil {
					t.Fatal("deleted panel retained a retry marker")
				}
			} else {
				if !first.PanelDirty || first.PanelNextRetryAt == nil || !first.PanelNextRetryAt.After(time.Now()) {
					t.Fatal("transient failure lost its durable backoff")
				}
				// A newer panel version bypasses the old version's backoff.
				refreshBreakthroughRitualPanel(bot, first.ID, nil)
				waitBreakthroughPanels(t)
				mu.Lock()
				if attempts[1] != 2 {
					t.Error("new panel version waited behind the previous retry deadline")
				}
				mu.Unlock()
				// Recovery uses persisted deadlines, without an in-memory retry list.
				contributionMust(t, storage.DB.Model(&model.BreakthroughRitual{}).Where("panel_dirty = ?", true).
					UpdateColumn("panel_next_retry_at", time.Now().Add(-time.Second)).Error)
				retryBreakthroughPanels(bot)
				waitBreakthroughPanels(t)
				first = model.BreakthroughRitual{}
				contributionMust(t, storage.DB.First(&first, 1).Error)
				if first.PanelDirty || first.PanelNextRetryAt != nil {
					t.Fatal("recovered panel did not clear its retry state")
				}
				var pending int64
				contributionMust(t, storage.DB.Model(&model.BreakthroughRitual{}).Where("panel_dirty = ?", true).Count(&pending).Error)
				if pending != 0 {
					t.Fatal("not all transient failures recovered")
				}
			}
		})
	}
}

func TestBreakthroughOldPanelFailurePreservesNewVersion(t *testing.T) {
	setupGuardTestDB(t)
	ritual := model.BreakthroughRitual{UserID: 1, ChatID: -1, MessageID: 1, PanelDirty: true, PanelVersion: 1, Status: BreakthroughRitualCancelled}
	contributionMust(t, storage.DB.Create(&ritual).Error)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		// A newer update arrives while the older Telegram edit is in flight.
		if err := storage.DB.Model(&ritual).Updates(map[string]any{"panel_version": 2, "panel_next_retry_at": nil}).Error; err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	contributionMust(t, err)
	refreshBreakthroughRitualPanelSync(bot, ritual.ID, nil)
	contributionMust(t, storage.DB.First(&ritual, ritual.ID).Error)
	if !ritual.PanelDirty || ritual.PanelVersion != 2 || ritual.PanelNextRetryAt != nil {
		t.Fatalf("older failure cleared or delayed the new update: %+v", ritual)
	}
}

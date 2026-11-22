package activity

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

func waitTreasurePanels(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := background.Panels.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTreasurePanelRetryYieldsFailedBatch(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprint("permanent=", permanent), func(t *testing.T) {
			setupTribulationTestDB(t)
			if err := storage.DB.AutoMigrate(&model.TreasureRound{}, &model.TreasurePick{}); err != nil {
				t.Fatal(err)
			}
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
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 33; i++ {
				if err := storage.DB.Create(&model.TreasureRound{ChatID: -1, MessageID: i, PanelDirty: true, PanelVersion: 1,
					Status: TreasureClosed, EntryCost: treasureEntryCost, RewardsJSON: `[0,0,0,1,1,1,2,2,10]`}).Error; err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				retryTreasurePanels(bot)
				waitTreasurePanels(t)
			}
			mu.Lock()
			for i := 1; i <= 33; i++ {
				if attempts[i] != 1 {
					t.Errorf("panel %d attempted %d times; want one attempt without starvation or immediate retries", i, attempts[i])
				}
			}
			healthy = true
			mu.Unlock()
			var first model.TreasureRound
			if err := storage.DB.First(&first, 1).Error; err != nil {
				t.Fatal(err)
			}
			if permanent {
				if first.PanelDirty || first.PanelNextRetryAt != nil {
					t.Fatal("deleted panel retained a retry marker")
				}
			} else {
				if !first.PanelDirty || first.PanelNextRetryAt == nil || !first.PanelNextRetryAt.After(time.Now()) {
					t.Fatal("transient failure lost its durable backoff")
				}
				refreshTreasurePanel(bot, first.ID)
				waitTreasurePanels(t)
				mu.Lock()
				if attempts[1] != 2 {
					t.Error("new panel version waited behind the previous retry deadline")
				}
				mu.Unlock()
				if err := storage.DB.Model(&model.TreasureRound{}).Where("panel_dirty = ?", true).
					UpdateColumn("panel_next_retry_at", time.Now().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
				retryTreasurePanels(bot)
				waitTreasurePanels(t)
				first = model.TreasureRound{}
				if err := storage.DB.First(&first, 1).Error; err != nil {
					t.Fatal(err)
				}
				if first.PanelDirty || first.PanelNextRetryAt != nil {
					t.Fatal("recovered panel did not clear its retry state")
				}
				var pending int64
				if err := storage.DB.Model(&model.TreasureRound{}).Where("panel_dirty = ?", true).Count(&pending).Error; err != nil {
					t.Fatal(err)
				}
				if pending != 0 {
					t.Fatal("not all transient failures recovered")
				}
			}
		})
	}
}

func TestTreasureOldPanelFailurePreservesNewVersion(t *testing.T) {
	setupTribulationTestDB(t)
	if err := storage.DB.AutoMigrate(&model.TreasureRound{}, &model.TreasurePick{}); err != nil {
		t.Fatal(err)
	}
	round := model.TreasureRound{ChatID: -1, MessageID: 1, PanelDirty: true, PanelVersion: 1,
		Status: TreasureClosed, EntryCost: treasureEntryCost, RewardsJSON: `[0,0,0,1,1,1,2,2,10]`}
	if err := storage.DB.Create(&round).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := storage.DB.Model(&round).Updates(map[string]any{"panel_version": 2, "panel_next_retry_at": nil}).Error; err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	refreshTreasurePanelSync(bot, round.ID)
	if err := storage.DB.First(&round, round.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !round.PanelDirty || round.PanelVersion != 2 || round.PanelNextRetryAt != nil {
		t.Fatalf("older failure cleared or delayed the new update: %+v", round)
	}
}

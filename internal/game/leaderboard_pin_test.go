package game

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func setupLeaderboardPinTest(t *testing.T, rejectedEndpoint string) (*tgbotapi.BotAPI, func() []string) {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	if err := storage.SetSystemConfigStringChecked("pinned_leaderboard_daily", "41"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		endpoint := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if endpoint == "getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		mu.Lock()
		calls = append(calls, endpoint)
		mu.Unlock()
		if endpoint == rejectedEndpoint {
			fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: pin permission denied"}`)
			return
		}
		if endpoint == "sendMessage" {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100,"type":"supergroup"}}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func TestLeaderboardReadFailureDoesNotPublish(t *testing.T) {
	for _, invalidState := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidState), func(t *testing.T) {
			bot, calls := setupLeaderboardPinTest(t, "")
			if invalidState {
				if err := storage.SetSystemConfigStringChecked("pinned_leaderboard_daily", "invalid"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := storage.DB.Callback().Query().Before("gorm:query").Register("test:pin_read_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "system_configs" {
						tx.AddError(errors.New("state unavailable"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer storage.DB.Callback().Query().Remove("test:pin_read_failure")
			}
			if err := sendAndManageLeaderboardPinSync(bot, -100, "新榜单", "daily"); err == nil {
				t.Fatal("unreadable pin state was accepted")
			}
			if len(calls()) != 0 {
				t.Fatalf("published without knowing the previous pin: %v", calls())
			}
		})
	}
}

func TestLeaderboardPinFailurePreservesPreviousPin(t *testing.T) {
	bot, calls := setupLeaderboardPinTest(t, "pinChatMessage")
	if err := sendAndManageLeaderboardPinSync(bot, -100, "新榜单", "daily"); err == nil {
		t.Fatal("failed pin was reported as success")
	}
	if !reflect.DeepEqual(calls(), []string{"sendMessage", "pinChatMessage"}) {
		t.Fatalf("old pin was removed before its replacement succeeded: %v", calls())
	}
	state, err := storage.GetSystemConfigStringChecked("pinned_leaderboard_daily")
	if err != nil || state != "41" {
		t.Fatalf("failed pin replaced stored state: %q %v", state, err)
	}
}

func TestLeaderboardStateWriteFailurePreservesPreviousPin(t *testing.T) {
	bot, calls := setupLeaderboardPinTest(t, "")
	if err := storage.DB.Exec(`CREATE TRIGGER fail_pin_state BEFORE UPDATE ON system_configs WHEN NEW.key = 'pinned_leaderboard_daily' BEGIN SELECT RAISE(ABORT, 'pin state unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := sendAndManageLeaderboardPinSync(bot, -100, "新榜单", "daily"); err == nil {
		t.Fatal("failed state write was reported as success")
	}
	if !reflect.DeepEqual(calls(), []string{"sendMessage", "pinChatMessage"}) {
		t.Fatalf("old pin removed before new state was saved: %v", calls())
	}
}

func TestLeaderboardPinsReplacementBeforeRemovingPreviousPin(t *testing.T) {
	bot, calls := setupLeaderboardPinTest(t, "")
	if err := sendAndManageLeaderboardPinSync(bot, -100, "新榜单", "daily"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls(), []string{"sendMessage", "pinChatMessage", "unpinChatMessage"}) {
		t.Fatalf("unsafe pin replacement order: %v", calls())
	}
	state, err := storage.GetSystemConfigStringChecked("pinned_leaderboard_daily")
	if err != nil || state != "42" {
		t.Fatalf("new pin was not recorded: %q %v", state, err)
	}
}

package telegram

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBackupPinReplacementPreservesStateAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, rejectedEndpoint, wantID string
		failWrite, wantError           bool
		calls                          []string
	}{
		{"success", "", "42", false, false, []string{"pinChatMessage", "unpinChatMessage"}},
		{"pin rejected", "pinChatMessage", "41", false, true, []string{"pinChatMessage"}},
		{"state write failed", "", "41", true, true, []string{"pinChatMessage"}},
		{"unpin rejected", "unpinChatMessage", "42", false, true, []string{"pinChatMessage", "unpinChatMessage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pin.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			previousDB, previousConfig := storage.DB, config.AppConfig
			storage.DB = db
			config.AppConfig = &config.Config{BackupGroupID: -100}
			t.Cleanup(func() { storage.DB = previousDB; config.AppConfig = previousConfig; _ = conn.Close() })
			if err := db.AutoMigrate(&model.SystemConfig{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("CREATE UNIQUE INDEX idx_test_config ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.SetSystemConfigStringChecked(storage.BackupLastPinnedMessageIDKey, "41"); err != nil {
				t.Fatal(err)
			}
			if tc.failWrite {
				if err := db.Exec(`CREATE TRIGGER reject_pin_state BEFORE UPDATE ON system_configs WHEN NEW.key = 'backup_last_pinned_message_id' BEGIN SELECT RAISE(ABORT, 'state unavailable'); END`).Error; err != nil {
					t.Fatal(err)
				}
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
				if endpoint == tc.rejectedEndpoint {
					fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: permission denied"}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"result":true}`)
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			pinLatestBackupMessage(bot, 42)
			mu.Lock()
			gotCalls := append([]string(nil), calls...)
			mu.Unlock()
			if !reflect.DeepEqual(gotCalls, tc.calls) {
				t.Fatalf("unsafe backup pin replacement: got=%v want=%v", gotCalls, tc.calls)
			}
			pinnedID, err := storage.GetSystemConfigStringChecked(storage.BackupLastPinnedMessageIDKey)
			if err != nil || pinnedID != tc.wantID {
				t.Fatalf("incorrect pin receipt: id=%s want=%s error=%v", pinnedID, tc.wantID, err)
			}
			pinErr, err := storage.GetSystemConfigStringChecked(storage.BackupLastPinErrorKey)
			if err != nil || (pinErr != "") != tc.wantError {
				t.Fatalf("lost backup pin error: stored=%q error=%v", pinErr, err)
			}
		})
	}
}

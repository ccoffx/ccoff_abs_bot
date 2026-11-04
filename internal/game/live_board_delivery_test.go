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

func TestLiveBoardEditFailureOnlyReplacesUnavailableMessage(t *testing.T) {
	for _, kind := range []string{"boss", "realm"} {
		for _, tc := range []struct {
			name, description string
			code, sends       int
			wantError         bool
		}{
			{"rate limit", "Too Many Requests: retry later", 429, 0, true},
			{"server error", "Bad Gateway", 502, 0, true},
			{"invalid text", "Bad Request: can't parse entities", 400, 0, true},
			{"unchanged", "Bad Request: message is not modified", 400, 0, false},
			{"deleted", "Bad Request: message to edit not found", 400, 1, false},
			{"uneditable", "Bad Request: message can't be edited", 400, 1, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				setupListeningAbuseScheduleDB(t)
				if err := storage.DB.AutoMigrate(&model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.Sect{}, &model.SectSecretRealmEvent{}, &model.SectSecretRealmParticipant{}); err != nil {
					t.Fatal(err)
				}
				var sends atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case strings.HasSuffix(r.URL.Path, "/getMe"):
						fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					case strings.HasSuffix(r.URL.Path, "/editMessageText"):
						fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q}`, tc.code, tc.description)
					default:
						sends.Add(1)
						fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100,"type":"supergroup"}}}`)
					}
				}))
				t.Cleanup(server.Close)
				bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
				if err != nil {
					t.Fatal(err)
				}
				if kind == "boss" {
					event := model.WorldBossEvent{BossID: "board", ChatID: -100, BoardChatID: -100, BoardMessageID: 41, Status: "active"}
					if err := storage.DB.Create(&event).Error; err != nil {
						t.Fatal(err)
					}
					err = ensureWorldBossLiveBoardSync(bot, event.BossID)
				} else {
					sect := model.Sect{Name: "test", OwnerID: 1}
					if err := storage.DB.Create(&sect).Error; err != nil {
						t.Fatal(err)
					}
					profile := DefaultSectSecretRealmConfig().Profiles[0]
					event := model.SectSecretRealmEvent{RealmID: "board", SectID: int64(sect.ID), ChatID: -100, BoardChatID: -100, BoardMessageID: 41, Status: "active", EndAt: time.Now().Add(time.Hour), ProfileKey: profile.Key, ConfigSnapshot: sectSecretRealmProfileSnapshot(profile)}
					if err := storage.DB.Create(&event).Error; err != nil {
						t.Fatal(err)
					}
					err = ensureSectSecretRealmLiveBoardSync(bot, event.RealmID)
				}
				if (err != nil) != tc.wantError || sends.Load() != int64(tc.sends) {
					t.Fatalf("edit failure caused incorrect publication: sends=%d want=%d error=%v", sends.Load(), tc.sends, err)
				}
			})
		}
	}
}

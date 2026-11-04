package game

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSectRetreatRevalidatesMembershipAfterABSFetch(t *testing.T) {
	for _, change := range []string{"member leaves", "operator changes sect", "sect upgrades"} {
		t.Run(change, func(t *testing.T) {
			bot := setupSectRegressionTest(t)
			now := time.Now()
			for _, operation := range []func() error{
				func() error {
					return storage.DB.Model(&model.Sect{}).Where("id = 1").Updates(map[string]any{"level": 4, "prestige": 1000}).Error
				},
				func() error {
					return storage.DB.Model(&model.SectMember{}).Where("user_id = 1").Update("role", SectRoleOwner).Error
				},
				func() error {
					return storage.DB.Create(&model.User{TelegramID: 2, Username: "target", AbsUserID: "abs2"}).Error
				},
				func() error {
					return storage.DB.Create(&model.SectMember{SectID: 1, UserID: 2, CaveUnlockedAt: &now}).Error
				},
				func() error {
					return storage.DB.Create(&model.Sect{Name: "other", OwnerID: 3, Level: 4, Prestige: 1000}).Error
				},
			} {
				if err := operation(); err != nil {
					t.Fatal(err)
				}
			}
			var once sync.Once
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() {
					var err error
					switch change {
					case "member leaves":
						err = storage.DB.Where("user_id = 2").Delete(&model.SectMember{}).Error
					case "operator changes sect":
						err = storage.DB.Model(&model.SectMember{}).Where("user_id = 1").Update("sect_id", 2).Error
					case "sect upgrades":
						err = storage.DB.Model(&model.Sect{}).Where("id = 1").Update("level", 5).Error
					}
					if err != nil {
						t.Error(err)
					}
				})
				if strings.HasSuffix(r.URL.Path, "/listening-sessions") {
					fmt.Fprint(w, `{"sessions":[]}`)
				} else {
					fmt.Fprint(w, `{"days":{}}`)
				}
			})
			handleStartSectCaveRetreat(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, true)
			var retreats int64
			if err := storage.DB.Model(&model.SectCaveRetreat{}).Count(&retreats).Error; err != nil {
				t.Fatal(err)
			}
			var sects []model.Sect
			if err := storage.DB.Order("id").Find(&sects).Error; err != nil {
				t.Fatal(err)
			}
			wantCount, wantPrestige := int64(0), 1000
			if change == "sect upgrades" {
				wantCount, wantPrestige = 1, 1000-sectCaveSectRetreatCost(5)
			}
			if retreats != wantCount || sects[0].Prestige != wantPrestige || sects[1].Prestige != 1000 {
				t.Fatalf("stale membership/cost used: retreats=%d sects=%+v", retreats, sects)
			}
		})
	}
}

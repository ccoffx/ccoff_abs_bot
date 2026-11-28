package game

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestExternalListeningRefreshWaitsBeforeFetchingDuringBatch(t *testing.T) {
	for _, kind := range []string{"explicit_refresh"} {
		t.Run(kind, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.CultivationUpgradeEvent{}, &model.SectMember{}, &model.Sect{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Exec("CREATE UNIQUE INDEX ux_external_refresh_cultivation ON cultivations(user_id)").Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Create(&model.User{TelegramID: 2, Username: "second", AbsUserID: "abs2", Status: "active"}).Error; err != nil {
				t.Fatal(err)
			}
			var secondRequests atomic.Int32
			started := make(chan struct{})
			requested := make(chan struct{}, 2)
			done := make(chan error, 1)
			today := SectDayKey(time.Now())
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/listening-days") {
					fmt.Fprint(w, `{"sessions":[],"mediaProgress":[]}`)
					return
				}
				seconds := 3600
				if strings.Contains(r.URL.Path, "/abs1/") {
					// The batch is holding the exclusive lock while fetching user 1.
					// A concurrent external operation must wait even when batch workers overlap.
					go func() {
						close(started)
						if kind == "secret_realm" {
							snapshot, err := fetchSectSecretRealmListeningSnapshotFromABS("abs2")
							if err == nil && snapshot.DaySeconds != 10800 {
								err = fmt.Errorf("secret realm used old snapshot: %+v", snapshot)
							}
							done <- err
						} else {
							_, _, _, err := refreshUserDailyListeningAndCultivation(2)
							done <- err
						}
					}()
					<-started
					select {
					case <-requested:
						t.Error("operation fetched ABS before the batch released its lock")
					case <-time.After(50 * time.Millisecond):
					}
				} else {
					seconds = 7200
					if secondRequests.Add(1) > 1 {
						requested <- struct{}{}
						seconds = 10800
					}
				}
				fmt.Fprintf(w, `{"days":{%q:%d}}`, today, seconds)
			})
			if _, _, _, err := RefreshAllDailyListeningStatsWithOptions(false); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not finish after the batch")
			}
			if secondRequests.Load() != 2 {
				t.Fatal("expected a batch fetch and a subsequent operation fetch", secondRequests.Load())
			}
		})
	}
}

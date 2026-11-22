package game

import (
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestRealmFailedSampleRepricesSavedProgressAfterGuardianLeaves(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint("replacement=", replacement), func(t *testing.T) {
			event := secretRealmLiveFixture(t, 3)
			contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = 1", event.RealmID).
				Update("major_realm", 4).Error)
			contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = 2", event.RealmID).
				Updates(map[string]any{"raw_delta_seconds": 1764, "suppressed_hours": .49, "delta_hours": .539,
					"guardian_bonus_percent": 10, "reward_points": 3, "reward_contribution": 1,
					"reward_drop_item": "old drop", "reward_drop_quantity": 1}).Error)
			wantGuardian, wantBonus, wantPoints := int64(0), 0, 0
			if replacement {
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = 3", event.RealmID).
					Update("major_realm", 2).Error)
				wantGuardian, wantBonus, wantPoints = 3, 3, 3
			}
			contributionMust(t, storage.DB.Where("user_id = 1").Delete(&model.SectMember{}).Error)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
			_, err := refreshSectSecretRealmLiveProgress(event)
			contributionMust(t, err)
			contributionMust(t, storage.DB.First(&event, event.ID).Error)
			var p model.SectSecretRealmParticipant
			contributionMust(t, storage.DB.First(&p, "realm_id = ? AND user_id = 2", event.RealmID).Error)
			wantHours := .49 * float64(100+wantBonus) / 100
			if event.GuardianUserID != wantGuardian || event.GuardianBonusPercent != wantBonus || !event.LiveRefreshIncomplete ||
				p.RawDeltaSeconds != 1764 || p.GuardianBonusPercent != wantBonus || math.Abs(p.DeltaHours-wantHours) > 1e-9 ||
				p.RewardPoints != wantPoints || p.RewardDropItem != "" || p.RewardDropQuantity != 0 || p.IsRewarded {
				t.Fatalf("failed sample retained old guardian rewards: event=%+v participant=%+v", event, p)
			}
			if math.Abs(event.TotalDeltaHours-p.DeltaHours) > 1e-9 || event.TotalRewardPoints != p.RewardPoints || event.TotalRewardDrops != 0 {
				t.Fatalf("published totals disagree with repriced participants: event=%+v participant=%+v", event, p)
			}
		})
	}
}

func TestActivityLivePublicationRechecksEligibilityAfterSlowABS(t *testing.T) {
	for _, scenario := range []struct {
		kind, change string
		code         int
	}{
		{"boss", "binding", 200}, {"boss", "binding", 503},
		{"realm", "binding", 200}, {"realm", "member", 200}, {"realm", "member", 503},
	} {
		t.Run(fmt.Sprintf("%s/%s/%d", scenario.kind, scenario.change, scenario.code), func(t *testing.T) {
			var boss model.WorldBossEvent
			var realm model.SectSecretRealmEvent
			if scenario.kind == "boss" {
				boss = worldBossLiveFixture(t, 1)
				contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", boss.BossID).
					Updates(map[string]any{"damage": boss.MaxHP + 1, "delta_hours": .1}).Error)
			} else {
				realm = secretRealmLiveFixture(t, 1)
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", realm.RealmID).
					Updates(map[string]any{"delta_hours": .1, "reward_points": 1, "major_realm": 4}).Error)
				contributionMust(t, storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", 4).Error)
			}
			started, release := make(chan struct{}, 1), make(chan struct{})
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				<-release
				w.WriteHeader(scenario.code)
				fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
			})
			done := make(chan error, 1)
			go func() {
				if scenario.kind == "boss" {
					_, _, err := refreshWorldBossLiveDamageLocked(boss)
					done <- err
				} else {
					_, err := refreshSectSecretRealmLiveProgress(realm)
					done <- err
				}
			}()
			defer func() { close(release); contributionMust(t, <-done) }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("ABS sampling did not start")
			}
			if scenario.change == "binding" {
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "replacement").Error)
			} else {
				contributionMust(t, storage.DB.Where("user_id = ?", 1).Delete(&model.SectMember{}).Error)
			}
			// Release the request and wait for publication before inspecting data.
			release <- struct{}{}
			select {
			case err := <-done:
				done <- err
				contributionMust(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("publication did not finish")
			}
			if scenario.kind == "boss" {
				var p model.WorldBossParticipant
				contributionMust(t, storage.DB.First(&p, "boss_id = ?", boss.BossID).Error)
				contributionMust(t, storage.DB.First(&boss, boss.ID).Error)
				if p.Damage != 0 || p.DeltaHours != 0 || boss.IsKilled || boss.CurrentHP != float64(boss.MaxHP) {
					t.Fatalf("ineligible account affected Boss HP: damage=%v hours=%v event=%+v", p.Damage, p.DeltaHours, boss)
				}
			} else {
				var p model.SectSecretRealmParticipant
				contributionMust(t, storage.DB.First(&p, "realm_id = ?", realm.RealmID).Error)
				contributionMust(t, storage.DB.First(&realm, realm.ID).Error)
				if p.DeltaHours != 0 || p.RewardPoints != 0 || realm.GuardianUserID != 0 || realm.TotalDeltaHours != 0 {
					t.Fatalf("ineligible account affected realm: participant=%+v event=%+v", p, realm)
				}
			}
		})
	}
}

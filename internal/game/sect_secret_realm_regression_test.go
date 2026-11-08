package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func secretRealmSettlementFixture(t *testing.T, users int) model.SectSecretRealmEvent {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	old := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = old })
	if err := storage.DB.AutoMigrate(&model.Sect{}, &model.SectMember{}, &model.SectContributionLog{}, &model.SectSecretRealmEvent{}, &model.SectSecretRealmParticipant{}, &model.Cultivation{}, &model.PointTransaction{}, &model.Inventory{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec("CREATE UNIQUE INDEX idx_test_realm_inventory ON inventories(user_id, item_name) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	sect := model.Sect{Name: "回归宗门", OwnerID: 1}
	if err := storage.DB.Create(&sect).Error; err != nil {
		t.Fatal(err)
	}
	profile := DefaultSectSecretRealmConfig().Profiles[0]
	end := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	event := model.SectSecretRealmEvent{RealmID: "regression", SectID: int64(sect.ID), Name: "回归秘境", ProfileKey: profile.Key, ConfigSnapshot: sectSecretRealmProfileSnapshot(profile), Status: "active", StartAt: end.Add(-time.Hour), EndAt: end}
	if err := storage.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= int64(users); id++ {
		for _, row := range []any{
			&model.User{TelegramID: id, Username: fmt.Sprint("user", id), AbsUserID: fmt.Sprint("abs", id)},
			&model.SectMember{UserID: id, SectID: event.SectID},
			&model.SectSecretRealmParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, RealmID: event.RealmID, SectID: event.SectID, UserID: id, AbsUserID: fmt.Sprint("abs", id)},
		} {
			if err := storage.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return event
}

func secretRealmSessionResponse(w http.ResponseWriter, r *http.Request, event model.SectSecretRealmEvent, after bool) {
	if strings.HasSuffix(r.URL.Path, "listening-stats") {
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{event.StartAt.Format("2006-01-02"): 3600}})
		return
	}
	start, end := event.StartAt, event.EndAt
	if after {
		start, end = event.EndAt.Add(time.Minute), event.EndAt.Add(61*time.Minute)
	}
	id := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")[0]
	session := bossSession("session-"+id, start, end, 3600)
	session["userId"] = id
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
}

func TestSecretRealmRetriesABSFailureWithoutLosingRewards(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var fail atomic.Bool
	fail.Store(true)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		secretRealmSessionResponse(w, r, event, false)
	})
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "active" || event.SettledAt != nil {
		t.Fatalf("failed ABS read finalized rewards: %+v %v", event, err)
	}
	fail.Store(false)
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" || event.TotalRewardPoints <= 0 {
		t.Fatalf("retry failed to pay earned rewards: %+v %v", event, err)
	}
}

func TestSecretRealmExcludesListeningAfterEvent(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { secretRealmSessionResponse(w, r, event, true) })
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" || event.TotalRewardPoints != 0 {
		t.Fatalf("post-event playback earned rewards: %+v %v", event, err)
	}
}

func TestSecretRealmSettlesPlaybackWithSmallClientClockSkew(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	event.StartAt = event.EndAt.Add(-2 * time.Hour)
	if err := storage.DB.Model(&event).Update("start_at", event.StartAt).Error; err != nil {
		t.Fatal(err)
	}
	var p model.SectSecretRealmParticipant
	if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil {
		t.Fatal(err)
	}
	// Reproduce a participant joining 18.76 seconds after opening, whose ABS
	// session reports about 19 seconds more playback than its wall-clock span.
	p.CreatedAt = event.StartAt.Add(18*time.Second + 755625828*time.Nanosecond)
	p.BaseRawSeconds = 59439.05850859936
	if err := storage.DB.Save(&p).Error; err != nil {
		t.Fatal(err)
	}
	sessionStart := event.StartAt.Add(-2*time.Hour - 6*time.Minute - 43*time.Second)
	sessionEnd := sessionStart.Add(19721800 * time.Millisecond)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{
				SectDayKey(event.StartAt.AddDate(0, 0, -1)): 42387.48418782577,
				SectDayKey(event.StartAt):                   24251.99260032269,
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{
			bossSession("client-clock-skew", sessionStart, sessionEnd, 19740.760275532873),
		}})
	})
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" {
		t.Fatalf("settlement failed: %+v err=%v", event, err)
	}
	if err := storage.DB.First(&p, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	wantSeconds := event.EndAt.Sub(p.CreatedAt).Seconds()
	if math.Abs(p.RawDeltaSeconds-wantSeconds) > 1e-6 || !p.IsRewarded || p.RewardPoints != 12 || p.RewardContribution != 5 || p.RewardPrestige != 1 {
		t.Fatalf("small client skew lost or inflated rewards: %+v; want seconds=%.9f points=12 contribution=5 prestige=1", p, wantSeconds)
	}
	// Repeated settlement must preserve both the wallet balance and its ledger.
	settleSectSecretRealm(nil, event.RealmID, 0)
	var user model.User
	if err := storage.DB.First(&user, "telegram_id = ?", p.UserID).Error; err != nil || user.Points != 12 {
		t.Fatalf("unexpected wallet balance: %+v err=%v", user, err)
	}
	var rewards []model.PointTransaction
	if err := storage.DB.Where("user_id = ? AND type = ? AND ref_id = ?", p.UserID, "sect_secret_realm_reward", event.RealmID).Find(&rewards).Error; err != nil || len(rewards) != 1 || rewards[0].Delta != 12 {
		t.Fatalf("unexpected reward ledger: %+v err=%v", rewards, err)
	}
}

func TestSecretRealmDepartedMemberDoesNotBlockOthers(t *testing.T) {
	event := secretRealmSettlementFixture(t, 2)
	if err := storage.DB.Where("user_id = ?", 1).Delete(&model.SectMember{}).Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { secretRealmSessionResponse(w, r, event, false) })
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" || event.TotalRewardPoints <= 0 {
		t.Fatalf("departed member blocked settlement: %+v %v", event, err)
	}
	var departed model.SectSecretRealmParticipant
	if err := storage.DB.First(&departed, "realm_id = ? AND user_id = ?", event.RealmID, 1).Error; err != nil || departed.RewardPoints != 0 {
		t.Fatalf("departed member still rewarded: %+v %v", departed, err)
	}
}

func TestSecretRealmHistoricalScanResumesAcrossSettlementRetries(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var fail atomic.Bool
	fail.Store(true)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "listening-stats") {
			secretRealmSessionResponse(w, r, event, false)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 5 {
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{bossSession("earned", event.StartAt, event.EndAt, 3600)}})
			return
		}
		if page > 5 {
			t.Errorf("unexpected page %d", page)
		}
		sessions := make([]any, dailyListeningSessionPageSize)
		for i := range sessions {
			sessions[i] = bossSession(fmt.Sprintf("newer-%d-%d", page, i), event.EndAt.Add(time.Hour), event.EndAt.Add(2*time.Hour), 3600)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	for attempt := 0; attempt < 2; attempt++ {
		settleSectSecretRealm(nil, event.RealmID, 0)
		if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "active" || event.SettledAt != nil {
			t.Fatalf("incomplete history finalized event: %+v %v", event, err)
		}
		var p model.SectSecretRealmParticipant
		if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil || p.ListeningScanPage != 5 || p.FinalListeningScanComplete || p.IsRewarded {
			t.Fatalf("retry lost history cursor: %+v %v", p, err)
		}
	}
	fail.Store(false)
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" || event.TotalRewardPoints <= 0 {
		t.Fatalf("resumed history failed to pay: %+v %v", event, err)
	}
	settleSectSecretRealm(nil, event.RealmID, 0)
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", 1).Error; err != nil || u.Points != event.TotalRewardPoints {
		t.Fatalf("retry paid rewards more than once: %+v %v", u, err)
	}
}

func TestSecretRealmListeningStartsAtJoinTime(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var p model.SectSecretRealmParticipant
	if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil {
		t.Fatal(err)
	}
	p.CreatedAt = event.StartAt.Add(30 * time.Minute)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { secretRealmSessionResponse(w, r, event, false) })
	seconds, complete, err := sectSecretRealmListeningSeconds(&p, event, p.AbsUserID, time.Now())
	if err != nil || !complete || seconds != 1800 {
		t.Fatalf("pre-join listening counted: seconds=%f complete=%t err=%v", seconds, complete, err)
	}
}

func TestSecretRealmBindingChangeBeforeRewardIsExcluded(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var p model.SectSecretRealmParticipant
	if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil {
		t.Fatal(err)
	}
	p.DeltaHours, p.RewardPoints = 1, 6
	if err := storage.DB.Save(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", p.UserID).Update("abs_user_id", "replacement-abs").Error; err != nil {
		t.Fatal(err)
	}
	if err := grantSectSecretRealmRewards(event, []model.SectSecretRealmParticipant{p}); err != nil {
		t.Fatal(err)
	}
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", p.UserID).Error; err != nil || u.Points != 0 {
		t.Fatalf("rebound account received stale reward: %+v %v", u, err)
	}
	if err := storage.DB.First(&p, p.ID).Error; err != nil || p.RewardPoints != 0 || p.IsRewarded {
		t.Fatalf("rebound participant kept reward: %+v %v", p, err)
	}
}

func TestSecretRealmUnboundParticipantCannotEarnRewards(t *testing.T) {
	event := secretRealmSettlementFixture(t, 2)
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { secretRealmSessionResponse(w, r, event, false) })
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" {
		t.Fatalf("unbound member blocked settlement: %+v %v", event, err)
	}
	var unbound, valid model.SectSecretRealmParticipant
	if err := storage.DB.First(&unbound, "realm_id = ? AND user_id = ?", event.RealmID, 1).Error; err != nil || unbound.IsRewarded || unbound.RewardPoints != 0 {
		t.Fatalf("unbound participant rewarded: %+v %v", unbound, err)
	}
	if err := storage.DB.First(&valid, "realm_id = ? AND user_id = ?", event.RealmID, 2).Error; err != nil || !valid.IsRewarded {
		t.Fatalf("valid participant lost reward: %+v %v", valid, err)
	}
}

func TestSecretRealmRechecksUnbindingBeforePayout(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var p model.SectSecretRealmParticipant
	if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil {
		t.Fatal(err)
	}
	p.RewardPoints = 10
	if err := storage.DB.Model(&event).Update("status", "settling").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", p.UserID).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	if err := grantSectSecretRealmRewards(event, []model.SectSecretRealmParticipant{p}); err != nil {
		t.Fatal(err)
	}
	var u model.User
	if err := storage.DB.First(&u, "telegram_id = ?", p.UserID).Error; err != nil || u.Points != 0 {
		t.Fatalf("unbound account received points: %+v %v", u, err)
	}
}

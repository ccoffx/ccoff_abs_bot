package game

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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
	if err := storage.DB.AutoMigrate(&model.ActivityEntryRequest{}, &model.Sect{}, &model.SectMember{}, &model.SectContributionLog{}, &model.SectSecretRealmEvent{}, &model.SectSecretRealmParticipant{}, &model.Cultivation{}, &model.CultivationUpgradeEvent{}, &model.PointTransaction{}, &model.Inventory{}); err != nil {
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
	end := time.Now().Add(-time.Second).Truncate(time.Second)
	event := model.SectSecretRealmEvent{RealmID: "regression", SectID: int64(sect.ID), Name: "回归秘境", ProfileKey: profile.Key, ConfigSnapshot: sectSecretRealmProfileSnapshot(profile), Status: "active", StartAt: end.Add(-time.Hour), EndAt: end}
	if err := storage.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= int64(users); id++ {
		for _, row := range []any{
			&model.User{TelegramID: id, Username: fmt.Sprint("user", id), AbsUserID: fmt.Sprint("abs", id)},
			&model.SectMember{UserID: id, SectID: event.SectID},
			&model.SectSecretRealmParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, RealmID: event.RealmID, SectID: event.SectID, UserID: id, AbsUserID: fmt.Sprint("abs", id), ListeningSnapshot: newActivityListeningSnapshot(nil, event.StartAt)},
		} {
			if err := storage.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return event
}

func secretRealmSessionResponse(w http.ResponseWriter, r *http.Request, event model.SectSecretRealmEvent, after bool) {
	if strings.HasSuffix(r.URL.Path, "listening-days") {
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(event.StartAt): 3600}})
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

func TestSecretRealmMockUsesBusinessDayAcrossTimeZones(t *testing.T) {
	// The Linux runner loads timestamps in UTC; local development uses CST.
	// Both representations must return playback on the same business date.
	at := time.Date(2026, time.November, 25, 17, 30, 0, 0, time.UTC)
	for _, start := range []time.Time{at, at.In(storage.DailyOperationsLocation)} {
		t.Run(start.Location().String(), func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/users/abs1/listening-days", nil)
			secretRealmSessionResponse(response, request, model.SectSecretRealmEvent{StartAt: start}, false)
			var stats struct {
				Days map[string]float64 `json:"days"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
				t.Fatal(err)
			}
			if len(stats.Days) != 1 || stats.Days["2026-11-26"] != 3600 {
				t.Fatalf("mock playback used the wrong business day: %s", response.Body.String())
			}
		})
	}
}

func TestSecretRealmABSFailureFreezesSavedProgressAndPaysOnce(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", event.RealmID).Update("raw_delta_seconds", 1800).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "settling" {
		t.Fatalf("temporary failure finalized event: %+v", event)
	}
	// A restarted closing pass must use the saved progress once the window expires.
	contributionMust(t, storage.DB.Model(&event).Update("end_at", time.Now().Add(-activityFinalSnapshotGrace-time.Second)).Error)
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "settled" || event.TotalRewardPoints <= 0 {
		t.Fatalf("saved progress not paid after deadline: %+v", event)
	}
	before := event.TotalRewardPoints
	settleSectSecretRealm(nil, event.RealmID, 0)
	var user model.User
	contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 1).Error)
	if user.Points != before {
		t.Fatalf("duplicate payout: %d != %d", user.Points, before)
	}
}

func TestSecretRealmExcludesListeningAfterEvent(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	event.EndAt = time.Now().Add(-10 * time.Minute)
	contributionMust(t, storage.DB.Model(&event).Update("end_at", event.EndAt).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { secretRealmSessionResponse(w, r, event, true) })
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" || event.TotalRewardPoints != 0 {
		t.Fatalf("post-event playback earned rewards: %+v %v", event, err)
	}
}

func TestSecretRealmDailyProgressCapsAtParticipationWindow(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	event.StartAt = event.EndAt.Add(-2 * time.Hour)
	if err := storage.DB.Model(&event).Update("start_at", event.StartAt).Error; err != nil {
		t.Fatal(err)
	}
	var p model.SectSecretRealmParticipant
	if err := storage.DB.First(&p, "realm_id = ?", event.RealmID).Error; err != nil {
		t.Fatal(err)
	}
	// A participant joins 18.76 seconds after opening. Excess daily input cannot
	// credit the pre-entry interval or exceed the participant's own window.
	p.CreatedAt = event.StartAt.Add(18*time.Second + 755625828*time.Nanosecond)
	p.ListeningSnapshot = newActivityListeningSnapshot(nil, p.CreatedAt)
	if err := storage.DB.Save(&p).Error; err != nil {
		t.Fatal(err)
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "listening-days") {
			_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{
				SectDayKey(p.CreatedAt): 86400,
				SectDayKey(event.EndAt): 86400,
			}})
			return
		}
		t.Errorf("activity requested sessions: %s", r.URL.Path)
		w.WriteHeader(503)
	})
	settleSectSecretRealm(nil, event.RealmID, 0)
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" {
		t.Fatalf("settlement failed: %+v err=%v", event, err)
	}
	if err := storage.DB.First(&p, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	wantSeconds := event.EndAt.Sub(p.CreatedAt).Seconds()
	if math.Abs(p.RawDeltaSeconds-wantSeconds) > 1e-6 || !p.IsRewarded || p.RewardPoints != 11 || p.RewardContribution != 5 || p.RewardPrestige != 1 {
		t.Fatalf("daily input lost or inflated rewards: %+v; want seconds=%.9f points=11 contribution=5 prestige=1", p, wantSeconds)
	}
	// Repeated settlement must preserve both the wallet balance and its ledger.
	settleSectSecretRealm(nil, event.RealmID, 0)
	var user model.User
	if err := storage.DB.First(&user, "telegram_id = ?", p.UserID).Error; err != nil || user.Points != 11 {
		t.Fatalf("unexpected wallet balance: %+v err=%v", user, err)
	}
	var rewards []model.PointTransaction
	if err := storage.DB.Where("user_id = ? AND type = ? AND ref_id = ?", p.UserID, "sect_secret_realm_reward", event.RealmID).Find(&rewards).Error; err != nil || len(rewards) != 1 || rewards[0].Delta != 11 {
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

func TestSecretRealmFinalSnapshotSurvivesFailedPayoutAndRetry(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	var calls atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		secretRealmSessionResponse(w, r, event, false)
	})
	fail := true
	contributionMust(t, storage.DB.Callback().Update().Before("gorm:update").Register("fail_final_payout", func(tx *gorm.DB) {
		if tx.Statement.Table == "sect_secret_realm_events" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["status"] == "settled" && fail {
				tx.AddError(fmt.Errorf("injected payout failure"))
			}
		}
	}))
	t.Cleanup(func() { storage.DB.Callback().Update().Remove("fail_final_payout") })
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "active" {
		t.Fatal("payout did not roll back")
	}
	var p model.SectSecretRealmParticipant
	contributionMust(t, storage.DB.First(&p, "realm_id = ?", event.RealmID).Error)
	if !p.ListeningSnapshot.Finalized || p.IsRewarded {
		t.Fatalf("final input lost: %+v", p)
	}
	fail = false
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "settled" || event.TotalRewardPoints <= 0 || calls.Load() != 1 {
		t.Fatalf("retry changed snapshot: calls=%d event=%+v", calls.Load(), event)
	}
}

func TestSecretRealmListeningStartsAtJoinTime(t *testing.T) {
	event := secretRealmSettlementFixture(t, 1)
	event.Status = "settling"
	contributionMust(t, storage.DB.Save(&event).Error)
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

func TestSecretRealmGuardianChangeBeforeRewardRetriesWithoutStaleBonus(t *testing.T) {
	for _, change := range []string{"leave", "binding"} {
		t.Run(change, func(t *testing.T) {
			event := secretRealmSettlementFixture(t, 2)
			event.GuardianUserID, event.GuardianName = 1, "former guardian"
			event.GuardianMajorRealm, event.GuardianBonusPercent = 4, 10
			contributionMust(t, storage.DB.Save(&event).Error)
			var participants []model.SectSecretRealmParticipant
			contributionMust(t, storage.DB.Where("realm_id = ?", event.RealmID).Order("user_id").Find(&participants).Error)
			for i := range participants {
				p := &participants[i]
				p.ListeningSnapshot.Finalized, p.ListeningSnapshot.Seconds = true, 1800
				p.RawDeltaSeconds, p.SuppressedHours, p.DeltaHours = 1800, .5, .55
				p.GuardianBonusPercent, p.RewardPoints = 10, 3
				contributionMust(t, storage.DB.Save(p).Error)
			}
			if change == "leave" {
				contributionMust(t, storage.DB.Where("user_id = ?", 1).Delete(&model.SectMember{}).Error)
			} else {
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "replacement").Error)
			}
			if err := grantSectSecretRealmRewards(event, participants); err == nil {
				t.Fatal("stale guardian bonus was paid instead of retrying settlement")
			}
			var user model.User
			contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 2).Error)
			if user.Points != 0 {
				t.Fatal("failed settlement paid a partial reward")
			}
			// The scheduler retries from the already frozen listening snapshots.
			settleSectSecretRealm(nil, event.RealmID, 0)
			contributionMust(t, storage.DB.First(&event, event.ID).Error)
			var p model.SectSecretRealmParticipant
			contributionMust(t, storage.DB.First(&p, "realm_id = ? AND user_id = ?", event.RealmID, 2).Error)
			if event.Status != "settled" || event.GuardianUserID != 0 || event.GuardianBonusPercent != 0 || p.DeltaHours != .5 || p.GuardianBonusPercent != 0 || !p.IsRewarded {
				t.Fatalf("retry retained stale guardian bonus: event=%+v participant=%+v", event, p)
			}
			contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 2).Error)
			points := user.Points
			settleSectSecretRealm(nil, event.RealmID, 0)
			contributionMust(t, storage.DB.First(&user, "telegram_id = ?", 2).Error)
			if user.Points != points {
				t.Fatal("settlement retry paid twice")
			}
		})
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

func TestManualSecretRealmSettlementAlwaysReportsOutcome(t *testing.T) {
	for _, scenario := range []string{"refresh_busy", "event_busy", "invalid_rules", "database_error", "snapshot_pending", "success"} {
		t.Run(scenario, func(t *testing.T) {
			users := 0
			if scenario == "snapshot_pending" {
				users = 1
			}
			event := secretRealmSettlementFixture(t, users)
			bot, replies := secretRealmQueryBot(t)
			want := "结算未完成"
			switch scenario {
			case "refresh_busy":
				sectSecretRealmLiveRefreshMu.Lock()
				t.Cleanup(sectSecretRealmLiveRefreshMu.Unlock)
				want = "正在刷新"
			case "event_busy":
				release, ok := tryActivityEventTurn("realm:" + event.RealmID)
				if !ok {
					t.Fatal("could not acquire event turn")
				}
				t.Cleanup(release)
				want = "正在处理中"
			case "invalid_rules":
				contributionMust(t, storage.DB.Model(&event).Update("config_snapshot", "invalid").Error)
			case "database_error":
				contributionMust(t, storage.DB.Migrator().DropTable(&model.SectSecretRealmEvent{}))
			case "snapshot_pending":
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("status", "active").Error)
				bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
				want = "正在等待最终听书同步"
			case "success":
				want = "【宗门秘境结算】"
			}
			settleSectSecretRealm(bot, event.RealmID, 123)
			if got := replies(); !strings.Contains(got, want) {
				t.Fatalf("expected %q in settlement outcome, got %q", want, got)
			}
		})
	}
}

func TestManualSecretRealmSettlementReportsSuccessAfterScheduledSettlement(t *testing.T) {
	event := secretRealmSettlementFixture(t, 0)
	// The scheduled pass finishes before the queued manual request runs.
	settleSectSecretRealm(nil, event.RealmID, 0)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.Status != "settled" {
		t.Fatalf("scheduled settlement did not finish: %+v", event)
	}
	bot, replies := secretRealmQueryBot(t)
	settleSectSecretRealm(bot, event.RealmID, 123)
	want := "✅ 宗门秘境结算完成，请发送 `宗门秘境排行` 查看结果。"
	if got := replies(); got != want {
		t.Fatalf("expected only success outcome %q, got %q", want, got)
	}
}

func TestManualSecretRealmSettlementAvoidsDuplicateSuccess(t *testing.T) {
	for _, noticeChatID := range []int64{0, 123, 456} {
		t.Run(fmt.Sprint(noticeChatID), func(t *testing.T) {
			event := secretRealmSettlementFixture(t, 0)
			config.AppConfig.NoticeGroupID = noticeChatID
			bot, replies := secretRealmQueryBot(t)
			settleSectSecretRealm(bot, event.RealmID, 123)
			got := replies()
			if strings.Count(got, "【宗门秘境结算】") != 1 {
				t.Fatalf("expected one announcement: %s", got)
			}
			wantOutcome := 0
			if noticeChatID == 456 {
				wantOutcome = 1
			}
			if strings.Count(got, "✅ 宗门秘境结算完成") != wantOutcome {
				t.Fatalf("unexpected requester outcome: %s", got)
			}
		})
	}
}

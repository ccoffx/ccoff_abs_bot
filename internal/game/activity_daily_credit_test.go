package game

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestActivityDailyCreditMatchesOrdinaryAcrossMissingDatesAndArchive(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Date(2026, 11, 20, 23, 0, 0, 0, storage.DailyOperationsLocation)
	baseline := map[string]float64{"2026-11-20": 1800}
	snapshot := newActivityListeningSnapshot(baseline, start)
	progress := 0.0
	for _, step := range []struct {
		elapsed time.Duration
		days    map[string]float64
		want    float64
	}{
		{0, baseline, 0},
		{30 * time.Minute, map[string]float64{"2026-11-20": 2400}, 600},
		{65 * time.Minute, map[string]float64{"2026-11-21": 300}, 900},
		{75 * time.Minute, map[string]float64{"2026-11-20": 3000, "2026-11-21": 600}, 1800},
		{90 * time.Minute, map[string]float64{"2026-11-20": 9000, "2026-11-21": 900, "2000-01-01": 999999}, 2100},
		{105 * time.Minute, map[string]float64{"2026-11-20": 15000, "2026-11-21": 300}, 2100},
	} {
		at := start.Add(step.elapsed)
		credited, err := recordOfficialDailyListeningStatsAt(1, "abs1", step.days, at, at)
		contributionMust(t, err)
		progress, err = advanceActivityListeningDays(step.days, at, at, start, progress, &snapshot)
		contributionMust(t, err)
		ordinaryDelta := credited["2026-11-20"] + credited["2026-11-21"] - 1800
		if progress != step.want || progress != ordinaryDelta {
			t.Fatalf("at=%s activity=%v ordinary=%v want=%v", at, progress, ordinaryDelta, step.want)
		}
		// Resume using only serialized state, as after a process restart.
		encoded, err := json.Marshal(snapshot)
		contributionMust(t, err)
		snapshot = model.ActivityListeningSnapshot{}
		contributionMust(t, json.Unmarshal(encoded, &snapshot))
	}
}

func TestActivityDailyCreditCapsBaselineAndOneOClockProgress(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Date(2026, 11, 20, 0, 15, 0, 0, storage.DailyOperationsLocation)
	days := map[string]float64{"2026-11-20": 100000}
	snapshot := newActivityListeningSnapshot(days, start)
	if snapshot.BaselineSeconds != 900 {
		t.Fatalf("uncapped entry baseline: %+v", snapshot)
	}
	at := start.Add(45 * time.Minute)
	credited, err := recordOfficialDailyListeningStatsAt(1, "abs1", days, at, at)
	contributionMust(t, err)
	progress, err := advanceActivityListeningDays(days, at, at, start, 0, &snapshot)
	contributionMust(t, err)
	if credited["2026-11-20"] != 3600 || progress != 2700 {
		t.Fatalf("one-o-clock cap disagreed: ordinary=%v activity=%v", credited, progress)
	}
}

func TestLegacyActivityRebasesDailyInputAndRetainsOnlyPublishedProgress(t *testing.T) {
	event := worldBossLiveFixture(t, 1)
	var p model.WorldBossParticipant
	contributionMust(t, storage.DB.First(&p, "boss_id = ?", event.BossID).Error)
	p.ListeningSnapshot = model.ActivityListeningSnapshot{}
	p.BaseHours, p.DeltaHours = 999999, .1
	contributionMust(t, storage.DB.Save(&p).Error)
	amount := 1200.0
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": "ignored legacy total", "days": map[string]float64{SectDayKey(time.Now()): amount}})
	})
	for _, want := range []float64{360, 960} {
		hours, _, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
		contributionMust(t, err)
		if math.Abs(hours*3600-want) > 1e-9 {
			t.Fatalf("legacy cumulative baseline affected progress: %+v got=%v want=%v", p.ListeningSnapshot, hours*3600, want)
		}
		contributionMust(t, storage.DB.First(&p, p.ID).Error)
		amount += 600
	}
}

func TestBossAndSecretRealmUseTheSameDailyDelta(t *testing.T) {
	for _, kind := range []string{"boss", "secret_realm"} {
		t.Run(kind, func(t *testing.T) {
			var start time.Time
			var read func() (float64, error)
			if kind == "boss" {
				event, p := bossEvidenceFixture(t)
				event.Status = "settling"
				contributionMust(t, storage.DB.Save(&event).Error)
				start = event.StartAt
				read = func() (float64, error) {
					hours, _, err := worldBossListeningHours(&p, event, p.AbsUserID, time.Now())
					return hours * 3600, err
				}
			} else {
				event := secretRealmSettlementFixture(t, 1)
				event.Status = "settling"
				contributionMust(t, storage.DB.Save(&event).Error)
				var p model.SectSecretRealmParticipant
				contributionMust(t, storage.DB.First(&p, "realm_id = ?", event.RealmID).Error)
				start = event.StartAt
				read = func() (float64, error) {
					seconds, _, err := sectSecretRealmListeningSeconds(&p, event, p.AbsUserID, time.Now())
					return seconds, err
				}
			}
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/listening-stats") {
					t.Errorf("unexpected history request: %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 999999999, "days": map[string]float64{SectDayKey(start): 1800, "2000-01-01": 999999}})
			})
			seconds, err := read()
			contributionMust(t, err)
			if seconds != 1800 {
				t.Fatalf("activity delta=%v want=1800", seconds)
			}
		})
	}
}

func TestBossEntryKeepsSavedDailyHighWaterWhenABSDecreases(t *testing.T) {
	event := worldBossLiveFixture(t, 1)
	now := time.Now()
	expires := now.Add(time.Hour)
	contributionMust(t, storage.DB.Create(&model.User{TelegramID: 2, AbsUserID: "abs2", Status: "active", ExpireAt: &expires}).Error)
	baseline := dailyListeningCreditSeconds(SectDayKey(now), 900, now)
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: SectDayKey(now), CappedSeconds: baseline, LastFetchedAt: now}).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 999999999, "days": map[string]float64{SectDayKey(now): 0}})
	})
	bot, _ := secretRealmQueryBot(t)
	HandleWorldBossCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}, "参加Boss")
	waitActivityEntries(t)
	var participant model.WorldBossParticipant
	contributionMust(t, storage.DB.First(&participant, "boss_id = ? AND user_id = ?", event.BossID, 2).Error)
	if participant.ListeningSnapshot.BaselineSeconds != baseline {
		t.Fatalf("entry lost saved daily credit: got=%v want=%v", participant.ListeningSnapshot.BaselineSeconds, baseline)
	}
}

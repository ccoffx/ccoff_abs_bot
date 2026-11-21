package game

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestSectTaskUsesSavedRebindBaseline(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now())
	writeOfficial(t, map[string]float64{day: 0})
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "abs2").Error)
	_, err := recordOfficialDailyListeningStats(1, "abs2", map[string]float64{day: 7200}, time.Now(), nil)
	contributionMust(t, err)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fresh report unexpectedly fetched: %s", r.URL.Path)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	stat := officialStat(t, day)
	got, ok := getTodaySectListeningHoursFromABS(1, time.Now())
	cached, cacheOK := GetTodaySectListeningHoursFromCache(1, time.Now())
	if stat.OfficialBaselineKind != "rebind" || stat.EffectiveHours != 0 || !ok || !cacheOK || got != 0 || cached != 0 {
		t.Fatalf("imported account history became task progress: saved=%g task=%g cache=%g ok=%t/%t", stat.EffectiveHours, got, cached, ok, cacheOK)
	}
}

func TestSectTaskIncludesSavedRetreatBasis(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	start := sectStartOfDay(now)
	retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(2 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	retreat.ListeningEvidenceJSON = retreatTestEvidence(retreat.StartAt, retreat.EndAt, 2*3600)
	contributionMust(t, storage.DB.Create(&retreat).Error)
	stat := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now), RawSeconds: 6 * 3600, CappedSeconds: 6 * 3600, EffectiveHours: 5.2, LastFetchedAt: now}
	contributionMust(t, storage.DB.Create(&stat).Error)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fresh report unexpectedly fetched: %s", r.URL.Path)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	got, ok := getTodaySectListeningHoursFromABS(1, now)
	cached, cacheOK := GetTodaySectListeningHoursFromCache(1, now)
	if !ok || !cacheOK || math.Abs(got-5.6) > 1e-9 || got != cached {
		t.Fatalf("task dropped retreat bonus: task=%g cache=%g ok=%t/%t", got, cached, ok, cacheOK)
	}
}

func TestOrdinaryAndSecretRealmRejectInvalidDailyInputs(t *testing.T) {
	for _, value := range []string{"null", "-1", `"3600"`, "true", "1e999"} {
		t.Run(value, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			day := SectDayKey(time.Now().AddDate(0, 0, -1))
			writeOfficial(t, map[string]float64{day: 3600})
			before, total := officialStat(t, day), officialTotal(t)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"days":{%q:%s}}`, day, value)
			})
			if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
				t.Fatal("ordinary refresh accepted invalid daily input")
			}
			if _, err := fetchSectSecretRealmListeningSnapshotFromABS("abs1"); err == nil {
				t.Fatal("secret realm accepted invalid daily input")
			}
			after := officialStat(t, day)
			if after.RawSeconds != before.RawSeconds || after.CappedSeconds != before.CappedSeconds || !after.LastFetchedAt.Equal(before.LastFetchedAt) || officialTotal(t) != total {
				t.Fatalf("invalid source changed saved report: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestSecretRealmStillAcceptsExplicitZeroCorrection(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	writeOfficial(t, map[string]float64{day: 3600})
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"days":{%q:0}}`, day) })
	if _, err := fetchSectSecretRealmListeningSnapshotFromABS("abs1"); err != nil {
		t.Fatal(err)
	}
	stat := officialStat(t, day)
	if stat.RawSeconds != 0 || stat.CappedSeconds != 3600 || !strings.Contains(stat.ValidationJSON, "official_decrease_retained") {
		t.Fatalf("explicit zero no longer follows daily retention policy: %+v", stat)
	}
}

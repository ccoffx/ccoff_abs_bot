package game

import (
	"math"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyArchiveClosesOnceAndNeverRecomputesSavedHistory(t *testing.T) {
	setupRetreatBackfillDB(t)
	first := time.Date(2026, 11, 20, 23, 59, 0, 0, storage.DailyOperationsLocation)
	write := func(at time.Time, days map[string]float64) {
		t.Helper()
		_, err := recordOfficialDailyListeningStatsAt(1, "abs1", days, at, at)
		contributionMust(t, err)
	}
	write(first, map[string]float64{"2026-11-19": 3600, "2026-11-20": 7200})
	if !officialStat(t, "2026-11-19").Archived || officialStat(t, "2026-11-20").Archived {
		t.Fatal("incorrect open/closed day")
	}
	second := first.Add(2 * time.Minute)
	write(second, map[string]float64{"2026-11-19": 86400, "2026-11-20": 7500, "2026-11-21": 3600})
	closed := officialStat(t, "2026-11-20")
	if !closed.Archived || closed.CappedSeconds != 7500 || officialStat(t, "2026-11-19").CappedSeconds != 3600 {
		t.Fatalf("midnight close failed: %+v", closed)
	}
	today := officialStat(t, "2026-11-21")
	if today.CappedSeconds != 60 {
		t.Fatalf("00:01 allowed more than a minute: %+v", today)
	}
	write(second.Add(time.Hour), map[string]float64{"2026-11-19": 86400, "2026-11-20": 80000, "2026-11-21": 3600})
	if got := officialStat(t, "2026-11-20"); got.CappedSeconds != 7500 || !got.LastFetchedAt.Equal(closed.LastFetchedAt) {
		t.Fatal("archived day changed")
	}
	before := officialTotal(t)
	// Invalidation/restart rebuilds from persisted rows, never from changed ABS history.
	contributionMust(t, storage.DB.Model(&model.ListeningReportSnapshot{}).Where("user_id=1").Update("official_input_json", "").Error)
	write(second.Add(2*time.Hour), map[string]float64{"2026-11-19": 86400, "2026-11-20": 80000, "2026-11-21": 3600})
	if math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatal("cache rebuild recomputed history")
	}
}

func TestDailyArchiveWaitsForMissingClosingDayAcrossRefreshes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, final float64
		wantHours    float64
		legacy       bool
	}{
		{name: "late_growth", first: 3600, final: 7200, wantHours: 2},
		{name: "unchanged_value_still_closes", first: 3600, final: 3600, wantHours: 1},
		{name: "explicit_zero_closes", first: 0, final: 0, wantHours: 0},
		{name: "unchanged_raw_releases_elapsed_cap", first: 86400, final: 86400, wantHours: 8.5},
		{name: "legacy_open_day", first: 3600, final: 7200, wantHours: 2, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRetreatBackfillDB(t)
			first := time.Date(2026, 11, 18, 1, 0, 0, 0, storage.DailyOperationsLocation)
			write := func(at time.Time, days map[string]float64) {
				t.Helper()
				_, err := recordOfficialDailyListeningStatsAt(1, "abs1", days, at, at)
				contributionMust(t, err)
			}
			write(first, map[string]float64{"2026-11-18": tc.first})
			before := officialStat(t, "2026-11-18")
			beforeTotal := officialTotal(t)
			if tc.legacy {
				contributionMust(t, storage.DB.Model(&before).Update("official_policy", false).Error)
			}
			for i := 1; i <= 2; i++ {
				write(first.AddDate(0, 0, i), map[string]float64{})
				pending := officialStat(t, "2026-11-18")
				if pending.Archived || pending.CappedSeconds != before.CappedSeconds || !pending.LastFetchedAt.Equal(before.LastFetchedAt) || officialTotal(t) != beforeTotal {
					t.Fatalf("missing date was closed or changed: %+v total=%v", pending, officialTotal(t))
				}
			}
			// Loss of report metadata must not lose the persisted pending date.
			contributionMust(t, storage.DB.Where("user_id = ?", 1).Delete(&model.ListeningReportSnapshot{}).Error)
			closing := first.AddDate(0, 0, 3)
			write(closing, map[string]float64{"2026-11-18": tc.final})
			closed := officialStat(t, "2026-11-18")
			if !closed.Archived || closed.CappedSeconds != tc.final || math.Abs(officialTotal(t)-tc.wantHours) > 1e-9 {
				t.Fatalf("closing snapshot not credited: %+v total=%v", closed, officialTotal(t))
			}
			write(closing.Add(time.Hour), map[string]float64{"2026-11-18": 100000})
			if officialTotal(t) != tc.wantHours || officialStat(t, "2026-11-18").CappedSeconds != tc.final {
				t.Fatal("closed day was reopened or paid twice")
			}
		})
	}
}

func TestDailyArchiveWaitsForRequestStartedAfterMidnight(t *testing.T) {
	setupRetreatBackfillDB(t)
	started := time.Date(2026, 11, 20, 23, 59, 59, 0, storage.DailyOperationsLocation)
	_, err := recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{"2026-11-20": 3600, "2026-11-21": 1}, started, started.Add(2*time.Second))
	contributionMust(t, err)
	if officialStat(t, "2026-11-20").Archived {
		t.Fatal("request started before midnight closed its own day")
	}
	// Today's initial sample began on the preceding date; it also remains open
	// if the first response after its own midnight omits the date.
	now := started.AddDate(0, 0, 2)
	_, err = recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{}, now, now)
	contributionMust(t, err)
	if officialStat(t, "2026-11-21").Archived {
		t.Fatal("date initially sampled across midnight lost its pending close")
	}
	now = now.Add(time.Minute)
	_, err = recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{"2026-11-20": 7200, "2026-11-21": 3600}, now, now)
	contributionMust(t, err)
	for day, want := range map[string]float64{"2026-11-20": 7200, "2026-11-21": 3600} {
		if closed := officialStat(t, day); !closed.Archived || closed.CappedSeconds != want {
			t.Fatalf("post-midnight request did not close pending date: %+v", closed)
		}
	}
}

func TestDailyOneOClockCapsAtOneHourAndRepeatedRefreshPaysOnlyDifference(t *testing.T) {
	setupRetreatBackfillDB(t)
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, storage.DailyOperationsLocation)
	for i := 0; i < 2; i++ {
		_, err := recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{"2026-11-20": 100000}, at, at)
		contributionMust(t, err)
	}
	if stat := officialStat(t, "2026-11-20"); stat.CappedSeconds != 3600 || officialTotal(t) != 1 {
		t.Fatalf("invalid one-o-clock credit: %+v total=%v", stat, officialTotal(t))
	}
	_, err := recordOfficialDailyListeningStatsAt(1, "abs1", map[string]float64{"2026-11-20": 100000}, at.Add(time.Hour), at.Add(time.Hour))
	contributionMust(t, err)
	if officialTotal(t) != 2 {
		t.Fatalf("daily delta paid incorrectly: %v", officialTotal(t))
	}
}

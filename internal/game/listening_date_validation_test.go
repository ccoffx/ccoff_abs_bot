package game

import (
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestDailyListeningRecordsRejectInvalidAndFutureDates(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-03T10:00:00+08:00")
	records := buildOfficialDailyListeningRecords(1, "abs1", map[string]float64{
		"2026-11-02": 3600,
		"2026-11-04": 86400,
		"2026-02-30": 86400,
		"total":      86400,
	}, now, now)
	if len(records) != 2 {
		t.Fatalf("invalid/future dates persisted: %+v", records)
	}
	for _, record := range records {
		if record.DayKey != "2026-11-02" && record.DayKey != "2026-11-03" {
			t.Fatalf("invalid/future date persisted: %+v", record)
		}
	}
}

func TestFutureDailyListeningCannotAwardCultivationOrContribution(t *testing.T) {
	_, now, _ := setupListeningContributionTest(t)
	for _, day := range []string{SectDayKey(now.AddDate(0, 0, 1)), "invalid-date"} {
		if err := storage.DB.Create(&model.DailyListeningStat{
			UserID: 1, AbsUserID: "abs1", DayKey: day, RawSeconds: 86400, CappedSeconds: 86400,
			EffectiveHours: 8.5, SpiritFireHours: 100,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertListeningSync(t, now, 9.376)
	assertContribution(t, storage.DB, 6)
}

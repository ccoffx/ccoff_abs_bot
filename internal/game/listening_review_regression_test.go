package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestDailyAndReportSnapshotRollBackTogether(t *testing.T) {
	setupRetreatBackfillDB(t)
	start := time.Now().In(storage.DailyOperationsLocation).AddDate(0, 0, -2)
	day := SectDayKey(start)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/listening-stats") {
			_ = json.NewEncoder(w).Encode(map[string]any{"totalTime": 3600, "days": map[string]float64{day: 3600}})
			return
		}
		s := bossSession("atomic-snapshot", start, start.Add(time.Hour), 3600)
		s["date"] = day
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{s}})
	})
	contributionMust(t, storage.DB.Exec("CREATE TRIGGER reject_report_snapshot BEFORE INSERT ON listening_report_snapshots BEGIN SELECT RAISE(ABORT, 'injected snapshot failure'); END").Error)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
		t.Fatal("snapshot failure accepted")
	}
	var count int64
	contributionMust(t, storage.DB.Model(&model.DailyListeningStat{}).Count(&count).Error)
	if count != 0 {
		t.Fatalf("daily rows escaped failed snapshot transaction: %d", count)
	}
	contributionMust(t, storage.DB.Exec("DROP TRIGGER reject_report_snapshot").Error)
	if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); !ok {
		t.Fatal("retry failed")
	}
	var snapshot model.ListeningReportSnapshot
	contributionMust(t, storage.DB.First(&snapshot, "user_id=1").Error)
	if snapshot.RawTotalSeconds != 3600 || recoveryListeningDay(t, day).RawSeconds != 3600 {
		t.Fatal("retry did not persist the same snapshot")
	}
}

func TestSettledDeletedHistoryIsNotReloaded(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	assertListeningSync(t, now, 15.84)
	contributionMust(t, db.Delete(&stat).Error)
	assertListeningSync(t, now, 0)
	var loaded int64
	contributionMust(t, db.Callback().Query().After("gorm:query").Register("review_deleted_history", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			loaded += tx.RowsAffected
		}
	}))
	t.Cleanup(func() { db.Callback().Query().Remove("review_deleted_history") })
	assertListeningSync(t, now, 0)
	if loaded != 0 {
		t.Fatalf("settled deleted history reloaded %d rows", loaded)
	}
	// Restoring the row must still reapply exactly once.
	contributionMust(t, db.Unscoped().Model(&stat).UpdateColumn("deleted_at", nil).Error)
	assertListeningSync(t, now, 15.84)
	assertListeningSync(t, now, 15.84)
}

func TestStaleRealmSyncCannotOverwriteNewCultivation(t *testing.T) {
	for _, newerMinor := range []int{1, 9} {
		t.Run(fmt.Sprint(newerMinor), func(t *testing.T) {
			db, _, _ := setupListeningContributionTest(t)
			stale := model.Cultivation{UserID: 1, TotalAudioTime: 4, MajorRealm: 0, MinorRealm: 1}
			contributionMust(t, db.Create(&stale).Error)
			contributionMust(t, db.Model(&stale).Updates(map[string]any{"total_audio_time": 8.9, "minor_realm": newerMinor}).Error)
			// Restore the old in-memory read after a newer writer committed.
			stale.TotalAudioTime, stale.MinorRealm = 4, 1
			SyncCultivationRealm(&stale)
			var current model.Cultivation
			contributionMust(t, db.First(&current, stale.ID).Error)
			if current.MinorRealm != newerMinor || stale.MinorRealm != newerMinor || stale.TotalAudioTime != 8.9 {
				t.Fatalf("stale realm writer: stored=%+v returned=%+v", current, stale)
			}
		})
	}
}

func TestReportUsesOneDatabaseSnapshotDuringConcurrentSettlement(t *testing.T) {
	db, now, _ := setupListeningContributionTest(t)
	today := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now), RawSeconds: 3600, CappedSeconds: 3600, EffectiveHours: 1}
	contributionMust(t, db.Create(&today).Error)
	assertListeningSync(t, now, 16.84)
	contributionMust(t, saveListeningReportStats(1, "abs1", 40000, now))
	written := false
	contributionMust(t, db.Callback().Query().After("gorm:query").Register("review_report_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table != "listening_report_snapshots" || written {
			return
		}
		written = true
		// Commit a newer complete version between the report's reads.
		err := db.Transaction(func(writer *gorm.DB) error {
			if err := writer.Model(&today).Updates(map[string]any{
				"raw_seconds": 7200, "capped_seconds": 7200, "effective_hours": 2, "base_cultivation_hours": 2,
				"applied_effective_hours": 2, "applied_capped_seconds": 7200, "applied_base_hours": 2, "applied_total_hours": 2,
			}).Error; err != nil {
				return err
			}
			if err := writer.Model(&model.Cultivation{}).Where("user_id = 1").UpdateColumn("total_audio_time", 17.84).Error; err != nil {
				return err
			}
			return writer.Model(&model.ListeningReportSnapshot{}).Where("user_id = 1").UpdateColumn("raw_total_seconds", 50000).Error
		})
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(func() { db.Callback().Query().Remove("review_report_snapshot") })
	report := personalReportFromCache("abs1")
	for _, want := range []string{"累计实际听书: 11.11 小时", "净修炼时长: 16.84 小时", "今日净修为: 1.00 小时"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report mixed database versions (want %q): %s", want, report)
		}
	}
	if !written {
		t.Fatal("concurrent update was not exercised")
	}
}

func TestReportDoesNotDisplayUnsettledDailyComponents(t *testing.T) {
	db, now, _ := setupListeningContributionTest(t)
	assertListeningSync(t, now, 15.84)
	today := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now), RawSeconds: 3600, CappedSeconds: 3600, EffectiveHours: 1}
	contributionMust(t, db.Create(&today).Error)
	assertPending := func() {
		t.Helper()
		report := personalReportFromCache("abs1")
		if !strings.Contains(report, "今日净修为: 待结算") || strings.Contains(report, "今日基础净修为:") {
			t.Fatalf("unsettled daily components presented as current: %s", report)
		}
		var stat model.DailyListeningStat
		contributionMust(t, db.First(&stat, today.ID).Error)
		if detail := formatDailyListeningStat("cache", stat, 15.84); !strings.Contains(detail, "今日净修为：待结算") || strings.Contains(detail, "基础净修为：") {
			t.Fatalf("daily query displayed unapplied components: %s", detail)
		}
		panel, err := spiritFirePanel(1, now)
		contributionMust(t, err)
		if !strings.Contains(panel, "今日净修为：待结算") || strings.Contains(panel, "今日基础净修为：") {
			t.Fatalf("fire panel displayed unapplied components: %s", panel)
		}
	}
	assertPending()
	assertListeningSync(t, now, 16.84)
	if report := personalReportFromCache("abs1"); !strings.Contains(report, "今日净修为: 1.00 小时") {
		t.Fatal(report)
	}
	// A later source refresh can commit before settlement, or settlement can fail.
	contributionMust(t, db.Model(&today).Updates(map[string]any{"effective_hours": 2, "capped_seconds": 7200, "raw_seconds": 7200}).Error)
	assertPending()
	assertListeningSync(t, now, 17.84)
}

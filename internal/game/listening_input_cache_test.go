package game

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOfficialListeningSyncLoadsOnlyChangedHistory(t *testing.T) {
	db, now, original := setupListeningContributionTest(t)
	contributionMust(t, db.Delete(&original).Error)
	days := make(map[string]float64, 731)
	for i := 1; i <= 730; i++ {
		days[SectDayKey(now.AddDate(0, 0, -i))] = 3600
	}
	_, err := recordOfficialDailyListeningStats(1, "abs1", days, time.Now())
	contributionMust(t, err)
	before := officialTotal(t)
	loaded := int64(0)
	contributionMust(t, db.Callback().Query().After("gorm:query").Register("count_official_history", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			loaded += tx.RowsAffected
		}
	}))
	t.Cleanup(func() { db.Callback().Query().Remove("count_official_history") })
	_, err = recordOfficialDailyListeningStats(1, "abs1", days, time.Now())
	contributionMust(t, err)
	if loaded > 2 || officialTotal(t) != before {
		t.Fatalf("unchanged 730-day sync loaded %d days; total %g -> %g", loaded, before, officialTotal(t))
	}
	t.Logf("unchanged 730-day sync: %d daily rows loaded (today only)", loaded)
	loaded = 0
	corrected := SectDayKey(now.AddDate(0, 0, -300))
	days[corrected] = 7200
	_, err = recordOfficialDailyListeningStats(1, "abs1", days, time.Now())
	contributionMust(t, err)
	if loaded > 4 || math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatalf("archived historical correction loaded %d days; total %g -> %g", loaded, before, officialTotal(t))
	}
	t.Logf("archived historical correction: %d daily rows loaded (corrected day and today)", loaded)
	// Replaying the corrected snapshot must not load the corrected history again.
	loaded = 0
	_, err = recordOfficialDailyListeningStats(1, "abs1", days, time.Now())
	contributionMust(t, err)
	if loaded > 2 {
		t.Fatal("unchanged historical correction was loaded again", loaded)
	}
	// Reopen the database without migrating or rebuilding anything. The cache
	// and its source-invalidation guards must survive process-level state loss.
	reopened, err := gorm.Open(sqlite.Open(db.Dialector.(*sqlite.Dialector).DSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	contributionMust(t, err)
	pool, err := reopened.DB()
	contributionMust(t, err)
	defer pool.Close()
	prior := storage.DB
	storage.DB = reopened
	defer func() { storage.DB = prior }()
	loaded = 0
	contributionMust(t, reopened.Callback().Query().After("gorm:query").Register("count_reopened_history", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			loaded += tx.RowsAffected
		}
	}))
	_, err = recordOfficialDailyListeningStats(1, "abs1", days, time.Now())
	contributionMust(t, err)
	if loaded > 2 || math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatalf("restart loaded %d daily rows or changed credit", loaded)
	}
	// The secret-realm path must consume this incremental result, without an
	// additional full-history reconstruction after the official source write.
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{r.URL.Query().Get("day"): 0}})
	})
	loaded = 0
	snapshot, err := fetchSectSecretRealmListeningSnapshotFromABS("abs1")
	contributionMust(t, err)
	if loaded > 2 || math.Abs(snapshot.EffectiveHours-before) > 1e-9 {
		t.Fatalf("secret realm loaded %d daily rows; snapshot=%+v", loaded, snapshot)
	}
	t.Logf("secret realm with 730 historical days: %d daily rows loaded", loaded)
}

func TestOfficialListeningInputCacheInvalidatesOnDeletionAndTransfer(t *testing.T) {
	setupRetreatBackfillDB(t)
	db := storage.DB
	day := SectDayKey(time.Now().AddDate(0, 0, -3))
	days := map[string]float64{day: 3600}
	writeOfficial(t, days)
	before := officialTotal(t)
	s := officialStat(t, day)
	contributionMust(t, db.Delete(&s).Error)
	var snapshot model.ListeningReportSnapshot
	contributionMust(t, db.First(&snapshot, "user_id=1").Error)
	if snapshot.OfficialInputJSON != "" {
		t.Fatal("source deletion retained a stale input cache")
	}
	writeOfficial(t, days)
	if math.Abs(officialTotal(t)-before) > 1e-9 {
		t.Fatal("restored source double-counted historical credit", officialTotal(t), before)
	}
	var count int64
	contributionMust(t, db.Model(&model.DailyListeningStat{}).Where("user_id = 1 AND day_key = ?", day).Count(&count).Error)
	if count != 1 {
		t.Fatal("same input did not recreate deleted source", count)
	}
	s = officialStat(t, day)
	contributionMust(t, db.Model(&s).Update("user_id", 2).Error)
	contributionMust(t, db.First(&snapshot, "user_id=1").Error)
	if snapshot.OfficialInputJSON != "" {
		t.Fatal("ownership transfer retained a stale input cache")
	}
}

func TestOfficialListeningInputCacheRevisitsElapsedCapAcrossMidnight(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	day := SectDayKey(now.AddDate(0, 0, -1))
	start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
	contributionMust(t, err)
	s := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day,
		RawSeconds: 86400, OfficialRawSeconds: 86400, CappedSeconds: 3600, EffectiveHours: 1,
		BaseCultivationHours: 1, OfficialPolicy: true, OfficialBaselineKind: "native",
		CultivationApplied: true, AppliedEligible: true, AppliedCappedSeconds: 3600,
		AppliedEffectiveHours: 1, AppliedBaseHours: 1, AppliedTotalHours: 1,
		LastFetchedAt: start.Add(time.Hour)}
	contributionMust(t, storage.DB.Create(&s).Error)
	contributionMust(t, storage.DB.Create(&model.Cultivation{UserID: 1, OfficialDailyInitialized: true,
		OfficialDailyAbsUserID: "abs1", ListeningInitialized: true, TotalAudioTime: 1, ListeningBaseHours: 1}).Error)
	encoded, err := json.Marshal(officialListeningInputCache{Version: 1, Days: map[string]officialListeningInput{day: {Raw: 86400, Credit: 3600}}})
	contributionMust(t, err)
	contributionMust(t, storage.DB.Create(&model.ListeningReportSnapshot{UserID: 1, AbsUserID: "abs1",
		StatsFetchedAt: start.Add(time.Hour), OfficialInputJSON: string(encoded)}).Error)
	writeOfficial(t, map[string]float64{day: 86400})
	if math.Abs(officialTotal(t)-8.5) > 1e-9 {
		t.Fatal("unchanged raw input froze the previous day's elapsed cap", officialTotal(t))
	}
	writeOfficial(t, map[string]float64{day: 86400})
	if math.Abs(officialTotal(t)-8.5) > 1e-9 {
		t.Fatal("replayed midnight credit", officialTotal(t))
	}
}

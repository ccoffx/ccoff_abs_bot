package game

import (
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupListeningContributionTest(t *testing.T) (*gorm.DB, time.Time, model.DailyListeningStat) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "contribution.db")+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(4)
	old := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = old; pool.Close() })
	models := append(spiritfire.Models(), &model.User{}, &model.Cultivation{}, &model.DailyListeningStat{}, &model.SectCaveRetreat{}, &model.Sect{}, &model.SectMember{}, &model.SectContributionLog{}, &model.AuditLog{})
	contributionMust(t, db.AutoMigrate(models...))
	contributionMust(t, db.Exec("CREATE UNIQUE INDEX ux_contribution_cultivation ON cultivations(user_id)").Error)
	now := time.Now()
	contributionMust(t, spiritfire.Initialize(db, now.AddDate(0, 0, -3)))
	contributionMust(t, db.Create(&model.User{TelegramID: 1, Username: "listener", AbsUserID: "abs1"}).Error)
	contributionMust(t, db.Create(&model.Sect{Model: gorm.Model{ID: 1}, Name: "测试宗门"}).Error)
	contributionMust(t, db.Create(&model.SectMember{SectID: 1, UserID: 1}).Error)
	ids := []uint{}
	for id := uint(2); id <= 23; id++ {
		ids = append(ids, id)
	}
	stat := model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now.AddDate(0, 0, -1)), RawSeconds: 8 * 3600, CappedSeconds: 8 * 3600, EffectiveHours: 6.4}
	contributionMust(t, db.Create(&stat).Error)
	contributionMust(t, db.Create(&model.SpiritFireLoadout{UserID: 1, EffectiveDay: stat.DayKey, FireIDs: spiritfire.Encode(ids)}).Error)
	return db, now, stat
}
func contributionMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func assertContribution(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var m model.SectMember
	contributionMust(t, db.Where("user_id = 1").First(&m).Error)
	if m.Contribution != want || m.WeeklyContribution != want {
		t.Fatalf("member=%+v want=%d", m, want)
	}
}
func assertListeningSync(t *testing.T, now time.Time, want float64) {
	t.Helper()
	got, ok := syncCultivationFromDailyListeningStatsAt(1, now)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("sync=%v ok=%v want=%v", got, ok, want)
	}
}
func addContributionRetreat(t *testing.T, db *gorm.DB, day string) model.SectCaveRetreat {
	t.Helper()
	start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
	contributionMust(t, err)
	r := model.SectCaveRetreat{UserID: 1, StartAt: start.Add(4 * time.Hour), EndAt: start.Add(8 * time.Hour), BaseRawSeconds: 4 * 3600, Status: "expired"}
	r.ListeningEvidenceJSON = retreatTestEvidence(r.StartAt, r.EndAt, 4*3600)
	contributionMust(t, db.Create(&r).Error)
	return r
}

func TestSpiritFireRetreatAlwaysAddsFullBenefit(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	// Real retreat evidence must increase final cultivation at every rate,
	// including the previously broken >100% range.
	r := addContributionRetreat(t, db, stat.DayKey)
	evidence := r.ListeningEvidenceJSON
	for _, percent := range []int{0, 50, 100, 186, 250} {
		contributionMust(t, db.Model(&r).Update("listening_evidence_json", "").Error)
		contributionMust(t, db.Transaction(func(tx *gorm.DB) error {
			if err := spiritfire.Lock(tx); err != nil {
				return err
			}
			s, err := spiritfire.Snapshot(tx, 1, stat.DayKey)
			if err != nil {
				return err
			}
			return tx.Model(&s).Update("percent", percent).Error
		}))
		without, ok := totalCultivationEffectiveHoursFromDailyStats(1, now)
		if !ok {
			t.Fatal("failed without retreat")
		}
		var a model.DailyListeningStat
		contributionMust(t, db.First(&a, stat.ID).Error)
		contributionMust(t, db.Model(&r).Update("listening_evidence_json", evidence).Error)
		with, ok := totalCultivationEffectiveHoursFromDailyStats(1, now)
		if !ok {
			t.Fatal("failed with retreat")
		}
		var b model.DailyListeningStat
		contributionMust(t, db.First(&b, stat.ID).Error)
		if math.Abs(with-without-.8) > 1e-9 || a.SpiritFireHours != b.SpiritFireHours {
			t.Fatalf("percent=%d without=%g with=%g bonuses=%g/%g", percent, without, with, a.SpiritFireHours, b.SpiritFireHours)
		}
		if percent == 186 && (math.Abs(with-10.176) > 1e-9 || math.Abs(without-9.376) > 1e-9) {
			t.Fatal(without, with)
		}
	}
}

func TestListeningContributionCutoverRetreatAndHistoricalCorrection(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Create(&model.Cultivation{UserID: 1, TotalAudioTime: 9.376, MajorRealm: 2}).Error)
	contributionMust(t, db.Model(&model.SectMember{}).Where("user_id = 1").Updates(map[string]interface{}{"contribution": 42, "weekly_contribution": 42}).Error)
	contributionMust(t, InitializeSectListeningContributionProgress(db, now))
	assertListeningSync(t, now, 9.376)
	assertContribution(t, db, 42)
	// Retreat +0.8 increases eligible whole hours from 6 to 7, fire stays fixed.
	addContributionRetreat(t, db, stat.DayKey)
	assertListeningSync(t, now, 10.176)
	assertContribution(t, db, 43)
	assertListeningSync(t, now, 10.176)
	assertContribution(t, db, 43)
	contributionMust(t, db.Model(&stat).Updates(map[string]interface{}{"raw_seconds": 4 * 3600, "capped_seconds": 4 * 3600, "effective_hours": 4}).Error)
	assertListeningSync(t, now, 4)
	assertContribution(t, db, 43)
	contributionMust(t, db.Model(&stat).Updates(map[string]interface{}{"raw_seconds": 8 * 3600, "capped_seconds": 8 * 3600, "effective_hours": 6.4}).Error)
	assertListeningSync(t, now, 10.176)
	assertContribution(t, db, 43)
	contributionMust(t, db.Create(&model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now), RawSeconds: 2 * 3600, EffectiveHours: 2}).Error)
	assertListeningSync(t, now, 12.176)
	assertContribution(t, db, 45)
	// Restart baseline initialization must neither reset the cursor nor pay again.
	contributionMust(t, InitializeSectListeningContributionProgress(db, now))
	updateCultivationAudioHours(1, 99999) // An activity cannot pay based on this total.
	assertContribution(t, db, 45)
	var cul model.Cultivation
	contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
	if cul.SectListeningSettledHours == nil || math.Abs(*cul.SectListeningSettledHours-9.2) > 1e-9 || math.Abs(cul.TotalAudioTime-12.176) > 1e-9 {
		t.Fatal(cul)
	}
}

func TestListeningContributionConcurrentSyncAndNewProfile(t *testing.T) {
	db, now, _ := setupListeningContributionTest(t)
	// A newly created profile starts at zero, unlike an upgraded legacy profile.
	cul, err := createCultivationIfMissing(1)
	contributionMust(t, err)
	if cul.SectListeningSettledHours == nil || *cul.SectListeningSettledHours != 0 {
		t.Fatal(cul)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, ok := syncCultivationFromDailyListeningStatsAt(1, now); results <- ok }()
	}
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("concurrent sync failed")
		}
	}
	assertContribution(t, db, 6)
	var logs int64
	contributionMust(t, db.Model(&model.SectContributionLog{}).Where("ref_type = ?", "listening_hours").Count(&logs).Error)
	if logs != 1 {
		t.Fatal("duplicate payouts", logs)
	}
	var sect model.Sect
	contributionMust(t, db.First(&sect, 1).Error)
	if sect.Prestige != 6 {
		t.Fatal(sect)
	}
}

func TestListeningContributionFailureRollsBackAndRetries(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	_, err := createCultivationIfMissing(1)
	contributionMust(t, err)
	contributionMust(t, db.Exec("CREATE TRIGGER reject_listening_log BEFORE INSERT ON sect_contribution_logs BEGIN SELECT RAISE(ABORT, 'forced log failure'); END").Error)
	if _, ok := syncCultivationFromDailyListeningStatsAt(1, now); ok {
		t.Fatal("settlement ignored log failure")
	}
	assertContribution(t, db, 0)
	var cul model.Cultivation
	contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
	if cul.TotalAudioTime != 0 || cul.SectListeningSettledHours == nil || *cul.SectListeningSettledHours != 0 {
		t.Fatal("advanced failed cursor", cul)
	}
	var saved model.DailyListeningStat
	contributionMust(t, db.First(&saved, stat.ID).Error)
	if saved.SpiritFireHours != 0 {
		t.Fatal("failed transaction persisted fire hours", saved)
	}
	var sect model.Sect
	contributionMust(t, db.First(&sect, 1).Error)
	if sect.Prestige != 0 {
		t.Fatal(sect)
	}
	contributionMust(t, db.Exec("DROP TRIGGER reject_listening_log").Error)
	assertListeningSync(t, now, 9.376)
	assertContribution(t, db, 6)
	assertListeningSync(t, now, 9.376)
	assertContribution(t, db, 6)
}

func TestListeningContributionBaselineMissingHistoryAndReadFailure(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Delete(&stat).Error)
	contributionMust(t, db.Create(&model.Cultivation{UserID: 1, TotalAudioTime: 8.5}).Error)
	contributionMust(t, InitializeSectListeningContributionProgress(db, now))
	var cul model.Cultivation
	contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
	if cul.SectListeningSettledHours == nil || *cul.SectListeningSettledHours != 8.5 {
		t.Fatal("lost pre-statistics baseline", cul)
	}
	// A bad evidence record aborts the entire migration and leaves NULL for retry.
	contributionMust(t, db.Model(&cul).Update("sect_listening_settled_hours", nil).Error)
	contributionMust(t, db.Create(&model.SectCaveRetreat{UserID: 1, StartAt: now.Add(-time.Hour), EndAt: now, ListeningEvidenceJSON: "invalid"}).Error)
	if err := InitializeSectListeningContributionProgress(db, now); err == nil {
		t.Fatal("broken evidence silently initialized zero")
	}
	cul = model.Cultivation{}
	contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
	if cul.SectListeningSettledHours != nil || cul.TotalAudioTime != 8.5 {
		t.Fatal("failed migration changed profile", cul)
	}
}

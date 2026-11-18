package game

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Optional operational replay: always copies the supplied consistent SQLite
// snapshot into t.TempDir; it never opens the source database for writing.
func TestOfficialDailySnapshotReplay(t *testing.T) {
	source := os.Getenv("CCOFF_LISTENING_REPLAY_SNAPSHOT")
	if source == "" {
		t.Skip("requires a consistent offline database snapshot")
	}
	user, err := strconv.ParseInt(os.Getenv("CCOFF_LISTENING_REPLAY_USER"), 10, 64)
	contributionMust(t, err)
	input, err := os.Open(source)
	contributionMust(t, err)
	defer input.Close()
	path := filepath.Join(t.TempDir(), "replay.db")
	output, err := os.Create(path)
	contributionMust(t, err)
	_, err = io.Copy(output, input)
	contributionMust(t, err)
	contributionMust(t, output.Close())
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	contributionMust(t, err)
	pool, err := db.DB()
	contributionMust(t, err)
	defer pool.Close()
	prior := storage.DB
	storage.DB = db
	defer func() { storage.DB = prior }()
	contributionMust(t, db.AutoMigrate(&model.Cultivation{}, &model.DailyListeningStat{}))
	var profile model.Cultivation
	contributionMust(t, db.Where("user_id = ?", user).First(&profile).Error)
	before := profile.TotalAudioTime
	var rows []model.DailyListeningStat
	contributionMust(t, db.Where("user_id = ?", user).Find(&rows).Error)
	days := map[string]float64{}
	for _, r := range rows {
		if r.AbsUserID != "" {
			days[r.DayKey] = r.OfficialRawSeconds
		}
	}
	var account model.User
	contributionMust(t, db.Where("telegram_id = ?", user).First(&account).Error)
	_, err = recordOfficialDailyListeningStats(user, account.AbsUserID, days, time.Now(), nil)
	contributionMust(t, err)
	contributionMust(t, db.Where("user_id = ?", user).First(&profile).Error)
	if math.Abs(profile.TotalAudioTime-before) > 1e-9 {
		t.Fatalf("migration changed real balance: %.12f -> %.12f", before, profile.TotalAudioTime)
	}
	repaired := 0
	for _, r := range rows {
		if r.DayKey >= SectDayKey(time.Now()) {
			continue
		}
		preview, err := RepairOfficialListeningDay(user, r.DayKey, false, 0, 0, "")
		contributionMust(t, err)
		if preview.BaseDeltaHours <= 1e-9 {
			continue
		}
		t.Logf("repair preview day=%s base_delta=%.12f", r.DayKey, preview.BaseDeltaHours)
		_, err = RepairOfficialListeningDay(user, r.DayKey, true, preview.BaseDeltaHours, 1, "offline replay only")
		contributionMust(t, err)
		_, err = RepairOfficialListeningDay(user, r.DayKey, true, preview.BaseDeltaHours, 1, "duplicate replay")
		if err == nil {
			t.Fatal("duplicate repair accepted")
		}
		repaired++
	}
	contributionMust(t, db.Where("user_id = ?", user).First(&profile).Error)
	after := profile.TotalAudioTime
	_, err = recordOfficialDailyListeningStats(user, account.AbsUserID, days, time.Now(), nil)
	contributionMust(t, err)
	contributionMust(t, db.Where("user_id = ?", user).First(&profile).Error)
	if math.Abs(profile.TotalAudioTime-after) > 1e-9 {
		t.Fatal("post-repair refresh changed balance")
	}
	t.Logf("snapshot migration retained %.12f hours; repaired_days=%d; after_reviewed_repairs=%.12f", before, repaired, after)
}

func TestOfficialDailyRepairPreviewWorksOnReadOnlyDatabase(t *testing.T) {
	setupRetreatBackfillDB(t)
	day := SectDayKey(time.Now().AddDate(0, 0, -1))
	contributionMust(t, storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, RawSeconds: 7200, OfficialRawSeconds: 7200, CappedSeconds: 3600, EffectiveHours: 1, ValidationJSON: `{"version":2}`}).Error)
	writeOfficial(t, map[string]float64{day: 7200})
	var file struct{ File string }
	contributionMust(t, storage.DB.Raw("PRAGMA database_list").Scan(&file).Error)
	db, err := gorm.Open(sqlite.Open("file:"+file.File+"?mode=ro"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	contributionMust(t, err)
	pool, err := db.DB()
	contributionMust(t, err)
	defer pool.Close()
	prior := storage.DB
	storage.DB = db
	defer func() { storage.DB = prior }()
	preview, err := RepairOfficialListeningDay(1, day, false, 0, 0, "")
	contributionMust(t, err)
	if preview.BaseDeltaHours != 1 || preview.TotalDeltaHours != 1 || preview.AfterTotalHours != 2 {
		t.Fatal(preview)
	}
}

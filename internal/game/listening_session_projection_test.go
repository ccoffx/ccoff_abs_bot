package game

import (
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"log"
	"strings"
	"time"
)

// Session-derived day projections are test-only; production uses official daily totals.
func recordPreparedDailyListeningStats(userID int64, absUserID string, days map[string]float64, correctedDays map[string]bool, fetchedAt time.Time, validations ...*dailyListeningValidation) error {
	return recordPreparedDailyListeningStatsWithReport(userID, absUserID, days, correctedDays, fetchedAt, nil, validations...)
}

func recordPreparedDailyListeningStatsWithReport(userID int64, absUserID string, days map[string]float64, correctedDays map[string]bool, fetchedAt time.Time, reportTotal *float64, validations ...*dailyListeningValidation) error {
	if storage.DB == nil || userID == 0 {
		return nil
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}

	records := buildOfficialDailyListeningRecords(userID, absUserID, days, fetchedAt, time.Now())
	for index := range records {
		if correctedDays[records[index].DayKey] {
			records[index].Source = dailyListeningCrossDaySource
			records[index].FetchStatus = dailyListeningCrossDayStatus
			records[index].RefreshReason = "abs_cross_day_rebalance"
		}
		if len(validations) > 0 {
			applyDailyListeningValidation(&records[index], validations[0])
		}
	}
	if len(records) == 0 {
		return nil
	}

	var previous []model.DailyListeningStat
	var written []model.DailyListeningStat
	stale := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCurrentListeningBindingInTx(tx, userID, absUserID); err != nil {
			return err
		}
		dayKeys := make([]string, 0, len(records))
		for _, record := range records {
			dayKeys = append(dayKeys, record.DayKey)
		}
		if err := tx.Where("user_id = ? AND day_key IN ?", userID, dayKeys).Find(&previous).Error; err != nil {
			return err
		}
		// Compare instants in Go: SQLite text timestamps may have different offsets.
		for _, old := range previous {
			if old.AbsUserID == strings.TrimSpace(absUserID) && old.LastFetchedAt.After(fetchedAt) && !old.LastFetchedAt.After(time.Now()) {
				stale = true
				return nil
			}
		}
		oldByDay := make(map[string]model.DailyListeningStat, len(previous))
		for _, old := range previous {
			oldByDay[old.DayKey] = old
		}
		changed := make([]model.DailyListeningStat, 0, len(records))
		today := SectDayKey(time.Now())
		for _, record := range records {
			old, exists := oldByDay[record.DayKey]
			if exists && record.DayKey != today && sameDailyListeningInput(old, record) {
				continue
			}
			changed = append(changed, record)
		}
		if len(changed) > 0 {
			written = changed
			res := tx.Clauses(dailyListeningStatOnConflict(time.Now())).Create(&changed)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("DAILY_LISTENING_STATS_UPSERT_MISSED")
			}
		}
		if reportTotal != nil {
			return saveListeningReportStatsInTx(tx, userID, strings.TrimSpace(absUserID), *reportTotal, fetchedAt)
		}
		return nil
	})
	if err != nil {
		log.Printf("每日听书统计写入失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return err
	}
	if stale {
		written = records
	}
	logDailyListeningWrite(userID, absUserID, previous, written, fetchedAt, stale)
	if stale {
		return fmt.Errorf("DAILY_LISTENING_STATS_STALE_SNAPSHOT")
	}
	return nil
}

func dailyListeningStatOnConflict(now time.Time) clause.OnConflict {
	// daily_listening_stats uses a SQLite partial unique index, so the upsert
	// target must include the same deleted_at predicate.
	return clause.OnConflict{
		Columns: []clause.Column{
			{Name: "user_id"},
			{Name: "day_key"},
		},
		TargetWhere: clause.Where{Exprs: []clause.Expression{
			clause.Eq{Column: clause.Column{Name: "deleted_at"}, Value: nil},
		}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"official_raw_seconds": gorm.Expr("excluded.official_raw_seconds"),
			"validation_json":      gorm.Expr("excluded.validation_json"),
			"raw_seconds":          gorm.Expr("excluded.raw_seconds"),
			"capped_seconds":       gorm.Expr("excluded.capped_seconds"),
			"effective_hours":      gorm.Expr("excluded.effective_hours"),
			"abs_user_id":          gorm.Expr("excluded.abs_user_id"),
			"last_fetched_at":      gorm.Expr("excluded.last_fetched_at"),
			"source":               gorm.Expr("excluded.source"),
			"fetch_status":         gorm.Expr("excluded.fetch_status"),
			"refresh_reason":       gorm.Expr("excluded.refresh_reason"),
			"updated_at":           now,
			"deleted_at":           nil,
		}),
	}
}

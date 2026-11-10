package sectbonus

import (
	"errors"
	"math"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const Percent = 3

var location = time.FixedZone("CST", 8*3600)

func Models() []any {
	return []any{&model.SectOwnerBonusState{}, &model.SectOwnerBonusRevision{}}
}

func Initialize(db *gorm.DB, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		state := model.SectOwnerBonusState{ID: 1, LaunchDay: now.In(location).AddDate(0, 0, 1).Format("2006-01-02")}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		var sects []model.Sect
		if err := tx.Find(&sects).Error; err != nil {
			return err
		}
		for _, sect := range sects {
			if sect.OwnerID <= 0 {
				continue
			}
			if err := Schedule(tx, sect.OwnerID, true, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// Schedule changes entitlement at the next Beijing midnight. Last transfer of
// the day wins; no transfer rewrites a day that has already begun.
func Schedule(tx *gorm.DB, userID int64, enabled bool, now time.Time) error {
	r := model.SectOwnerBonusRevision{UserID: userID, DayKey: now.In(location).AddDate(0, 0, 1).Format("2006-01-02"), Enabled: enabled}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "day_key"}},
		DoUpdates: clause.Assignments(map[string]any{"enabled": enabled}),
	}).Create(&r).Error
}

// DailyBonus calculates entitlement without writing the listening record.
func DailyBonus(tx *gorm.DB, stat model.DailyListeningStat) (float64, error) {
	var r model.SectOwnerBonusRevision
	err := tx.Where("user_id = ? AND day_key <= ?", stat.UserID, stat.DayKey).Order("day_key DESC").First(&r).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	if err == nil && r.Enabled && !math.IsNaN(stat.EffectiveHours) && !math.IsInf(stat.EffectiveHours, 0) {
		return math.Max(0, stat.EffectiveHours) * Percent / 100, nil
	}
	return 0, nil
}

// RefreshDaily persists the bonus within the caller's locked listening transaction.
func RefreshDaily(tx *gorm.DB, stat *model.DailyListeningStat) error {
	bonus, err := DailyBonus(tx, *stat)
	if err != nil {
		return err
	}
	old := stat.SectOwnerBonusHours
	stat.SectOwnerBonusHours = bonus
	if stat.ID == 0 || old == bonus {
		return nil
	}
	return tx.Model(&model.DailyListeningStat{}).Where("id = ?", stat.ID).Update("sect_owner_bonus_hours", bonus).Error
}

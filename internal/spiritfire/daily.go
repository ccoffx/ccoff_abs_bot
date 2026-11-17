package spiritfire

import (
	"errors"
	"fmt"
	"math"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Bonus uses credited listening and ordinary suppressed hours, excluding
// retreat improvements. Fire and retreat rewards are added independently;
// the resulting cultivation is not limited to the ordinary daily cap.
func Bonus(creditedSeconds, base float64, percent int) float64 {
	if math.IsNaN(creditedSeconds) || math.IsInf(creditedSeconds, 0) || creditedSeconds < 0 || math.IsNaN(base) || math.IsInf(base, 0) || base < 0 || percent < 0 {
		return 0
	}
	creditedHours := math.Min(24, creditedSeconds/3600)
	return math.Max(0, creditedHours-base) * float64(percent) / 100
}

// Snapshot resolves historical loadout revisions, not current ownership. A
// transfer cannot rewrite yesterday, and late ABS corrections use the same rate.
func Snapshot(tx *gorm.DB, user int64, day string) (model.SpiritFireDaily, error) {
	s := model.SpiritFireDaily{UserID: user, DayKey: day, FireIDs: "[]"}
	var state model.SpiritFireState
	if err := tx.First(&state, 1).Error; err != nil {
		return s, err
	}
	if day < state.LaunchDay {
		return s, nil
	}
	err := tx.Where("user_id = ? AND day_key = ?", user, day).First(&s).Error
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return s, err
	}
	ids, err := LoadoutAt(tx, user, day)
	if err != nil {
		return s, err
	}
	rules, err := RulesAt(tx, day)
	if err != nil {
		return s, err
	}
	s.FireIDs = Encode(ids)
	seen := map[uint]bool{}
	for _, id := range ids {
		if id <= 1 || seen[id] {
			return s, fmt.Errorf("异火搭配快照异常")
		}
		seen[id] = true
		var f model.SpiritFire
		if err := tx.First(&f, id).Error; err != nil {
			return s, err
		}
		s.Percent += rules.Percents[f.Grade]
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&s).Error; err != nil {
		return s, err
	}
	err = tx.Where("user_id = ? AND day_key = ?", user, day).First(&s).Error
	return s, err
}

// RefreshDaily stores components independently. EffectiveHours retains its
// existing meaning (ordinary suppressed hours) for sect activity calculations.
func RefreshDaily(tx *gorm.DB, stat *model.DailyListeningStat, base float64, now time.Time) error {
	if stat.DayKey > Day(now) {
		return nil
	}
	snap, err := Snapshot(tx, stat.UserID, stat.DayKey)
	if err != nil {
		return err
	}
	return refreshDailyWithPercent(tx, stat, base, snap.Percent)
}

// RefreshDailyBatch reuses immutable daily snapshots in a single user's locked
// transaction. Only previously unseen dates need to resolve historical loadouts.
func RefreshDailyBatch(tx *gorm.DB, stats []model.DailyListeningStat, bases map[string]float64, now time.Time) error {
	if len(stats) == 0 {
		return nil
	}
	user := stats[0].UserID
	var state model.SpiritFireState
	if err := tx.First(&state, 1).Error; err != nil {
		return err
	}
	var snapshots []model.SpiritFireDaily
	days := make([]string, 0, len(stats))
	for _, stat := range stats {
		days = append(days, stat.DayKey)
	}
	if err := tx.Where("user_id = ? AND day_key IN ?", user, days).Find(&snapshots).Error; err != nil {
		return err
	}
	byDay := make(map[string]model.SpiritFireDaily, len(snapshots))
	for _, snap := range snapshots {
		byDay[snap.DayKey] = snap
	}
	today := Day(now)
	for i := range stats {
		stat := &stats[i]
		if stat.UserID != user {
			return fmt.Errorf("SPIRIT_FIRE_BATCH_USER_MISMATCH")
		}
		if stat.DayKey > today {
			continue
		}
		percent := 0
		if stat.DayKey >= state.LaunchDay {
			snap, ok := byDay[stat.DayKey]
			if !ok {
				var err error
				snap, err = Snapshot(tx, user, stat.DayKey)
				if err != nil {
					return err
				}
				byDay[stat.DayKey] = snap
			}
			percent = snap.Percent
		}
		if err := refreshDailyWithPercent(tx, stat, bases[stat.DayKey], percent); err != nil {
			return err
		}
	}
	return nil
}

func refreshDailyWithPercent(tx *gorm.DB, stat *model.DailyListeningStat, base float64, percent int) error {
	oldBase, oldBonus := stat.BaseCultivationHours, stat.SpiritFireHours
	stat.BaseCultivationHours = base
	stat.SpiritFireHours = Bonus(stat.CappedSeconds, stat.EffectiveHours, percent)
	if stat.ID == 0 || (oldBase == base && oldBonus == stat.SpiritFireHours) {
		return nil
	}
	return tx.Model(&model.DailyListeningStat{}).Where("id = ?", stat.ID).Updates(map[string]any{"base_cultivation_hours": base, "spirit_fire_hours": stat.SpiritFireHours}).Error
}

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

// Bonus uses ordinary suppressed hours, excluding retreat improvements.
// Retreat rewards are added independently and must never reduce fire income.
func Bonus(rawSeconds, base float64, percent int) float64 {
	if math.IsNaN(rawSeconds) || math.IsInf(rawSeconds, 0) || rawSeconds < 0 || math.IsNaN(base) || math.IsInf(base, 0) || base < 0 || percent < 0 {
		return 0
	}
	raw := math.Min(24, rawSeconds/3600)
	return math.Max(0, raw-base) * float64(percent) / 100
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
	oldBase, oldBonus := stat.BaseCultivationHours, stat.SpiritFireHours
	stat.BaseCultivationHours = base
	stat.SpiritFireHours = Bonus(stat.RawSeconds, stat.EffectiveHours, snap.Percent)
	if stat.ID == 0 || (oldBase == base && oldBonus == stat.SpiritFireHours) {
		return nil
	}
	return tx.Model(&model.DailyListeningStat{}).Where("id = ?", stat.ID).Updates(map[string]any{"base_cultivation_hours": base, "spirit_fire_hours": stat.SpiritFireHours}).Error
}

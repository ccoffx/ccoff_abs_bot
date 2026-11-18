package game

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
	"ccoff-abs-bot/internal/spiritfire"
	"gorm.io/gorm"
)

// settleListeningDaysInTx applies only changed daily amounts to the profile.
// The applied columns, contribution cursor and profile commit atomically.
func settleListeningDaysInTx(tx *gorm.DB, cul *model.Cultivation, now time.Time) (float64, float64, string, error) {
	var retreats []model.SectCaveRetreat
	if err := tx.Where("user_id = ? AND start_at <= ?", cul.UserID, now).Order("id").Find(&retreats).Error; err != nil {
		return 0, 0, "", err
	}
	encoded, err := json.Marshal(retreats)
	if err != nil {
		return 0, 0, "", err
	}
	version := fmt.Sprintf("%x", sha256.Sum256(encoded))
	today := SectDayKey(now)
	query := tx.Unscoped().Where("user_id = ?", cul.UserID)
	if cul.ListeningInitialized {
		affected := tx.Where("cultivation_applied = ? OR applied_effective_hours <> effective_hours OR applied_capped_seconds <> capped_seconds OR day_key = ?", false, today).
			Or("applied_eligible = ? AND deleted_at IS NULL AND day_key <= ?", false, today).
			Or("applied_eligible = ? AND (applied_base_hours <> base_cultivation_hours OR applied_total_hours <> base_cultivation_hours + spirit_fire_hours + sect_owner_bonus_hours)", true).
			Or("deleted_at IS NOT NULL AND (applied_total_hours <> 0 OR applied_base_hours <> 0)").
			Or("day_key > ? AND (applied_total_hours <> 0 OR applied_base_hours <> 0)", today)
		if version != cul.ListeningRetreatVersion {
			affected = affected.Or("applied_base_hours <> applied_effective_hours")
			for _, r := range retreats {
				affected = affected.Or("day_key >= ? AND day_key <= ?", SectDayKey(r.StartAt), SectDayKey(r.EndAt))
			}
		}
		query = query.Where(affected)
	}
	var stats []model.DailyListeningStat
	if err := query.Find(&stats).Error; err != nil {
		return 0, 0, "", err
	}
	bases := make(map[string]float64, len(stats))
	active := make([]model.DailyListeningStat, 0, len(stats))
	for _, s := range stats {
		if !s.DeletedAt.Valid && validDailyListeningDayKey(s.DayKey) && s.DayKey <= today {
			bases[s.DayKey] = s.EffectiveHours + dailySectCaveRetreatBonusHours(s, retreats)
			active = append(active, s)
		}
	}
	if err := spiritfire.RefreshDailyBatch(tx, active, bases, now); err != nil {
		return 0, 0, "", err
	}
	if err := sectbonus.RefreshDailyBatch(tx, active); err != nil {
		return 0, 0, "", err
	}
	byID := make(map[uint]model.DailyListeningStat, len(active))
	for _, s := range active {
		byID[s.ID] = s
	}
	total, base := cul.TotalAudioTime, cul.ListeningBaseHours
	if !cul.ListeningInitialized {
		total, base = 0, 0
	}
	for _, old := range stats {
		s, valid := byID[old.ID]
		nextBase, nextTotal := 0.0, 0.0
		if valid {
			nextBase = s.BaseCultivationHours
			nextTotal = nextBase + s.SpiritFireHours + s.SectOwnerBonusHours
		}
		deltaTotal, deltaBase := nextTotal, nextBase
		if cul.ListeningInitialized && old.CultivationApplied {
			deltaTotal -= old.AppliedTotalHours
			deltaBase -= old.AppliedBaseHours
		}
		total += deltaTotal
		base += deltaBase
		if old.CultivationApplied && old.AppliedEligible == valid && old.AppliedBaseHours == nextBase && old.AppliedTotalHours == nextTotal && old.AppliedEffectiveHours == old.EffectiveHours && old.AppliedCappedSeconds == old.CappedSeconds {
			continue
		}
		if err := tx.Unscoped().Model(&model.DailyListeningStat{}).Where("id = ?", old.ID).UpdateColumns(map[string]any{
			"applied_eligible": valid, "cultivation_applied": true, "applied_base_hours": nextBase, "applied_total_hours": nextTotal,
			"applied_effective_hours": old.EffectiveHours, "applied_capped_seconds": old.CappedSeconds,
		}).Error; err != nil {
			return 0, 0, "", err
		}
	}
	return math.Max(0, total), math.Max(0, base), version, nil
}

func recordListeningUpgradeInTx(tx *gorm.DB, cul *model.Cultivation, now time.Time) error {
	oldMajor, oldMinor := cul.MajorRealm, cul.MinorRealm
	oldKey := oldMajor*100 + oldMinor
	calculateCultivationRealm(cul)
	nextKey := cul.MajorRealm*100 + cul.MinorRealm
	// Bootstrap adopts the current realm without replaying historical rewards.
	if !cul.ListeningInitialized {
		cul.ListeningRewardRealm = max(oldKey, nextKey)
		return nil
	}
	cul.ListeningRewardRealm = max(cul.ListeningRewardRealm, oldKey)
	if nextKey <= cul.ListeningRewardRealm {
		return nil
	}
	event := model.CultivationUpgradeEvent{UserID: cul.UserID, FromMajor: oldMajor, FromMinor: oldMinor, ToMajor: cul.MajorRealm, ToMinor: cul.MinorRealm, CreatedAt: now}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	cul.ListeningRewardRealm = nextKey
	return nil
}

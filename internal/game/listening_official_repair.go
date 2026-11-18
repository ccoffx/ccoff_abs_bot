package game

import (
	"fmt"
	"math"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

// OfficialListeningRepair is a reviewable adjustment of one migrated day. It
// restores only the legacy policy offset, never re-imports ABS account history.
type OfficialListeningRepair struct {
	UserID               int64
	Day                  string
	BaselineSeconds      float64
	BeforeEffectiveHours float64
	AfterEffectiveHours  float64
	BaseDeltaHours       float64
	BeforeTotalHours     float64
	AfterTotalHours      float64
	TotalDeltaHours      float64
}

// RepairOfficialListeningDay previews by default. Applying requires a reason
// and the exact previewed delta, so a changed source must be reviewed again.
// A repeated apply with the original nonzero delta fails rather than paying twice.
func RepairOfficialListeningDay(userID int64, day string, apply bool, expectedDelta float64, actorID int64, reason string) (OfficialListeningRepair, error) {
	result := OfficialListeningRepair{UserID: userID, Day: day}
	if storage.DB == nil || !validDailyListeningDayKey(day) || day >= SectDayKey(time.Now()) {
		return result, fmt.Errorf("repair requires a historical day")
	}
	if apply && (actorID == 0 || strings.TrimSpace(reason) == "" || expectedDelta < 0 || math.IsNaN(expectedDelta) || math.IsInf(expectedDelta, 0)) {
		return result, fmt.Errorf("repair requires actor, reason and nonnegative expected delta")
	}
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if apply {
			if err := spiritfire.Lock(tx); err != nil {
				return err
			}
		}
		var user model.User
		if err := tx.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		var stat model.DailyListeningStat
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND day_key = ?", userID, user.AbsUserID, day).First(&stat).Error; err != nil {
			return err
		}
		if !stat.OfficialPolicy {
			return fmt.Errorf("refresh official daily policy before repair")
		}
		// Account history intentionally excluded by binding rules is not a debt.
		// Unknown pre-marker rows require provenance review, never inference from amounts.
		if stat.OfficialBaselineKind != "legacy_migration" {
			return fmt.Errorf("repair requires a verified legacy migration baseline; baseline kind=%q", stat.OfficialBaselineKind)
		}
		targetBaseline := math.Max(stat.OfficialBaselineHours, calculateSectEffectiveHoursFromSeconds(stat.OfficialBaselineSeconds))
		delta := math.Max(0, targetBaseline-stat.OfficialBaselineHours)
		result.BaselineSeconds, result.BeforeEffectiveHours = stat.OfficialBaselineSeconds, stat.EffectiveHours
		result.AfterEffectiveHours, result.BaseDeltaHours = stat.EffectiveHours+delta, delta
		if !DailyListeningCultivationSettled(stat) {
			return fmt.Errorf("settle daily source before repair")
		}
		var before model.Cultivation
		if err := tx.Where("user_id = ?", userID).First(&before).Error; err != nil {
			return err
		}
		target := stat
		target.EffectiveHours += delta
		target.OfficialBaselineCreditedSeconds = stat.OfficialBaselineSeconds
		var retreats []model.SectCaveRetreat
		if err := tx.Where("user_id = ? AND start_at <= ?", userID, time.Now()).Find(&retreats).Error; err != nil {
			return err
		}
		for _, retreat := range retreats {
			if _, err := decodeSectRetreatEvidence(retreat); err != nil {
				return err
			}
		}
		base := target.EffectiveHours + dailySectCaveRetreatBonusHours(target, retreats)
		var state model.SpiritFireState
		if err := tx.First(&state, 1).Error; err != nil {
			return err
		}
		fire := 0.0
		if day >= state.LaunchDay {
			var snapshot model.SpiritFireDaily
			if err := tx.Where("user_id = ? AND day_key = ?", userID, day).First(&snapshot).Error; err != nil {
				return fmt.Errorf("refresh bonus snapshot before repair: %w", err)
			}
			fire = spiritfire.Bonus(target.BonusListeningSeconds(), target.EffectiveHours, snapshot.Percent)
		}
		owner, err := sectbonus.DailyBonus(tx, target)
		if err != nil {
			return err
		}
		result.BeforeTotalHours = before.TotalAudioTime
		result.TotalDeltaHours = base + fire + owner - stat.AppliedTotalHours
		result.AfterTotalHours = before.TotalAudioTime + result.TotalDeltaHours
		if result.TotalDeltaHours < -1e-9 {
			return fmt.Errorf("repair would reduce total cultivation; manual review required")
		}
		if !apply {
			return nil
		}
		if delta <= 1e-9 && result.TotalDeltaHours <= 1e-9 {
			return fmt.Errorf("nothing to repair")
		}
		if math.Abs(delta-expectedDelta) > 1e-9 {
			return fmt.Errorf("repair preview changed: expected %.12f, current %.12f", expectedDelta, delta)
		}
		if err := lockCurrentListeningBindingInTx(tx, userID, user.AbsUserID); err != nil {
			return err
		}
		if err := tx.Model(&stat).Updates(map[string]any{"official_baseline_hours": targetBaseline, "official_baseline_credited_seconds": stat.OfficialBaselineSeconds, "effective_hours": stat.EffectiveHours + delta, "base_cultivation_hours": base, "spirit_fire_hours": fire, "sect_owner_bonus_hours": owner}).Error; err != nil {
			return err
		}
		after, err := settleCultivationListeningInTx(tx, userID, time.Now())
		if err != nil {
			return err
		}
		if math.Abs(after.TotalAudioTime-result.AfterTotalHours) > 1e-9 {
			return fmt.Errorf("repair affected other unsettled days; refresh and preview again")
		}
		return tx.Create(&model.AuditLog{ActorID: actorID, ActorRole: "operator", Action: "REPAIR_OFFICIAL_LISTENING_DAY", Target: fmt.Sprintf("user:%d/day:%s", userID, day), Detail: fmt.Sprintf("reason=%s; baseline_seconds=%.9f; effective_hours=%.12f->%.12f; total_hours=%.12f->%.12f", reason, stat.OfficialBaselineSeconds, result.BeforeEffectiveHours, result.AfterEffectiveHours, before.TotalAudioTime, after.TotalAudioTime)}).Error
	})
	return result, err
}

package game

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

// Ordinary cultivation deliberately trusts ABS calendar totals. Session evidence
// remains independent, for abuse and retreat windows only.
func recordOfficialDailyListeningStats(userID int64, absID string, days map[string]float64, fetchedAt time.Time, reportTotal *float64) (map[string]float64, error) {
	if storage.DB == nil || days == nil || userID == 0 || strings.TrimSpace(absID) == "" {
		return nil, fmt.Errorf("ABS_DAILY_NOT_READY")
	}
	absID = strings.TrimSpace(absID)
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}
	now := time.Now()
	if fetchedAt.After(now) {
		return nil, fmt.Errorf("ABS_DAILY_FUTURE_SNAPSHOT")
	}
	for day, seconds := range days {
		if !validDailyListeningDayKey(day) || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return nil, fmt.Errorf("ABS_DAILY_INVALID_VALUE: %s", day)
		}
	}
	records := buildOfficialDailyListeningRecords(userID, absID, days, fetchedAt, now)
	credited := make(map[string]float64)
	var previous, written []model.DailyListeningStat
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Match the settlement lock order; source, profile, rewards and metadata are
		// committed together. Readers never see a successful uncredited snapshot.
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		if err := lockCurrentListeningBindingInTx(tx, userID, absID); err != nil {
			return err
		}
		var profile model.Cultivation
		profileErr := tx.Where("user_id = ?", userID).First(&profile).Error
		if profileErr != nil && profileErr != gorm.ErrRecordNotFound {
			return profileErr
		}
		migrating := profileErr == nil && (!profile.OfficialDailyInitialized || profile.OfficialDailyAbsUserID != absID) && profile.ListeningInitialized
		baselineBefore := profile.OfficialDailyBaselineBefore
		if migrating {
			baselineBefore = SectDayKey(now)
		}
		if baselineBefore != "" && !validDailyListeningDayKey(baselineBefore) {
			return fmt.Errorf("ABS_DAILY_INVALID_MIGRATION_BOUNDARY")
		}
		var snapshot model.ListeningReportSnapshot
		err := tx.First(&snapshot, "user_id = ?", userID).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == nil && snapshot.AbsUserID == absID && snapshot.StatsFetchedAt.After(fetchedAt) && !snapshot.StatsFetchedAt.After(now) {
			return fmt.Errorf("DAILY_LISTENING_STATS_STALE_SNAPSHOT")
		}
		if err := tx.Where("user_id = ?", userID).Find(&previous).Error; err != nil {
			return err
		}
		oldByDay := make(map[string]model.DailyListeningStat, len(previous))
		for _, old := range previous {
			if old.AbsUserID == absID && old.LastFetchedAt.After(fetchedAt) && !old.LastFetchedAt.After(now) {
				return fmt.Errorf("DAILY_LISTENING_STATS_STALE_SNAPSHOT")
			}
			oldByDay[old.DayKey] = old
			credited[old.DayKey] = old.CappedSeconds
		}
		for _, record := range records {
			old, exists := oldByDay[record.DayKey]
			mergeOfficialDailyRecord(&record, old, exists, now)
			if !exists && baselineBefore != "" && record.DayKey < baselineBefore {
				// A previously unseen historical source day can overlap a legacy
				// physically allocated day, even if omitted from the first snapshot.
				// The persisted cutoff must not advance on ordinary refreshes.
				record.OfficialBaselineKind = "historical_excluded"
				record.OfficialBaselineSeconds = record.CappedSeconds
				record.OfficialBaselineHours, record.EffectiveHours = 0, 0
			}
			credited[record.DayKey] = record.CappedSeconds
			if exists && record.DayKey != SectDayKey(now) && sameDailyListeningInput(old, record) && old.OfficialPolicy {
				continue
			}
			if exists {
				// Explicit column selection includes zeros, while retaining settlement and
				// historical bonus columns until the shared settlement below updates them.
				record.ID = old.ID
				if err := tx.Model(&old).Select("RawSeconds", "OfficialRawSeconds", "CappedSeconds", "EffectiveHours", "ValidationJSON", "AbsUserID", "LastFetchedAt", "Source", "FetchStatus", "RefreshReason", "OfficialPolicy", "OfficialBaselineKind", "OfficialBaselineSeconds", "OfficialBaselineHours", "OfficialBaselineCreditedSeconds").Updates(&record).Error; err != nil {
					return err
				}
			} else if err := tx.Create(&record).Error; err != nil {
				return err
			}
			written = append(written, record)
		}
		if _, err := settleCultivationListeningInTx(tx, userID, now); err != nil {
			return err
		}
		if err := tx.Model(&model.Cultivation{}).Where("user_id = ?", userID).Updates(map[string]any{"official_daily_initialized": true, "official_daily_abs_user_id": absID, "official_daily_baseline_before": baselineBefore}).Error; err != nil {
			return err
		}
		total := 0.0
		if reportTotal != nil {
			total = *reportTotal
		} else {
			for _, seconds := range days {
				total += seconds
			}
		}
		return saveListeningReportStatsInTx(tx, userID, absID, total, fetchedAt)
	})
	if err != nil {
		return nil, err
	}
	logDailyListeningWrite(userID, absID, previous, written, fetchedAt, false)
	return credited, nil
}

func mergeOfficialDailyRecord(record *model.DailyListeningStat, old model.DailyListeningStat, exists bool, now time.Time) {
	record.OfficialPolicy = true
	record.OfficialBaselineKind = "native"
	record.Source, record.FetchStatus, record.RefreshReason = "abs_days", "ok", "abs_official_daily"
	if exists {
		record.OfficialBaselineKind = old.OfficialBaselineKind
		record.OfficialBaselineSeconds, record.OfficialBaselineHours = old.OfficialBaselineSeconds, old.OfficialBaselineHours
		record.OfficialBaselineCreditedSeconds = old.OfficialBaselineCreditedSeconds
		rebound := old.AbsUserID != "" && old.AbsUserID != record.AbsUserID
		if rebound {
			record.OfficialBaselineKind = "rebind"
			// Carry earned cultivation and its bonus basis, not the previous
			// account's playback high-water mark. Only growth beyond this new
			// account's first observation is eligible after rebinding.
			record.OfficialBaselineSeconds = record.CappedSeconds
			record.OfficialBaselineHours = old.EffectiveHours
			record.OfficialBaselineCreditedSeconds = old.BonusListeningSeconds()
		} else if !old.OfficialPolicy || old.AbsUserID != record.AbsUserID {
			if !old.OfficialPolicy {
				record.OfficialBaselineKind = "legacy_migration"
			}
			// Freeze the existing balance, not the newly returned ABS amount. Growth
			// since the last legacy snapshot can still earn its normal daily delta.
			baseline := math.Max(old.CappedSeconds, old.OfficialRawSeconds)
			if old.ValidationJSON == "" {
				baseline = math.Max(baseline, old.RawSeconds)
			}
			record.OfficialBaselineSeconds = math.Min(baseline, dailyListeningElapsedLimit(record.DayKey, now))
			record.OfficialBaselineHours = old.EffectiveHours
			record.OfficialBaselineCreditedSeconds = old.CappedSeconds
		}
		if !rebound {
			record.CappedSeconds = math.Max(record.CappedSeconds, math.Max(old.CappedSeconds, record.OfficialBaselineSeconds))
		}
	}
	// The carried balance and new account growth share the Telegram user's
	// daily allowance, including across repeated account changes.
	record.EffectiveHours = math.Min(sectDailyCultivationHourCap, math.Max(0, record.OfficialBaselineHours+(calculateSectEffectiveHoursFromSeconds(record.CappedSeconds)-calculateSectEffectiveHoursFromSeconds(record.OfficialBaselineSeconds))))
	detail := dailyListeningValidationDetail{Version: 3, OfficialSeconds: record.OfficialRawSeconds, AdjustedSeconds: record.RawSeconds, CreditedSeconds: record.CappedSeconds, ElapsedLimitSeconds: dailyListeningElapsedLimit(record.DayKey, now), Anomalies: map[string]int{}}
	if record.OfficialRawSeconds < record.CappedSeconds {
		detail.Anomalies["official_decrease_retained"] = 1
	}
	if record.OfficialRawSeconds > detail.ElapsedLimitSeconds {
		detail.Anomalies["daily_elapsed_limit"] = 1
	}
	data, _ := json.Marshal(detail)
	record.ValidationJSON = string(data)
}

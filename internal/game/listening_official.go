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

type officialListeningStats struct {
	Days map[string]float64
}

// Decode all ordinary daily inputs consistently before any caller can write
// them. JSON null is unknown data, not a numeric correction to zero.
func parseOfficialListeningStats(body []byte) (officialListeningStats, error) {
	var wire struct {
		Days map[string]json.RawMessage `json:"days"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return officialListeningStats{}, err
	}
	if wire.Days == nil {
		return officialListeningStats{}, fmt.Errorf("ABS_LISTENING_DAYS_MISSING")
	}
	stats := officialListeningStats{Days: make(map[string]float64, len(wire.Days))}
	for day, value := range wire.Days {
		if strings.TrimSpace(string(value)) == "null" {
			return officialListeningStats{}, fmt.Errorf("ABS_DAILY_NULL_VALUE: %s", day)
		}
		var seconds float64
		if err := json.Unmarshal(value, &seconds); err != nil {
			return officialListeningStats{}, fmt.Errorf("ABS_DAILY_INVALID_VALUE: %s: %w", day, err)
		}
		if !validDailyListeningDayKey(day) || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return officialListeningStats{}, fmt.Errorf("ABS_DAILY_INVALID_VALUE: %s", day)
		}
		stats.Days[day] = seconds
	}
	return stats, nil
}

type officialListeningInput struct {
	Raw    float64 `json:"r"`
	Credit float64 `json:"c"`
}

type officialListeningInputCache struct {
	Version int                               `json:"v"`
	Days    map[string]officialListeningInput `json:"days"`
}

// Ordinary cultivation deliberately trusts ABS calendar totals. Session evidence
// remains independent, for abuse and retreat windows only.
func recordOfficialDailyListeningStats(userID int64, absID string, days map[string]float64, fetchedAt time.Time) (map[string]float64, error) {
	return recordOfficialDailyListeningStatsAt(userID, absID, days, fetchedAt, time.Now())
}

func recordOfficialDailyListeningStatsAt(userID int64, absID string, days map[string]float64, fetchedAt time.Time, now time.Time) (map[string]float64, error) {
	if storage.DB == nil || days == nil || userID == 0 || strings.TrimSpace(absID) == "" {
		return nil, fmt.Errorf("ABS_DAILY_NOT_READY")
	}
	absID = strings.TrimSpace(absID)
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}
	if fetchedAt.After(now) {
		return nil, fmt.Errorf("ABS_DAILY_FUTURE_SNAPSHOT")
	}
	for day, seconds := range days {
		if !validDailyListeningDayKey(day) || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return nil, fmt.Errorf("ABS_DAILY_INVALID_VALUE: %s", day)
		}
	}
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
		cache := officialListeningInputCache{}
		cacheOK := profile.OfficialDailyInitialized && profile.OfficialDailyAbsUserID == absID &&
			snapshot.AbsUserID == absID && !snapshot.StatsFetchedAt.IsZero() && !snapshot.StatsFetchedAt.After(now) &&
			json.Unmarshal([]byte(snapshot.OfficialInputJSON), &cache) == nil && cache.Version == 1 && cache.Days != nil
		if !cacheOK {
			cache = officialListeningInputCache{Version: 1, Days: make(map[string]officialListeningInput)}
			// A legacy/imported source can be newer than the report metadata. Check
			// for it without loading every day's full record. Valid caches are already
			// guarded by source-invalidation triggers and the report timestamp above.
			var stale int
			if err := tx.Model(&model.DailyListeningStat{}).Select("1").
				Where("user_id = ? AND abs_user_id = ? AND julianday(last_fetched_at) > julianday(?) AND julianday(last_fetched_at) <= julianday(?)", userID, absID, fetchedAt, now).
				Limit(1).Scan(&stale).Error; err != nil {
				return err
			}
			if stale != 0 {
				return fmt.Errorf("DAILY_LISTENING_STATS_STALE_SNAPSHOT")
			}
			// Cache invalidation or restart must not reopen archived history.
			var saved []model.DailyListeningStat
			if err := tx.Where("user_id = ?", userID).Find(&saved).Error; err != nil {
				return err
			}
			for _, stat := range saved {
				cache.Days[stat.DayKey] = officialListeningInput{Raw: stat.OfficialRawSeconds, Credit: stat.CappedSeconds}
			}
		}
		for day, input := range cache.Days {
			credited[day] = input.Credit
		}
		changed := make(map[string]float64)
		today := SectDayKey(now)
		lastDay := SectDayKey(snapshot.StatsFetchedAt)
		var openDays []model.DailyListeningStat
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND day_key < ? AND archived = ?", userID, absID, today, false).Find(&openDays).Error; err != nil {
			return err
		}
		open := make(map[string]bool, len(openDays))
		var archiveIDs []uint
		for _, stat := range openDays {
			// The row itself retains the pending close across missing date keys,
			// later report snapshots and cache rebuilds. Do not advance its sample
			// time or archive it until ABS actually returns that date.
			if !stat.LastFetchedAt.IsZero() && SectDayKey(stat.LastFetchedAt) <= stat.DayKey {
				open[stat.DayKey] = true
				continue
			}
			// On upgrade, preserve and freeze history already sampled after its day.
			archiveIDs = append(archiveIDs, stat.ID)
			if !stat.OfficialPolicy {
				changed[stat.DayKey] = stat.OfficialRawSeconds
			}
		}
		for day, raw := range days {
			if day > today {
				continue
			}
			old, exists := cache.Days[day]
			if day < today {
				// Existing history is immutable after its first closing snapshot.
				// Unobserved offline/bootstrap days can be imported once.
				if exists && !open[day] {
					continue
				}
			}
			// Revisit the previous fetch date across midnight: its elapsed-time cap
			// may have increased even if ABS reports the same raw amount.
			if !cacheOK || !exists || old.Raw != raw || day >= lastDay || open[day] {
				changed[day] = raw
			}
		}
		changed[today] = days[today]
		records := buildOfficialDailyListeningRecords(userID, absID, changed, fetchedAt, now)
		keys := make([]string, 0, len(records))
		for _, record := range records {
			keys = append(keys, record.DayKey)
		}
		// Bound SQL parameters during bootstrap and large historical corrections.
		for start := 0; start < len(keys); start += 400 {
			var batch []model.DailyListeningStat
			if err := tx.Where("user_id = ? AND day_key IN ?", userID, keys[start:min(start+400, len(keys))]).Find(&batch).Error; err != nil {
				return err
			}
			previous = append(previous, batch...)
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
			if exists && old.Archived {
				continue
			}
			mergeOfficialDailyRecord(&record, old, exists, now)
			// A request started before midnight may finish after it. Such a sample
			// still needs a request begun after the date ended before it can close.
			record.Archived = record.DayKey < SectDayKey(fetchedAt)
			if !exists && baselineBefore != "" && record.DayKey < baselineBefore {
				// A previously unseen historical source day can overlap a legacy
				// physically allocated day, even if omitted from the first snapshot.
				// The persisted cutoff must not advance on ordinary refreshes.
				record.OfficialBaselineKind = "historical_excluded"
				record.OfficialBaselineSeconds = record.CappedSeconds
				record.OfficialBaselineHours, record.EffectiveHours = 0, 0
			}
			credited[record.DayKey] = record.CappedSeconds
			cache.Days[record.DayKey] = officialListeningInput{Raw: record.OfficialRawSeconds, Credit: record.CappedSeconds}
			if exists && record.DayKey != today && sameDailyListeningInput(old, record) && old.OfficialPolicy && old.Archived == record.Archived {
				continue
			}
			if exists {
				// Explicit column selection includes zeros, while retaining settlement and
				// historical bonus columns until the shared settlement below updates them.
				record.ID = old.ID
				if err := tx.Model(&old).Select("Archived", "RawSeconds", "OfficialRawSeconds", "CappedSeconds", "EffectiveHours", "ValidationJSON", "AbsUserID", "LastFetchedAt", "Source", "FetchStatus", "RefreshReason", "OfficialPolicy", "OfficialBaselineKind", "OfficialBaselineSeconds", "OfficialBaselineHours", "OfficialBaselineCreditedSeconds").Updates(&record).Error; err != nil {
					return err
				}
			} else if err := tx.Create(&record).Error; err != nil {
				return err
			}
			written = append(written, record)
		}
		for start := 0; start < len(archiveIDs); start += 400 {
			if err := tx.Model(&model.DailyListeningStat{}).Where("id IN ?", archiveIDs[start:min(start+400, len(archiveIDs))]).UpdateColumn("archived", true).Error; err != nil {
				return err
			}
		}
		if _, err := settleCultivationListeningInTx(tx, userID, now); err != nil {
			return err
		}
		if err := tx.Model(&model.Cultivation{}).Where("user_id = ?", userID).Updates(map[string]any{"official_daily_initialized": true, "official_daily_abs_user_id": absID, "official_daily_baseline_before": baselineBefore}).Error; err != nil {
			return err
		}
		inputJSON, err := json.Marshal(cache)
		if err != nil {
			return err
		}
		return saveListeningReportStatsInTx(tx, userID, absID, fetchedAt, string(inputJSON))
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
			record.OfficialBaselineSeconds = baseline
			record.OfficialBaselineHours = old.EffectiveHours
			record.OfficialBaselineCreditedSeconds = old.CappedSeconds
		}
		if !rebound {
			record.CappedSeconds = math.Max(record.CappedSeconds, math.Max(old.CappedSeconds, record.OfficialBaselineSeconds))
		}
	}
	// A retained high-water mark or legacy balance cannot bypass today's clock
	// limit (for example a bad pre-upgrade snapshot shortly after midnight).
	record.CappedSeconds = math.Min(record.CappedSeconds, dailyListeningElapsedLimit(record.DayKey, now))
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

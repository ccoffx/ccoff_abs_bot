package game

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var listeningDayCloseMu sync.Mutex

const listeningDayCloseSeedKey = "listening_day_close_seed_day"

// Seed missing dates once and retain failed requests across later midnights.
func seedListeningDayCloses(now time.Time) error {
	today := sectStartOfDay(now)
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET id = id WHERE id = (SELECT MIN(id) FROM users)").Error; err != nil {
			return err
		}
		var state model.SystemConfig
		err := tx.Where("key = ?", listeningDayCloseSeedKey).First(&state).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		first := today.AddDate(0, 0, -1)
		if state.Value != "" {
			last, err := time.ParseInLocation(time.DateOnly, state.Value, storage.DailyOperationsLocation)
			if err != nil {
				return err
			}
			first = last.AddDate(0, 0, 1)
		}
		var users []model.User
		if err := tx.Where("TRIM(abs_user_id) <> '' AND status <> ?", "unbound").Find(&users).Error; err != nil {
			return err
		}
		for day := first; day.Before(today); day = day.AddDate(0, 0, 1) {
			for _, user := range users {
				if !user.CreatedAt.IsZero() && !user.CreatedAt.Before(day.AddDate(0, 0, 1)) {
					continue
				}
				if err := enqueueListeningDayClose(tx, user.TelegramID, user.AbsUserID, SectDayKey(day)); err != nil {
					return err
				}
			}
		}
		var open []model.DailyListeningStat
		if err := tx.Joins("JOIN users ON users.telegram_id = daily_listening_stats.user_id AND users.abs_user_id = daily_listening_stats.abs_user_id AND users.deleted_at IS NULL").
			Where("daily_listening_stats.day_key < ? AND daily_listening_stats.archived = ? AND daily_listening_stats.official_policy = ?", SectDayKey(today), false, true).Find(&open).Error; err != nil {
			return err
		}
		for _, row := range open {
			if err := enqueueListeningDayClose(tx, row.UserID, row.AbsUserID, row.DayKey); err != nil {
				return err
			}
		}
		if first.Before(today) {
			return storage.UpsertSystemConfigValueInTx(tx, listeningDayCloseSeedKey, SectDayKey(today.AddDate(0, 0, -1)))
		}
		return nil
	})
}

func enqueueListeningDayClose(tx *gorm.DB, userID int64, absID, day string) error {
	var archived int64
	if err := tx.Model(&model.DailyListeningStat{}).Where("user_id = ? AND abs_user_id = ? AND day_key = ? AND archived = ?", userID, absID, day, true).Count(&archived).Error; err != nil {
		return err
	}
	status := "pending"
	if archived > 0 {
		status = "completed"
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ListeningDayClose{UserID: userID, AbsUserID: absID, DayKey: day, Status: status}).Error
}

func ClosePreviousListeningDay(now time.Time) error {
	if storage.DB == nil || abs.DefaultClient == nil {
		return errors.New("LISTENING_DAY_CLOSE_NOT_READY")
	}
	if !listeningDayCloseMu.TryLock() {
		return nil
	}
	defer listeningDayCloseMu.Unlock()
	if err := seedListeningDayCloses(now); err != nil {
		return err
	}
	var pending []model.ListeningDayClose
	if err := storage.DB.Where("status = ?", "pending").Order("day_key, id").Find(&pending).Error; err != nil {
		return err
	}
	failures := runListeningSyncBatch(len(pending), func(i int) error {
		entry := pending[i]
		if err := closeListeningDay(entry, entry.DayKey); err != nil {
			return fmt.Errorf("user=%d day=%s: %w", entry.UserID, entry.DayKey, err)
		}
		return nil
	})
	return errors.Join(failures...)
}

func closeListeningDay(entry model.ListeningDayClose, day string) error {
	listeningRefreshMu.RLock()
	defer listeningRefreshMu.RUnlock()
	var bound int64
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ? AND abs_user_id = ? AND status <> ?", entry.UserID, entry.AbsUserID, "unbound").Count(&bound).Error; err != nil {
		return err
	}
	if bound == 0 {
		return storage.DB.Model(&entry).Where("user_id = ? AND abs_user_id = ? AND status = ?", entry.UserID, entry.AbsUserID, "pending").Update("status", "cancelled").Error
	}
	if !acquireListeningSyncSlot() {
		return errors.New("LISTENING_DAY_CLOSE_CAPACITY")
	}
	defer releaseListeningSyncSlot()
	fetchedAt := time.Now()
	if day >= SectDayKey(fetchedAt) {
		return errors.New("LISTENING_DAY_CLOSE_NOT_PAST")
	}
	body, status, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningDaysPath(entry.AbsUserID, day), nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("LISTENING_DAY_CLOSE_STATUS_%d", status)
	}
	raw, err := abs.ParseListeningDay(body, day)
	if err != nil {
		return err
	}
	now := time.Now()
	record := newOfficialDailyListeningRecord(entry.UserID, entry.AbsUserID, day, raw, fetchedAt, now)
	record.Archived = true
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		if err := lockCurrentListeningBindingInTx(tx, entry.UserID, entry.AbsUserID); err != nil {
			return err
		}
		var old model.DailyListeningStat
		var request model.ListeningDayClose
		if err := tx.First(&request, entry.ID).Error; err != nil {
			return err
		}
		if request.Status != "pending" {
			return nil
		}
		err := tx.Where("user_id = ? AND day_key = ?", entry.UserID, day).First(&old).Error
		exists := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if exists && (old.Archived || strings.TrimSpace(old.AbsUserID) != entry.AbsUserID) {
			return tx.Model(&request).Update("status", "completed").Error
		}
		if old.LastFetchedAt.After(fetchedAt) && !old.LastFetchedAt.After(now) {
			return errors.New("LISTENING_DAY_CLOSE_STALE")
		}
		mergeOfficialDailyRecord(&record, old, exists, now)
		if !exists {
			var profile model.Cultivation
			err := tx.Where("user_id = ?", entry.UserID).First(&profile).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if profile.OfficialDailyBaselineBefore != "" && day < profile.OfficialDailyBaselineBefore {
				record.OfficialBaselineKind = "historical_excluded"
				record.OfficialBaselineSeconds = record.CappedSeconds
				record.EffectiveHours = 0
			}
		}
		record.RefreshReason = "abs_day_close"
		if exists {
			record.ID = old.ID
			err = updateOfficialDailyRecordInTx(tx, &record)
		} else {
			err = tx.Create(&record).Error
		}
		if err != nil {
			return err
		}
		// The input cache is invalidated by the daily-row update. The report's
		// current-day fetch time belongs to the independent ordinary refresh.
		if _, err = settleCultivationListeningInTx(tx, entry.UserID, now); err != nil {
			return err
		}
		return tx.Model(&request).Update("status", "completed").Error
	})
	if errors.Is(err, ErrListeningBindingChanged) {
		return storage.DB.Model(&entry).Where("user_id = ? AND abs_user_id = ? AND status = ?", entry.UserID, entry.AbsUserID, "pending").Update("status", "cancelled").Error
	}
	return err
}

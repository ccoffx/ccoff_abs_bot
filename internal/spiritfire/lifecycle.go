package spiritfire

import (
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/gorm"
)

// SweepOwner must only run after a successful current ABS refresh for this
// owner. Missing local accounts may be released immediately without a refresh.
func SweepOwner(db *gorm.DB, user int64, refreshed bool, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		var u model.User
		err := tx.Where("telegram_id = ?", user).First(&u).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReleaseUserInTx(tx, user, now)
		}
		if err != nil {
			return err
		}
		if !refreshed {
			return nil
		}
		// A successful refresh with no day records is not proof of inactivity.
		var fresh model.DailyListeningStat
		err = tx.Where("user_id = ? AND abs_user_id = ?", user, u.AbsUserID).Order("last_fetched_at DESC").First(&fresh).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if fresh.LastFetchedAt.Before(now.Add(-24 * time.Hour)) {
			return nil
		}

		var stats []model.DailyListeningStat
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND raw_seconds >= 600 AND day_key <= ?", user, u.AbsUserID, Day(now)).Order("day_key DESC").Find(&stats).Error; err != nil {
			return err
		}
		var last time.Time
		if len(stats) > 0 {
			last, err = time.ParseInLocation("2006-01-02", stats[0].DayKey, Location)
			if err != nil {
				return err
			}
		}
		var fs []model.SpiritFire
		if err := tx.Where("owner_id = ?", user).Find(&fs).Error; err != nil {
			return err
		}
		for _, f := range fs {
			active := last
			if f.AcquiredAt.After(active) {
				active = f.AcquiredAt
			}
			if now.Before(active.AddDate(0, 0, 30)) {
				if f.GraceAt != nil {
					if err := tx.Model(&f).Update("grace_at", nil).Error; err != nil {
						return err
					}
				}
				continue
			}
			if f.GraceAt == nil {
				if err := tx.Model(&f).Update("grace_at", now).Error; err != nil {
					return err
				}
				if err := tx.Create(&model.SpiritFireNotice{UserID: user, Text: fmt.Sprintf("🔥 【%s】已连续30天无达标听书，进入7天宽限期。请在%s前完成单日不少于10分钟有效听书，否则异火无偿回归天地。", f.Name, now.AddDate(0, 0, 7).In(Location).Format("2006-01-02 15:04"))}).Error; err != nil {
					return err
				}
				if err := Audit(tx, 0, "GRACE", f.ID, fmt.Sprint(user)); err != nil {
					return err
				}
			} else if !now.Before(f.GraceAt.AddDate(0, 0, 7)) {
				if err := ReleaseInTx(tx, f, now, "inactive"); err != nil {
					return err
				}
				if err := tx.Create(&model.SpiritFireNotice{UserID: user, Text: "🔥 【" + f.Name + "】因宽限期内未恢复达标听书，已回归天地；永久掌控记录保留。"}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

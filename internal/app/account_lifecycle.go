package app

import (
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func claimAccountLifecycle(id uint, now time.Time) (model.User, string, error) {
	var u model.User
	kind := ""
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Acquire SQLite's writer lock before reading eligibility. A renewal
		// either commits before this read or is rejected by the durable guard.
		if err := tx.Exec("UPDATE users SET id = id WHERE id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.First(&u, id).Error; err != nil {
			return err
		}
		if storage.IsSuperAdminUser(u) {
			// Configuration may have granted immunity since an earlier failed run.
			return tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind IN ?", id, []string{"suspend", "delete"}).Error
		}
		var pending model.AccountLifecycleClaim
		err := tx.First(&pending, "user_row_id = ?", id).Error
		if err == nil {
			kind = pending.Kind
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if u.IsWhitelist || u.AbsUserID == "" || u.ExpireAt == nil {
			return nil
		}
		if u.ExpireAt.After(now) {
			return nil
		}
		if !u.IsSuspended {
			kind = "suspend"
		} else if !u.ExpireAt.AddDate(0, 0, config.AppConfig.AccountGraceDays).After(now) {
			kind = "delete"
		}
		if kind == "" {
			return nil
		}
		return tx.Create(&model.AccountLifecycleClaim{UserRowID: id, Kind: kind, CreatedAt: now}).Error
	})
	return u, kind, err
}

func runAccountLifecycleAt(bot *tgbotapi.BotAPI, now time.Time) error {
	var users []model.User
	if err := storage.DB.Where("abs_user_id <> '' OR EXISTS (SELECT 1 FROM account_lifecycle_claims WHERE user_row_id = users.id)").Find(&users).Error; err != nil {
		return err
	}
	var failures []error
	for _, snapshot := range users {
		u, kind, err := claimAccountLifecycle(snapshot.ID, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("user %d claim: %w", snapshot.TelegramID, err))
			continue
		}
		if kind == "" {
			if storage.IsSuperAdminUser(u) || u.IsWhitelist || u.AbsUserID == "" || u.ExpireAt == nil || !u.ExpireAt.After(now) {
				continue
			}
			days := int(u.ExpireAt.Sub(now).Hours() / 24)
			if days >= config.AppConfig.NotifyBeforeDays {
				continue
			}
			day := storage.DailyOperationsDateKey(now)
			var count int64
			if err := storage.DB.Model(&model.AccountLifecycleNotice{}).Where("user_row_id = ? AND day = ?", u.ID, day).Count(&count).Error; err != nil {
				failures = append(failures, err)
				continue
			}
			if count > 0 {
				continue
			}
			if err := telegram.ReplyTextChecked(bot, u.TelegramID, fmt.Sprintf("⚠️ **账号到期预警**\n\n您的听书账号将在 `%d` 天后到期，请及时续期。", days+1)); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := storage.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.AccountLifecycleNotice{UserRowID: u.ID, Day: day}).Error; err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if kind == lifecycle.ListeningAbuseReleaseOperation {
			// The abuse monitor resumes the active freeze record, including after restart.
			continue
		}
		if kind == "reactivate" {
			if err := account.ReactivateExpiredAccount(u.TelegramID); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if kind == "delete" {
			var claim model.AccountLifecycleClaim
			if err := storage.DB.First(&claim, "user_row_id = ? AND kind = ?", u.ID, "delete").Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					failures = append(failures, err)
				}
				continue
			}
			if err := completeAccountDeletion(u, claim); err != nil {
				failures = append(failures, fmt.Errorf("user %d delete: %w", u.TelegramID, err))
				continue
			}
			message := "🗑 **账户已注销**\n\n账号已超过数据保留期限，账户及听书进度已永久删除。"
			if claim.AuditAction != "" {
				message = "🗑 **账户已注销**\n\n此前确认的账号删除已完成。"
			}
			telegram.ReplyText(bot, u.TelegramID, message)
			continue
		}
		if abs.DefaultClient == nil {
			failures = append(failures, fmt.Errorf("ABS client unavailable"))
			continue
		}
		switch kind {
		case "suspend":
			err = abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, false)
		default:
			err = fmt.Errorf("unknown lifecycle operation %q", kind)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("user %d %s: %w", u.TelegramID, kind, err))
			continue
		}
		err = storage.DB.Transaction(func(tx *gorm.DB) error {
			// Removing the guard and finalizing local state are one commit. If
			// audit/cleanup fails the claim remains for restart-safe ABS retries.
			if err := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ?", u.ID, kind).Error; err != nil {
				return err
			}
			return applySuspendLocalStatusWithAuditDB(tx, 0, u.TelegramID, u.AbsUserID, true, "AUTO_SUSPEND_EXPIRED_USER", "expired lifecycle suspend")
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("user %d finalize: %w", u.TelegramID, err))
			continue
		}
		telegram.ReplyText(bot, u.TelegramID, fmt.Sprintf("⛔ **账号已暂停使用**\n\n账号已到期，请使用续期卡恢复权限。数据保留 `%d` 天，宽限期结束后将永久删除。", config.AppConfig.AccountGraceDays))
	}
	return errors.Join(failures...)
}

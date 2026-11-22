package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

type adminStatusIntent struct {
	AbsID     string
	Suspended bool
	Reason    string
}

func prepareAdminStatusChange(actorID, targetID int64, absID string, suspended bool, action, reason string) error {
	intent := adminStatusIntent{absID, suspended, reason}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	kind := "admin_suspend"
	if !suspended {
		kind = "admin_resume"
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE users SET id = id WHERE telegram_id = ?", targetID).Error; err != nil {
			return err
		}
		var user model.User
		if err := tx.First(&user, "telegram_id = ?", targetID).Error; err != nil {
			return err
		}
		if storage.IsSuperAdminUser(user) {
			return storage.ErrTargetIsSuperAdmin
		}
		if absID == "" {
			return storage.ErrAbsUserIDEmpty
		}
		if user.AbsUserID != absID {
			return fmt.Errorf("target_abs_user_changed")
		}
		var claim model.AccountLifecycleClaim
		err := tx.First(&claim, "user_row_id = ?", user.ID).Error
		if err == nil {
			if claim.Kind == kind && claim.ActorID == actorID && claim.AuditAction == action && claim.AuditDetail == string(encoded) {
				return nil
			}
			return fmt.Errorf("账号生命周期操作处理中，请稍后重试")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&model.AccountLifecycleClaim{UserRowID: user.ID, Kind: kind, ActorID: actorID, AuditAction: action, AuditDetail: string(encoded), CreatedAt: time.Now()}).Error
	})
}

// The durable claim blocks rebinding, renewal and competing status changes;
// SQLite's writer is released for the entire external request.
func completeAdminStatusChange(targetID int64) (completed *model.AccountLifecycleClaim, err error) {
	err = lifecycle.TryAccountOperation(targetID, func() error {
		var user model.User
		if err := storage.DB.First(&user, "telegram_id = ?", targetID).Error; err != nil {
			return err
		}
		var claim model.AccountLifecycleClaim
		if err := storage.DB.Where("user_row_id = ? AND kind IN ?", user.ID, []string{"admin_suspend", "admin_resume"}).First(&claim).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		// Persist scheduling before the request, including malformed/permanently
		// failing intents. A restart must not let the same first batch starve others.
		now := time.Now()
		next := now.Add(adminStatusRetryDelay(claim.RetryAttempts))
		res := storage.DB.Model(&model.AccountLifecycleClaim{}).
			Where("user_row_id = ? AND kind = ? AND audit_detail = ?", user.ID, claim.Kind, claim.AuditDetail).
			Updates(map[string]any{"last_attempt_at": now, "next_retry_at": next, "retry_attempts": gorm.Expr("retry_attempts + 1")})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("ADMIN_STATUS_CLAIM_CHANGED")
		}
		var intent adminStatusIntent
		if err := json.Unmarshal([]byte(claim.AuditDetail), &intent); err != nil {
			return err
		}
		if intent.AbsID != user.AbsUserID || storage.IsSuperAdminUser(user) {
			return fmt.Errorf("管理员账号操作目标已变化")
		}
		if abs.DefaultClient == nil {
			return fmt.Errorf("ABS_CLIENT_UNAVAILABLE")
		}
		if err := abs.DefaultClient.SetUserActiveStatus(intent.AbsID, !intent.Suspended); err != nil {
			return err
		}
		err := storage.DB.Transaction(func(tx *gorm.DB) error {
			res := tx.Delete(&model.AccountLifecycleClaim{}, "user_row_id = ? AND kind = ? AND audit_detail = ?", user.ID, claim.Kind, claim.AuditDetail)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("ADMIN_STATUS_CLAIM_CHANGED")
			}
			return applySuspendLocalStatusWithAuditDB(tx, claim.ActorID, targetID, intent.AbsID, intent.Suspended, claim.AuditAction, intent.Reason)
		})
		if err == nil {
			completed = &claim
		}
		return err
	})
	return completed, err
}
func submitAdminStatusChange(bot *tgbotapi.BotAPI, targetID int64, reportFailure bool) {
	background.Submit(fmt.Sprintf("admin_status:%d", targetID), func() {
		claim, err := completeAdminStatusChange(targetID)
		if err != nil {
			log.Printf("管理员账号状态后台同步失败，将重试: target=%d err=%s", targetID, textfmt.FormatPlainError(err))
			if reportFailure {
				var user model.User
				var pending model.AccountLifecycleClaim
				if storage.DB.First(&user, "telegram_id = ?", targetID).Error == nil && storage.DB.First(&pending, "user_row_id = ?", user.ID).Error == nil {
					telegram.ReplyText(bot, pending.ActorID, "⏳ 账号权限同步暂未完成，后台将重试，请勿重复操作该账号。")
				}
			}
			return
		}
		if claim != nil {
			telegram.ReplyText(bot, claim.ActorID, fmt.Sprintf("✅ 用户 `%d` 的账号权限已同步更新。", targetID))
		}
	})
}

// Back off from one minute to thirty minutes; never stop retrying durable work.
func adminStatusRetryDelay(attempts int) time.Duration {
	return min(time.Minute*time.Duration(1<<min(max(attempts, 0), 5)), 30*time.Minute)
}

func dueAdminStatusUsers(now time.Time) ([]model.User, error) {
	var users []model.User
	err := storage.DB.Joins("JOIN account_lifecycle_claims c ON c.user_row_id = users.id").
		Where("c.kind IN ? AND (c.next_retry_at IS NULL OR c.next_retry_at <= ?)", []string{"admin_suspend", "admin_resume"}, now).
		Order("c.last_attempt_at ASC, c.created_at ASC, c.user_row_id ASC").Limit(20).Find(&users).Error
	return users, err
}

func retryAdminStatusChanges(bot *tgbotapi.BotAPI) {
	users, err := dueAdminStatusUsers(time.Now())
	if err != nil {
		log.Printf("管理员账号状态重试扫描失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, user := range users {
		submitAdminStatusChange(bot, user.TelegramID, false)
	}
}

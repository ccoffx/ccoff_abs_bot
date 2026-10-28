package account

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

type exchangeInviteUseMode string

const (
	ExchangeInviteUseNone     exchangeInviteUseMode = ""
	ExchangeInviteUseTrial    exchangeInviteUseMode = "trial"
	ExchangeInviteUseRegister exchangeInviteUseMode = "register"
)

func DetermineExchangeInviteUseMode(userID int64) (exchangeInviteUseMode, error) {
	var u model.User
	err := storage.DB.Where("telegram_id = ?", userID).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ExchangeInviteUseRegister, nil
	}
	if err != nil {
		return ExchangeInviteUseNone, err
	}
	if storage.IsTrialAccount(u) {
		return ExchangeInviteUseTrial, nil
	}
	if strings.TrimSpace(u.AbsUserID) == "" {
		return ExchangeInviteUseRegister, nil
	}
	return ExchangeInviteUseNone, nil
}

type renewRedeemResult struct {
	Days           int
	NewExpireAt    time.Time
	AbsUserID      string
	NeedReactivate bool
}

func RedeemRenewCodeByHash(userID int64, renewHash string) (renewRedeemResult, error) {
	var result renewRedeemResult
	if strings.TrimSpace(renewHash) == "" {
		return result, storage.ErrSecurityPepperNotConfigured
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var rCode model.RenewCode
		if err := tx.Where("code_hash = ? AND is_used = ?", renewHash, false).First(&rCode).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrInvalidRenewCode
			}
			return err
		}

		if err := storage.CheckCodeOwnership(rCode.Source, rCode.OwnerUserID, userID); err != nil {
			return err
		}

		var u model.User
		if err := tx.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if storage.IsTrialAccount(u) {
			return storage.ErrTrialCannotUseRenewCode
		}

		res := tx.Model(&model.RenewCode{}).
			Where("id = ? AND is_used = ?", rCode.ID, false).
			Updates(map[string]interface{}{
				"is_used":    true,
				"used_by_id": userID,
			})

		if res.Error != nil {
			return res.Error
		}

		if res.RowsAffected == 0 {
			return storage.ErrInvalidRenewCode
		}

		now := time.Now()
		if u.ExpireAt == nil || u.ExpireAt.Before(now) {
			result.NewExpireAt = now.AddDate(0, 0, rCode.Days)
		} else {
			result.NewExpireAt = u.ExpireAt.AddDate(0, 0, rCode.Days)
		}

		userRes := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND account_type <> ?", u.ID, userID, storage.AccountTypeTrial).
			Update("expire_at", result.NewExpireAt)
		if userRes.Error != nil {
			return userRes.Error
		}
		if userRes.RowsAffected == 0 {
			return fmt.Errorf("RENEW_USER_STATE_CHANGED")
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"USE_RENEW_CODE",
			fmt.Sprintf("renew_code_id=%d", rCode.ID),
			0,
			fmt.Sprintf("user %s(%d) used renew code %s for %d days; expire_at=%s; need_reactivate=%t",
				textfmt.FormatPlainValue(u.Username), userID, textfmt.FormatPlainValue(rCode.CodePreview), rCode.Days, result.NewExpireAt.Format(time.RFC3339), u.IsSuspended && u.AbsUserID != ""),
		); err != nil {
			return err
		}

		result.Days = rCode.Days
		result.AbsUserID = u.AbsUserID
		result.NeedReactivate = u.IsSuspended && u.AbsUserID != ""

		return nil
	})
	if err != nil {
		return renewRedeemResult{}, err
	}
	return result, nil
}

func SendRenewRedeemResult(bot *tgbotapi.BotAPI, chatID int64, userID int64, result renewRedeemResult) {
	if result.NeedReactivate {
		if err := abs.DefaultClient.SetUserActiveStatus(result.AbsUserID, true); err != nil {
			log.Printf("⚠️ 续期后 ABS 解封失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(result.AbsUserID), textfmt.FormatPlainError(err))
			auditErr := storage.WriteAuditLogInTx(storage.DB, userID, "RENEW_REACTIVATE_USER_FAILED", fmt.Sprintf("%d", userID), 0,
				fmt.Sprintf("renew card extended account but ABS reactivation failed: tg=%d abs_user_id=%s expire_at=%s days=%d error=%s",
					userID, textfmt.FormatPlainValue(result.AbsUserID), result.NewExpireAt.Format(time.RFC3339), result.Days, textfmt.FormatPlainError(err)))
			if auditErr != nil {
				log.Printf("⚠️ 续期 ABS 解封失败审计写入失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(result.AbsUserID), textfmt.FormatPlainError(auditErr))
				telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 用户续期已到账，但 ABS 恢复失败，且失败审计写入失败。\n用户：%d\nABS：%s\n到期：%s\n天数：%d\nABS错误：%s\n审计错误：%s", userID, textfmt.FormatPlainValue(result.AbsUserID), result.NewExpireAt.Format(time.RFC3339), result.Days, textfmt.FormatPlainError(err), textfmt.FormatPlainError(auditErr)))
			}

			telegram.ReplyText(bot, chatID, fmt.Sprintf(
				"⚠️ 续期已到账，新的到期时间为 `%s`。\n\nABS 解封暂时失败，系统已记录异常，请联系管理员处理。",
				result.NewExpireAt.Format("2006-01-02"),
			))
			return
		}

		if err := storage.ApplyRenewReactivateLocalStatusWithAudit(userID, result.AbsUserID, result.NewExpireAt, result.Days); err != nil {
			log.Printf("⚠️ ABS 已解封，但本地解除封禁状态或审计写入失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(result.AbsUserID), textfmt.FormatPlainError(err))
			auditErr := storage.WriteAuditLogInTx(storage.DB, userID, "RENEW_REACTIVATE_USER_LOCAL_FAILED", fmt.Sprintf("%d", userID), 0,
				fmt.Sprintf("renew card reactivated ABS but local state/audit failed: tg=%d abs_user_id=%s expire_at=%s days=%d error=%s",
					userID, textfmt.FormatPlainValue(result.AbsUserID), result.NewExpireAt.Format(time.RFC3339), result.Days, textfmt.FormatPlainError(err)))
			if auditErr != nil {
				log.Printf("⚠️ 续期 ABS 已解封，但本地失败审计写入失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(result.AbsUserID), textfmt.FormatPlainError(auditErr))
				telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 用户续期已到账且 ABS 已恢复，但本地权限状态或成功审计失败，且本地失败审计写入失败。\n用户：%d\nABS：%s\n到期：%s\n天数：%d\n本地错误：%s\n审计错误：%s\n请立即人工核查。", userID, textfmt.FormatPlainValue(result.AbsUserID), result.NewExpireAt.Format(time.RFC3339), result.Days, textfmt.FormatPlainError(err), textfmt.FormatPlainError(auditErr)))
				telegram.ReplyText(bot, chatID, fmt.Sprintf(
					"⚠️ 续期已到账，新的到期时间为 `%s`。\n\nABS 已恢复，但本地权限状态和失败审计写入均异常，已通知管理员人工核查。",
					result.NewExpireAt.Format("2006-01-02"),
				))
			} else {
				telegram.ReplyText(bot, chatID, fmt.Sprintf(
					"⚠️ 续期已到账，新的到期时间为 `%s`。\n\nABS 已恢复，但本地权限状态或审计写入失败，请联系管理员处理。",
					result.NewExpireAt.Format("2006-01-02"),
				))
			}
			return
		}
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"🎉 续费成功！延长 `%d` 天。\n📅 新到期时间：`%s`",
		result.Days,
		result.NewExpireAt.Format("2006-01-02"),
	))
}

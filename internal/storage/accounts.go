package storage

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func UserProfileStatusInactive(u model.User) bool {
	status := strings.TrimSpace(u.Status)
	return status != "" && status != "active"
}

func ActiveListeningAbuseFreeze(userID int64, absUserID string) (*model.ListeningAbuseRecord, error) {
	return activeListeningAbuseFreezeTx(DB, userID, absUserID)
}

func activeListeningAbuseFreezeTx(tx *gorm.DB, userID int64, absUserID string) (*model.ListeningAbuseRecord, error) {
	if tx == nil || userID == 0 || strings.TrimSpace(absUserID) == "" {
		return nil, nil
	}

	var record model.ListeningAbuseRecord
	err := tx.Where("user_id = ? AND abs_user_id = ? AND action = ? AND status = ?",
		userID, absUserID, ListeningAbuseActionFreeze, ListeningAbuseStatusActive).
		Order("freeze_end_at DESC, id DESC").
		First(&record).Error
	if err == nil {
		return &record, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return nil, err
}

func UserHasUsableLocalAbsAccountAt(u model.User, now time.Time) (bool, error) {
	if strings.TrimSpace(u.AbsUserID) == "" || UserProfileStatusInactive(u) || u.IsSuspended {
		return false, nil
	}
	if !u.IsWhitelist && IsUserExpiredAt(u, now) {
		return false, nil
	}

	freeze, err := ActiveListeningAbuseFreeze(u.TelegramID, u.AbsUserID)
	if err != nil {
		return false, err
	}
	return freeze == nil, nil
}

func UserHasUsableLocalAbsAccountTxAt(tx *gorm.DB, u model.User, now time.Time) (bool, error) {
	if strings.TrimSpace(u.AbsUserID) == "" || UserProfileStatusInactive(u) || u.IsSuspended {
		return false, nil
	}
	if !u.IsWhitelist && IsUserExpiredAt(u, now) {
		return false, nil
	}

	freeze, err := activeListeningAbuseFreezeTx(tx, u.TelegramID, u.AbsUserID)
	if err != nil {
		return false, err
	}
	return freeze == nil, nil
}

func ApplyRenewReactivateLocalStatusWithAudit(actorID int64, expectedAbsUserID string, expireAt time.Time, days int) error {
	if expectedAbsUserID == "" {
		return ErrAbsUserIDEmpty
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.Where("telegram_id = ?", actorID).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		if target.AbsUserID != expectedAbsUserID {
			return fmt.Errorf("target_abs_user_changed")
		}

		res := tx.Model(&model.User{}).
			Where("id = ? AND abs_user_id = ?", target.ID, expectedAbsUserID).
			Update("is_suspended", false)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("target_state_changed")
		}

		return WriteAuditLogInTx(
			tx,
			actorID,
			"RENEW_REACTIVATE_USER",
			fmt.Sprintf("%d", target.TelegramID),
			0,
			fmt.Sprintf("renew card reactivated user after successful ABS status update: username=%s tg=%d abs_user_id=%s expire_at=%s days=%d",
				textfmt.FormatPlainValue(target.Username), target.TelegramID, textfmt.FormatPlainValue(target.AbsUserID), expireAt.Format(time.RFC3339), days),
		)
	})
}

func RegistrationExpireAtForExistingUser(existingExpireAt *time.Time, defaultExpireAt *time.Time) (*time.Time, bool) {
	if defaultExpireAt == nil {
		return nil, existingExpireAt != nil
	}
	if existingExpireAt == nil || existingExpireAt.Before(*defaultExpireAt) {
		return defaultExpireAt, true
	}
	return nil, false
}

const (
	ListeningAbuseDailyRawHoursThreshold = 15.0
	ListeningAbuseFreezeHours            = 24

	ListeningAbuseActionWarning = "warning"
	ListeningAbuseActionFreeze  = "freeze"

	ListeningAbuseStatusWarningSent    = "warning_sent"
	ListeningAbuseStatusActive         = "active"
	ListeningAbuseStatusReleased       = "released"
	ListeningAbuseStatusAmnestied      = "amnestied"
	ListeningAbuseStatusReleaseBlocked = "release_blocked"

	ListeningAbuseEffectiveStartDayKey = "listening_abuse_effective_start_day"
	ListeningAbuseLastScanDayKey       = "listening_abuse_last_scan_day"
	ListeningAbuseLastScanAtKey        = "listening_abuse_last_scan_at"
	ListeningAbuseLastAttemptAtKey     = "listening_abuse_last_attempt_at"
	ListeningAbuseLastErrorKey         = "listening_abuse_last_error"
)

func IsUserExpiredAt(u model.User, now time.Time) bool {
	return u.ExpireAt != nil && !u.ExpireAt.After(now)
}

const (
	AccountTypeFormal = "formal"
	AccountTypeTrial  = "trial"

	ReferralStartPrefix = "ref_"

	ReferralTrialDays          = 3
	ReferralTaskHours          = 5.0
	ReferralRewardPoints       = 10
	ReferralDailyActivationMax = 3
	ReferralMonthlyRewardMax   = 150
	ReferralMinInviterMajor    = 1
	ReferralMinInviterMinor    = 1

	ReferralStatusActive    = "active"
	ReferralStatusEffective = "effective"
	ReferralStatusExpired   = "expired"

	ReferralAutoClaimLastRunAtKey = "referral_auto_claim_last_run_at"
	ReferralAutoClaimInterval     = 5 * time.Minute
	ReferralAutoClaimScanLimit    = 50
	ReferralAutoClaimRequestDelay = 80 * time.Millisecond
)

func normalizeAccountType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return AccountTypeFormal
	}
	return value
}

func IsTrialAccount(u model.User) bool {
	return normalizeAccountType(u.AccountType) == AccountTypeTrial
}

func IsFormalAccount(u model.User) bool {
	return !IsTrialAccount(u)
}

package storage

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
)

// Configuration grants the role even when the stored profile is an ordinary user.
func IsSuperAdminUser(u model.User) bool {
	return u.Role == "super_admin" || (config.AppConfig != nil && config.AppConfig.AdminIDs[u.TelegramID])
}

func UserProfileStatusInactive(u model.User) bool {
	status := strings.TrimSpace(u.Status)
	return status != "active"
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
	if strings.TrimSpace(u.AbsUserID) == "" || UserProfileStatusInactive(u) || u.IsSuspended || u.AdminSuspended {
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
	if strings.TrimSpace(u.AbsUserID) == "" || UserProfileStatusInactive(u) || u.IsSuspended || u.AdminSuspended {
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
	ListeningAbuseStatusReleaseBlocked = "release_blocked"

	ListeningAbuseLastScanDayKey   = "listening_abuse_last_scan_day"
	ListeningAbuseLastScanAtKey    = "listening_abuse_last_scan_at"
	ListeningAbuseLastAttemptAtKey = "listening_abuse_last_attempt_at"
	ListeningAbuseLastErrorKey     = "listening_abuse_last_error"
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
	ReferralAutoClaimCursorKey    = "referral_auto_claim_cursor_id"
	ReferralAutoClaimInterval     = 5 * time.Minute
	ReferralAutoClaimScanLimit    = 50
	ReferralAutoClaimRequestDelay = 80 * time.Millisecond
)

func IsTrialAccount(u model.User) bool {
	return strings.TrimSpace(u.AccountType) == AccountTypeTrial
}

func IsFormalAccount(u model.User) bool {
	return strings.TrimSpace(u.AccountType) == AccountTypeFormal
}

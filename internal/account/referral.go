package account

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

type referralStats struct {
	Activated   int64
	Effective   int64
	MonthReward int
	MonthKey    string
}

func referralInviterMeetsCultivationRequirement(major int, minor int) bool {
	if major > storage.ReferralMinInviterMajor {
		return true
	}
	return major == storage.ReferralMinInviterMajor && minor >= storage.ReferralMinInviterMinor
}

func requireReferralInviterCultivationInTx(tx *gorm.DB, userID int64) error {
	var cul model.Cultivation
	if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return storage.ErrReferralCultivationLow
		}
		return err
	}
	if !referralInviterMeetsCultivationRequirement(cul.MajorRealm, cul.MinorRealm) {
		return storage.ErrReferralCultivationLow
	}
	return nil
}

func referralDayBounds(t time.Time) (time.Time, time.Time, string) {
	loc := time.FixedZone("CST", 8*3600)
	local := t.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1), start.Format("2006-01-02")
}

func referralMonthKey(t time.Time) string {
	loc := time.FixedZone("CST", 8*3600)
	return t.In(loc).Format("200601")
}

func referralStatsText(stats referralStats) string {
	return fmt.Sprintf("累计激活：`%d`\n有效新人：`%d`\n本月奖励：`%d/%d` 积分", stats.Activated, stats.Effective, stats.MonthReward, storage.ReferralMonthlyRewardMax)
}

func referralStatsUnavailableText() string {
	return fmt.Sprintf("累计激活：读取失败\n有效新人：读取失败\n本月奖励：读取失败/%d 积分", storage.ReferralMonthlyRewardMax)
}

func loadReferralStats(inviterID int64, now time.Time) (referralStats, error) {
	stats := referralStats{MonthKey: referralMonthKey(now)}
	if err := storage.DB.Model(&model.ReferralActivation{}).
		Where("inviter_id = ?", inviterID).
		Count(&stats.Activated).Error; err != nil {
		return stats, err
	}
	if err := storage.DB.Model(&model.ReferralActivation{}).
		Where("inviter_id = ? AND status = ?", inviterID, storage.ReferralStatusEffective).
		Count(&stats.Effective).Error; err != nil {
		return stats, err
	}
	if err := storage.DB.Model(&model.ReferralActivation{}).
		Where("inviter_id = ? AND reward_month_key = ?", inviterID, stats.MonthKey).
		Select("COALESCE(SUM(reward_points), 0)").
		Scan(&stats.MonthReward).Error; err != nil {
		return stats, err
	}
	return stats, nil
}

func consumeReferralDailyActivationQuotaInTx(tx *gorm.DB, inviterID int64, dayKey string, now time.Time) error {
	res := tx.Exec(`
		INSERT INTO referral_daily_activation_quotas (
			created_at,
			updated_at,
			inviter_id,
			day_key,
			activation_count
		)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(inviter_id, day_key) DO UPDATE SET
			activation_count = referral_daily_activation_quotas.activation_count + 1,
			updated_at = excluded.updated_at
		WHERE referral_daily_activation_quotas.activation_count < ?
	`, now, now, inviterID, dayKey, storage.ReferralDailyActivationMax)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return storage.ErrReferralDailyLimit
	}
	return nil
}

func consumeReferralMonthlyRewardQuotaInTx(tx *gorm.DB, inviterID int64, monthKey string, points int, now time.Time) (bool, error) {
	if points <= 0 {
		return false, nil
	}
	res := tx.Exec(`
		INSERT INTO referral_monthly_reward_quotas (
			created_at,
			updated_at,
			inviter_id,
			month_key,
			reward_points
		)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(inviter_id, month_key) DO UPDATE SET
			reward_points = referral_monthly_reward_quotas.reward_points + excluded.reward_points,
			updated_at = excluded.updated_at
		WHERE referral_monthly_reward_quotas.reward_points + excluded.reward_points <= ?
	`, now, now, inviterID, monthKey, points, storage.ReferralMonthlyRewardMax)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func ParseReferralStartPayload(text string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return "", false
	}
	payload := strings.TrimSpace(fields[1])
	if !strings.HasPrefix(payload, storage.ReferralStartPrefix) {
		return "", false
	}
	code := strings.TrimSpace(strings.TrimPrefix(payload, storage.ReferralStartPrefix))
	return code, code != ""
}

func referralLink(bot *tgbotapi.BotAPI, code string) string {
	botName := ""
	if bot != nil {
		botName = strings.TrimSpace(bot.Self.UserName)
	}
	if botName == "" {
		return fmt.Sprintf("/start %s%s", storage.ReferralStartPrefix, code)
	}
	return fmt.Sprintf("https://t.me/%s?start=%s", botName, url.QueryEscape(storage.ReferralStartPrefix+code))
}

func ensureReferralCode(userID int64) (model.ReferralCode, error) {
	var code model.ReferralCode
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if strings.TrimSpace(u.AbsUserID) == "" || !storage.IsFormalAccount(u) {
			return storage.ErrReferralInviterNotEligible
		}
		if err := requireReferralInviterCultivationInTx(tx, userID); err != nil {
			return err
		}

		var txCode model.ReferralCode
		err := tx.Where("user_id = ?", userID).First(&txCode).Error
		if err == nil {
			if !txCode.IsEnabled {
				res := tx.Model(&model.ReferralCode{}).
					Where("id = ? AND user_id = ? AND is_enabled = ?", txCode.ID, userID, false).
					Update("is_enabled", true)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					return fmt.Errorf("REFERRAL_CODE_STATE_CHANGED")
				}
				txCode.IsEnabled = true
			}
			code = txCode
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		for i := 0; i < 8; i++ {
			candidate := security.GenerateRandomCode(10)
			txCode := model.ReferralCode{
				UserID:    userID,
				Code:      candidate,
				IsEnabled: true,
			}
			res := tx.Create(&txCode)
			if res.Error == nil && res.RowsAffected > 0 {
				code = txCode
				return nil
			}
			if res.Error == nil {
				return fmt.Errorf("CREATE_REFERRAL_CODE_MISSED")
			}
			if !storage.IsUniqueConstraintError(res.Error) {
				return res.Error
			}
		}
		return fmt.Errorf("CREATE_REFERRAL_CODE_FAILED")
	})
	if err != nil {
		return model.ReferralCode{}, err
	}
	return code, nil
}

func ValidateReferralCodeForStart(code string, inviteeID int64) error {
	var ref model.ReferralCode
	if err := storage.DB.Where("code = ? AND is_enabled = ?", strings.TrimSpace(code), true).First(&ref).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return storage.ErrReferralInvalidCode
	}
	if ref.UserID == inviteeID {
		return storage.ErrReferralSelfInvite
	}
	if err := requireReferralInviterCultivationInTx(storage.DB, ref.UserID); err != nil {
		if errors.Is(err, storage.ErrReferralCultivationLow) {
			return storage.ErrReferralInviterNotEligible
		}
		return err
	}
	return nil
}

func createReferralTrialUserInTx(tx *gorm.DB, user *model.User) error {
	if tx == nil || user == nil {
		return fmt.Errorf("REFERRAL_TRIAL_USER_INVALID")
	}
	entry := *user
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("REFERRAL_TRIAL_USER_CREATE_MISSED")
	}
	*user = entry
	return nil
}

func createReferralActivationInTx(tx *gorm.DB, activation *model.ReferralActivation) error {
	if tx == nil || activation == nil {
		return fmt.Errorf("REFERRAL_ACTIVATION_INVALID")
	}
	entry := *activation
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrReferralAlreadyTried
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("REFERRAL_ACTIVATION_CREATE_MISSED")
	}
	*activation = entry
	return nil
}

func CreateReferralTrialAccountInTx(tx *gorm.DB, inviteeID int64, username string, absUserID string, secCodeHash string, referralCode string, now time.Time) (time.Time, error) {
	referralCode = strings.TrimSpace(referralCode)
	if referralCode == "" {
		return time.Time{}, storage.ErrReferralInvalidCode
	}

	var code model.ReferralCode
	if err := tx.Where("code = ? AND is_enabled = ?", referralCode, true).First(&code).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return time.Time{}, err
		}
		return time.Time{}, storage.ErrReferralInvalidCode
	}
	if code.UserID == inviteeID {
		return time.Time{}, storage.ErrReferralSelfInvite
	}

	var inviter model.User
	if err := tx.Where("telegram_id = ?", code.UserID).First(&inviter).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return time.Time{}, err
		}
		return time.Time{}, storage.ErrReferralInviterNotEligible
	}
	if strings.TrimSpace(inviter.AbsUserID) == "" || !storage.IsFormalAccount(inviter) {
		return time.Time{}, storage.ErrReferralInviterNotEligible
	}
	if err := requireReferralInviterCultivationInTx(tx, code.UserID); err != nil {
		if errors.Is(err, storage.ErrReferralCultivationLow) {
			return time.Time{}, storage.ErrReferralInviterNotEligible
		}
		return time.Time{}, err
	}

	var existingActivation model.ReferralActivation
	if err := tx.Where("invitee_id = ?", inviteeID).First(&existingActivation).Error; err == nil {
		return time.Time{}, storage.ErrReferralAlreadyTried
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, err
	}

	_, _, activationDayKey := referralDayBounds(now)
	if err := consumeReferralDailyActivationQuotaInTx(tx, code.UserID, activationDayKey, now); err != nil {
		return time.Time{}, err
	}

	trialEndsAt := now.AddDate(0, 0, storage.ReferralTrialDays)
	var existingUser model.User
	err := tx.Where("telegram_id = ?", inviteeID).First(&existingUser).Error
	if err == nil {
		if strings.TrimSpace(existingUser.AbsUserID) != "" {
			return time.Time{}, storage.ErrReferralExistingAccount
		}
		updates := map[string]interface{}{
			"username":         username,
			"abs_user_id":      absUserID,
			"security_code":    secCodeHash,
			"status":           "active",
			"is_suspended":     false,
			"expire_at":        trialEndsAt,
			"account_type":     storage.AccountTypeTrial,
			"trial_started_at": now,
			"trial_ends_at":    trialEndsAt,
		}
		userRes := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND abs_user_id = ?", existingUser.ID, inviteeID, "").
			Updates(updates)
		if userRes.Error != nil {
			return time.Time{}, userRes.Error
		}
		if userRes.RowsAffected == 0 {
			return time.Time{}, fmt.Errorf("REFERRAL_TRIAL_USER_STATE_CHANGED")
		}
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		trialUser := model.User{
			TelegramID:     inviteeID,
			Username:       username,
			AbsUserID:      absUserID,
			SecurityCode:   secCodeHash,
			Status:         "active",
			ExpireAt:       &trialEndsAt,
			IsSuspended:    false,
			AccountType:    storage.AccountTypeTrial,
			TrialStartedAt: &now,
			TrialEndsAt:    &trialEndsAt,
		}
		if err := createReferralTrialUserInTx(tx, &trialUser); err != nil {
			return time.Time{}, err
		}
	} else {
		return time.Time{}, err
	}

	activation := model.ReferralActivation{
		CodeID:           code.ID,
		InviterID:        code.UserID,
		InviteeID:        inviteeID,
		Status:           storage.ReferralStatusActive,
		TrialStartedAt:   now,
		TrialEndsAt:      trialEndsAt,
		ActivationDayKey: activationDayKey,
	}
	if err := createReferralActivationInTx(tx, &activation); err != nil {
		return time.Time{}, err
	}

	if err := storage.WriteAuditLogInTx(
		tx,
		inviteeID,
		"REFERRAL_TRIAL_REGISTER",
		fmt.Sprintf("referral_activation_id=%d", activation.ID),
		0,
		fmt.Sprintf("invitee=%d registered trial account by inviter=%d code_id=%d abs_user_id=%s trial_ends_at=%s",
			inviteeID, code.UserID, code.ID, textfmt.FormatPlainValue(absUserID), trialEndsAt.Format(time.RFC3339)),
	); err != nil {
		return time.Time{}, err
	}

	return trialEndsAt, nil
}

func ConvertTrialToFormalWithInviteCode(userID int64, inviteHash string) (time.Time, error) {
	var nextExpireAt time.Time
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if !storage.IsTrialAccount(u) {
			return storage.ErrTrialFormalInviteOnly
		}

		var invite model.InviteCode
		if err := tx.Where("code_hash = ? AND is_used = ?", inviteHash, false).First(&invite).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrInvalidInviteCode
			}
			return err
		}

		if err := storage.CheckCodeOwnership(invite.Source, invite.OwnerUserID, userID); err != nil {
			return err
		}
		res := tx.Model(&model.InviteCode{}).
			Where("id = ? AND is_used = ?", invite.ID, false).
			Updates(map[string]interface{}{
				"is_used":    true,
				"used_by_id": userID,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrInvalidInviteCode
		}

		var defaultExpireAt *time.Time
		if config.AppConfig.AccountValidDays > 0 {
			exp := time.Now().AddDate(0, 0, config.AppConfig.AccountValidDays)
			defaultExpireAt = &exp
		}

		updates := map[string]interface{}{
			"account_type": storage.AccountTypeFormal,
		}
		var txNextExpireAt time.Time
		if next, shouldUpdate := storage.RegistrationExpireAtForExistingUser(u.ExpireAt, defaultExpireAt); shouldUpdate {
			if next == nil {
				updates["expire_at"] = nil
			} else {
				updates["expire_at"] = next
				txNextExpireAt = *next
			}
		} else if u.ExpireAt != nil {
			txNextExpireAt = *u.ExpireAt
		}

		userRes := tx.Model(&model.User{}).
			Where("id = ? AND telegram_id = ? AND account_type = ? AND abs_user_id = ?", u.ID, userID, storage.AccountTypeTrial, u.AbsUserID).
			Updates(updates)
		if userRes.Error != nil {
			return userRes.Error
		}
		if userRes.RowsAffected == 0 {
			return fmt.Errorf("REFERRAL_TRIAL_CONVERT_USER_STATE_CHANGED")
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"TRIAL_CONVERT_FORMAL",
			fmt.Sprintf("invite_code_id=%d", invite.ID),
			0,
			fmt.Sprintf("trial user %s(%d) converted to formal account with invite code %s; expire_at=%s",
				textfmt.FormatPlainValue(u.Username), userID, textfmt.FormatPlainValue(invite.CodePreview), txNextExpireAt.Format(time.RFC3339)),
		); err != nil {
			return err
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"USE_INVITE_CODE",
			fmt.Sprintf("invite_code_id=%d", invite.ID),
			0,
			fmt.Sprintf("trial user %s(%d) used invite code %s for formal conversion",
				textfmt.FormatPlainValue(u.Username), userID, textfmt.FormatPlainValue(invite.CodePreview)),
		); err != nil {
			return err
		}
		nextExpireAt = txNextExpireAt
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return nextExpireAt, nil
}

// sumReferralTrialRawSeconds 汇总试用窗口 [start, end) 内的听书日聚合秒数。
// absUserID 必须传入试用账号当前绑定：同一 Telegram 账号被删除档案后重新注册试用时，
// 历史绑定在同日的统计不得计入任务；excludeDayKey 非空时排除该日（到期日由快照补回）。
func sumReferralTrialRawSeconds(userID int64, absUserID string, start time.Time, end time.Time, excludeDayKey string) (float64, error) {
	if end.Before(start) {
		return 0, nil
	}
	startKey := game.SectDayKey(start)
	endExclusiveKey := game.SectDayKey(end.AddDate(0, 0, 1))

	query := storage.DB.Model(&model.DailyListeningStat{}).
		Where("user_id = ? AND abs_user_id = ? AND day_key >= ? AND day_key < ?", userID, strings.TrimSpace(absUserID), startKey, endExclusiveKey)
	if strings.TrimSpace(excludeDayKey) != "" {
		query = query.Where("day_key <> ?", strings.TrimSpace(excludeDayKey))
	}

	var total float64
	err := query.
		Select("COALESCE(SUM(capped_seconds), 0)").
		Scan(&total).Error
	return total, err
}

// loadReferralTrialDaySeconds 读取指定单日的听书日聚合秒数（无记录返回 0）。
func loadReferralTrialDaySeconds(userID int64, absUserID string, dayKey string) (float64, error) {
	var stat model.DailyListeningStat
	err := storage.DB.Where("user_id = ? AND abs_user_id = ? AND day_key = ?", userID, strings.TrimSpace(absUserID), dayKey).
		First(&stat).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return stat.CappedSeconds, nil
}

// referralTrialEndDaySnapshotDue 纯函数判定：当前扫描是否需要维护到期日快照。
// 只有试用尚未到期、且当前正好处于到期日当天时，才把当日听书值写入快照，
// 供到期后（生命周期巡检停用账号前）的补结算使用。
func referralTrialEndDaySnapshotDue(now time.Time, trialEndsAt time.Time) bool {
	if now.After(trialEndsAt) {
		return false
	}
	return game.SectDayKey(now) == game.SectDayKey(trialEndsAt)
}

// referralTrialSettleUsesEndDaySnapshot 纯函数判定：本次结算是否属于到期后的补结算路径。
// 补结算不得使用到期日整日聚合值（其中包含到期后、次日 03:00 生命周期巡检前的听书），
// 必须用试用中维护的到期日快照替代。
func referralTrialSettleUsesEndDaySnapshot(now time.Time, trialEndsAt time.Time) bool {
	return now.After(trialEndsAt)
}

// updateReferralTrialEndDaySnapshot 维护到期日听书快照（最新值语义：跨日回填修正后以最新值为准）。
// 仅当记录仍处于 active 且未生效时写入；失败只记日志，不阻断结算流程。
func updateReferralTrialEndDaySnapshot(activationID uint, inviteeID int64, seconds float64) {
	if activationID == 0 || inviteeID == 0 {
		return
	}
	res := storage.DB.Model(&model.ReferralActivation{}).
		Where("id = ? AND invitee_id = ? AND status = ? AND effective_at IS NULL", activationID, inviteeID, storage.ReferralStatusActive).
		Update("trial_end_day_seconds", seconds)
	if res.Error != nil {
		log.Printf("⚠️ 新人任务到期日快照写入失败: activation=%d invitee=%d err=%s", activationID, inviteeID, textfmt.FormatPlainError(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("新人任务到期日快照跳过（记录状态已变化）: activation=%d invitee=%d", activationID, inviteeID)
	}
}

func capReferralTrialTaskSeconds(seconds float64, start time.Time, end time.Time, now time.Time) float64 {
	if seconds <= 0 {
		return 0
	}
	capEnd := end
	if now.Before(capEnd) {
		capEnd = now
	}
	if !capEnd.After(start) {
		return 0
	}
	maxSeconds := capEnd.Sub(start).Seconds()
	if seconds > maxSeconds {
		return maxSeconds
	}
	return seconds
}

// referralTrialTaskOutcome 汇总一次新人任务结算（或进度检查）的结果。
type referralTrialTaskOutcome struct {
	ActivationID  uint
	InviteeID     int64
	InviterID     int64
	RawSeconds    float64
	NewExpireAt   time.Time
	RewardPoints  int
	RewardGranted bool
}

// settlementAttemptAt 返回本次统计的截止时刻：试用未到期时取当前时间，
// 已到期时取试用到期时间，保证"到期前确实听满"的记录即使扫描滞后也能公平结算。
func settlementAttemptAt(now time.Time, trialEndsAt time.Time) time.Time {
	if now.After(trialEndsAt) {
		return trialEndsAt
	}
	return now
}

// settleReferralTrialTaskIfDue 检查单个试用用户的新人任务：事务外刷新 ABS 听书统计，
// 达标后在同一事务内完成体验延期、邀请者积分（月度配额内）和激活状态落库。
// allowAfterTrialEnd=true（后台自动结算）时，试用到期前已听满的记录仍可公平补结算。
func settleReferralTrialTaskIfDue(activation model.ReferralActivation, now time.Time, allowAfterTrialEnd bool) (referralTrialTaskOutcome, bool, error) {
	outcome := referralTrialTaskOutcome{
		ActivationID: activation.ID,
		InviteeID:    activation.InviteeID,
		InviterID:    activation.InviterID,
	}
	if activation.EffectiveAt != nil {
		return outcome, false, storage.ErrReferralAlreadyEffective
	}

	attemptAt := settlementAttemptAt(now, activation.TrialEndsAt)
	if now.After(activation.TrialEndsAt) {
		if !allowAfterTrialEnd {
			return outcome, false, storage.ErrReferralTrialExpired
		}
	}

	var u model.User
	if err := storage.DB.Where("telegram_id = ?", activation.InviteeID).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return outcome, false, storage.ErrUserNotFound
		}
		return outcome, false, err
	}
	if !storage.IsTrialAccount(u) || strings.TrimSpace(u.AbsUserID) == "" {
		return outcome, false, storage.ErrReferralNoActivation
	}

	if _, ok := game.RefreshDailyListeningStatsFromABS(activation.InviteeID, u.AbsUserID); !ok {
		log.Printf("referral task refresh failed: user=%d abs=%s", activation.InviteeID, textfmt.FormatPlainValue(u.AbsUserID))
	}

	endDayKey := game.SectDayKey(activation.TrialEndsAt)
	var rawSeconds float64
	if referralTrialSettleUsesEndDaySnapshot(now, activation.TrialEndsAt) {
		// 到期后补结算：到期日整日聚合值包含到期后的听书（生命周期巡检每日 03:00 才停用账号），
		// 不得计入任务；改用试用中最后一次扫描维护的到期日快照。
		preEndDaySeconds, err := sumReferralTrialRawSeconds(activation.InviteeID, u.AbsUserID, activation.TrialStartedAt, activation.TrialEndsAt, endDayKey)
		if err != nil {
			return outcome, false, err
		}
		rawSeconds = preEndDaySeconds + activation.TrialEndDaySeconds
	} else {
		// 试用中实时结算：窗口右端只可能包含“今天”，今日统计刚完成 ABS 刷新、
		// 全部发生在当前时刻之前，必然处于试用窗口内，可安全整日计入。
		var err error
		rawSeconds, err = sumReferralTrialRawSeconds(activation.InviteeID, u.AbsUserID, activation.TrialStartedAt, activation.TrialEndsAt, "")
		if err != nil {
			return outcome, false, err
		}
		// 当前正好处于到期日当天：维护到期日快照，供到期后的公平补结算使用。
		if referralTrialEndDaySnapshotDue(now, activation.TrialEndsAt) {
			endDaySeconds, err := loadReferralTrialDaySeconds(activation.InviteeID, u.AbsUserID, endDayKey)
			if err != nil {
				return outcome, false, err
			}
			updateReferralTrialEndDaySnapshot(activation.ID, activation.InviteeID, endDaySeconds)
		}
	}

	rawSeconds = capReferralTrialTaskSeconds(rawSeconds, activation.TrialStartedAt, activation.TrialEndsAt, attemptAt)
	outcome.RawSeconds = rawSeconds
	if rawSeconds < storage.ReferralTaskHours*3600 {
		return outcome, false, storage.ErrReferralTaskNotComplete
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var locked model.ReferralActivation
		if err := tx.Where("id = ?", activation.ID).First(&locked).Error; err != nil {
			return err
		}
		if locked.EffectiveAt != nil {
			return storage.ErrReferralAlreadyEffective
		}

		var invitee model.User
		if err := tx.Where("telegram_id = ?", activation.InviteeID).First(&invitee).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if !storage.IsTrialAccount(invitee) {
			return storage.ErrReferralNoActivation
		}

		base := locked.TrialEndsAt
		if invitee.ExpireAt != nil && invitee.ExpireAt.After(base) {
			base = *invitee.ExpireAt
		}
		txNewExpireAt := base.AddDate(0, 0, storage.ReferralTrialDays)
		effectiveAt := now
		_, _, rewardDayKey := referralDayBounds(now)
		rewardMonthKey := referralMonthKey(now)
		txRewardPoints := 0
		txRewardGranted := false

		quotaGranted, err := consumeReferralMonthlyRewardQuotaInTx(tx, locked.InviterID, rewardMonthKey, storage.ReferralRewardPoints, now)
		if err != nil {
			return err
		}
		if quotaGranted {
			txRewardPoints = storage.ReferralRewardPoints
			if err := storage.ApplyPointDeltaInTx(
				tx,
				locked.InviterID,
				txRewardPoints,
				"referral_reward",
				fmt.Sprintf("邀请新人 %d 试用期内听书满 %.0f 小时", activation.InviteeID, storage.ReferralTaskHours),
				"referral",
				fmt.Sprintf("%d", locked.ID),
			); err != nil {
				return err
			}
			txRewardGranted = true
		}

		inviteeUpdates := map[string]interface{}{
			"expire_at": txNewExpireAt,
		}
		// 自动结算可能晚于试用到期：若期间生命周期巡检已停用账号，延期到账时一并恢复本地状态，
		// 与管理端补续期的恢复语义一致；ABS 侧状态由生命周期巡检按新 expire_at 修复。
		if invitee.IsSuspended || (invitee.Status != "" && invitee.Status != "active") {
			inviteeUpdates["is_suspended"] = false
			inviteeUpdates["status"] = "active"
		}
		inviteeRes := tx.Model(&model.User{}).
			Where("id = ? AND account_type = ? AND abs_user_id = ?", invitee.ID, storage.AccountTypeTrial, invitee.AbsUserID).
			Updates(inviteeUpdates)
		if inviteeRes.Error != nil {
			return inviteeRes.Error
		}
		if inviteeRes.RowsAffected == 0 {
			return fmt.Errorf("REFERRAL_TRIAL_INVITEE_STATE_CHANGED")
		}

		updates := map[string]interface{}{
			"status":                   storage.ReferralStatusEffective,
			"effective_at":             effectiveAt,
			"extended_at":              effectiveAt,
			"raw_seconds_at_effective": rawSeconds,
			"reward_points":            txRewardPoints,
			"reward_day_key":           rewardDayKey,
			"reward_month_key":         rewardMonthKey,
		}
		if txRewardGranted {
			updates["rewarded_at"] = effectiveAt
		}
		activationRes := tx.Model(&model.ReferralActivation{}).
			Where("id = ? AND invitee_id = ? AND status = ? AND effective_at IS NULL", locked.ID, activation.InviteeID, storage.ReferralStatusActive).
			Updates(updates)
		if activationRes.Error != nil {
			return activationRes.Error
		}
		if activationRes.RowsAffected == 0 {
			return fmt.Errorf("REFERRAL_TRIAL_ACTIVATION_STATE_CHANGED")
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			activation.InviteeID,
			"REFERRAL_TRIAL_TASK_CLAIM",
			fmt.Sprintf("referral_activation_id=%d", locked.ID),
			0,
			fmt.Sprintf("invitee=%d completed referral trial task; raw_hours=%.2f extension_days=%d inviter=%d reward_points=%d expire_at=%s",
				activation.InviteeID, rawSeconds/3600.0, storage.ReferralTrialDays, locked.InviterID, txRewardPoints, txNewExpireAt.Format(time.RFC3339)),
		); err != nil {
			return err
		}
		outcome.NewExpireAt = txNewExpireAt
		outcome.RewardPoints = txRewardPoints
		outcome.RewardGranted = txRewardGranted
		return nil
	})
	if err != nil {
		// 事务失败或提交失败：结果一律清空，调用方不得发布任何未生效的延期或奖励。
		outcome.NewExpireAt = time.Time{}
		outcome.RewardPoints = 0
		outcome.RewardGranted = false
		return outcome, false, err
	}
	return outcome, true, nil
}

func showMyReferral(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	code, err := ensureReferralCode(msg.From.ID)
	if err != nil {
		if errors.Is(err, storage.ErrReferralInviterNotEligible) || errors.Is(err, storage.ErrUserNotFound) {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 仅正式账号可生成个人邀请链接。新人体验账号需先使用正式邀请码转正。")
			return
		}
		if errors.Is(err, storage.ErrReferralCultivationLow) {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 个人邀请链接需达到【斗者初期】后解锁。请先听书积累修为，并发送 `听书报告` 同步斗气档案。")
			return
		}
		log.Printf("create referral code failed: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 邀请链接生成失败，请稍后重试。")
		return
	}

	statsText := referralStatsUnavailableText()
	if stats, err := loadReferralStats(msg.From.ID, time.Now()); err != nil {
		log.Printf("load referral stats failed: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
	} else {
		statsText = referralStatsText(stats)
	}

	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf(
		"🔗 **我的邀请链接**\n\n链接：`%s`\n\n规则：斗者初期及以上正式账号可邀请新人；新人通过链接注册后获得 `%d` 天体验；体验期内听书满 `%.0f` 小时将**自动**获得 `%d` 天体验延期（无需手动领取）；邀请者获得 `%d` 积分。\n\n限制：同一邀请链接每日最多激活 `%d` 名新人；每月邀请奖励最多 `%d` 积分。\n\n%s",
		referralLink(bot, code.Code),
		storage.ReferralTrialDays,
		storage.ReferralTaskHours,
		storage.ReferralTrialDays,
		storage.ReferralRewardPoints,
		storage.ReferralDailyActivationMax,
		storage.ReferralMonthlyRewardMax,
		statsText,
	))
}

// referralAutoClaimMu 保证同一时刻只有一轮自动结算在跑。
var referralAutoClaimMu sync.Mutex

// runReferralAutoClaimIfNeeded 后台自动结算新人任务：
// 扫描试用中/到期的 active 邀请激活记录，刷新听书统计后自动为达标用户结算延期与奖励，
// 并分别私信提醒被邀请者与邀请者。与手动命令无关，24 小时持续运行。
func RunReferralAutoClaimIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	if bot == nil || storage.DB == nil {
		return
	}

	lastAt, err := storage.GetSystemConfigTimeChecked(storage.ReferralAutoClaimLastRunAtKey)
	if err != nil {
		log.Printf("⚠️ 新人任务自动结算状态读取失败，本轮跳过: err=%s", textfmt.FormatPlainError(err))
		return
	}
	if !lastAt.IsZero() && now.Sub(lastAt) < storage.ReferralAutoClaimInterval {
		return
	}

	if !referralAutoClaimMu.TryLock() {
		return
	}
	defer referralAutoClaimMu.Unlock()

	// 双重检查：拿到锁后再确认没有其他 goroutine 刚刚跑过。
	lastAt, err = storage.GetSystemConfigTimeChecked(storage.ReferralAutoClaimLastRunAtKey)
	if err != nil {
		log.Printf("⚠️ 新人任务自动结算状态读取失败，本轮跳过: err=%s", textfmt.FormatPlainError(err))
		return
	}
	if !lastAt.IsZero() && now.Sub(lastAt) < storage.ReferralAutoClaimInterval {
		return
	}

	var activations []model.ReferralActivation
	if err := storage.DB.
		Where("status = ? AND effective_at IS NULL AND trial_ends_at > ?", storage.ReferralStatusActive, now.AddDate(0, 0, -1)).
		Order("trial_ends_at ASC").
		Limit(storage.ReferralAutoClaimScanLimit).
		Find(&activations).Error; err != nil {
		log.Printf("⚠️ 新人任务自动结算扫描失败: err=%s", textfmt.FormatPlainError(err))
		return
	}

	for _, activation := range activations {
		outcome, settled, err := settleReferralTrialTaskIfDue(activation, time.Now(), true)
		if err != nil {
			switch {
			case errors.Is(err, storage.ErrReferralTaskNotComplete), errors.Is(err, storage.ErrReferralAlreadyEffective):
				// 正常未达标或已被处理，静默跳过。
			case errors.Is(err, storage.ErrUserNotFound), errors.Is(err, storage.ErrReferralNoActivation):
				log.Printf("新人任务自动结算跳过: activation=%d invitee=%d err=%s", activation.ID, activation.InviteeID, textfmt.FormatPlainError(err))
			default:
				log.Printf("⚠️ 新人任务自动结算失败: activation=%d invitee=%d err=%s", activation.ID, activation.InviteeID, textfmt.FormatPlainError(err))
			}
		} else if settled {
			notifyReferralAutoClaimResult(bot, outcome)
		}
		time.Sleep(storage.ReferralAutoClaimRequestDelay)
	}

	// 已过补结算窗口（试用到期超过 24h）仍未达标的记录：标记为 expired 终态，避免永久滞留 active。
	// 不影响统计口径（累计激活统计全部记录，有效新人只统计 effective）；
	// invitee_id 唯一约束依旧生效，过期后也不能再次被邀请。
	if err := storage.DB.Model(&model.ReferralActivation{}).
		Where("status = ? AND effective_at IS NULL AND trial_ends_at <= ?", storage.ReferralStatusActive, now.AddDate(0, 0, -1)).
		Update("status", storage.ReferralStatusExpired).Error; err != nil {
		log.Printf("⚠️ 新人任务过期状态标记失败: err=%s", textfmt.FormatPlainError(err))
	}

	if err := storage.SetSystemConfigStringChecked(storage.ReferralAutoClaimLastRunAtKey, time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("⚠️ 新人任务自动结算状态写入失败: err=%s", textfmt.FormatPlainError(err))
	}
}

// notifyReferralAutoClaimResult 自动结算成功后分别私信被邀请者（延期到账）与邀请者（积分奖励）。
// 发送失败只记日志，不影响已落库的结算结果。
func notifyReferralAutoClaimResult(bot *tgbotapi.BotAPI, outcome referralTrialTaskOutcome) {
	inviteeText := fmt.Sprintf(
		"🎉 **新人任务自动完成**\n\n体验期内累计听书：`%.2f/%.0f` 小时\n已自动为您延长 `%d` 天体验，无需任何操作。\n\n新的到期时间：`%s`\n感谢体验，期待您的继续使用！",
		outcome.RawSeconds/3600.0,
		storage.ReferralTaskHours,
		storage.ReferralTrialDays,
		outcome.NewExpireAt.Format("2006-01-02"),
	)
	sendReferralNotify(bot, outcome.InviteeID, inviteeText)

	var inviterText string
	if outcome.RewardGranted {
		inviterText = fmt.Sprintf(
			"🎁 **邀请奖励到账**\n\n您邀请的新人已完成新人任务（体验期内听书满 `%.0f` 小时）。\n\n奖励：`+%d` 积分",
			storage.ReferralTaskHours,
			outcome.RewardPoints,
		)
	} else {
		inviterText = fmt.Sprintf(
			"🎁 **邀请奖励通知**\n\n您邀请的新人已完成新人任务（体验期内听书满 `%.0f` 小时），其体验已自动延期 `%d` 天。\n\n本月邀请积分奖励已达 `%d` 分上限，本次不再发放积分。",
			storage.ReferralTaskHours,
			storage.ReferralTrialDays,
			storage.ReferralMonthlyRewardMax,
		)
	}
	sendReferralNotify(bot, outcome.InviterID, inviterText)
}

func sendReferralNotify(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	// 去重键必须包含业务维度（这里是毫秒级触发序号）：
	// 同一邀请者同轮可能有多个新人达标，用固定 chatID 会被调度器去重吞掉后续通知。
	if !telegram.EnqueueNoAutoDelete(bot, msg, "referral_auto_claim_notice", telegram.TelegramAsyncPriorityNormal, "") {
		log.Printf("⚠️ 新人任务自动结算通知入队失败: chat=%d", chatID)
	}
}

func showReferralStats(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	statsText := referralStatsUnavailableText()
	if stats, err := loadReferralStats(msg.From.ID, time.Now()); err != nil {
		log.Printf("load referral stats failed: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
	} else {
		statsText = referralStatsText(stats)
	}
	telegram.ReplyText(bot, msg.Chat.ID, "📊 **邀请统计**\n\n"+statsText)
}

func HandleReferralCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	if !msg.Chat.IsPrivate() {
		switch text {
		case "我的邀请", "邀请链接", "拉新链接", "邀请统计":
			telegram.SendPlainText(bot, msg.Chat.ID, "邀请链接和邀请统计请私聊 Bot 执行。")
			return true
		default:
			return false
		}
	}

	switch text {
	case "我的邀请", "邀请链接", "拉新链接":
		showMyReferral(bot, msg)
		return true
	case "邀请统计":
		showReferralStats(bot, msg)
		return true
	default:
		return false
	}
}

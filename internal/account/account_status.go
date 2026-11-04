package account

import (
	"fmt"
	"log"
	"strings"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

type accountStatusDisplayMode int

const (
	AccountStatusDisplaySelf accountStatusDisplayMode = iota
	AccountStatusDisplayAdmin
)

type accountStatusKind string

const (
	accountStatusNoAccount              accountStatusKind = "no_account"
	accountStatusInactiveProfile        accountStatusKind = "inactive_profile"
	accountStatusSuspended              accountStatusKind = "suspended"
	accountStatusListeningAbuseFreeze   accountStatusKind = "listening_abuse_freeze"
	accountStatusExpired                accountStatusKind = "expired"
	accountStatusWhitelist              accountStatusKind = "whitelist"
	accountStatusExpiring               accountStatusKind = "expiring"
	accountStatusPermanent              accountStatusKind = "permanent"
	accountStatusAbsDisabledOutOfSync   accountStatusKind = "abs_disabled_out_of_sync"
	accountStatusUnknownNeedsRetry      accountStatusKind = "unknown_needs_retry"
	accountStatusListeningAbuseReadFail accountStatusKind = "listening_abuse_read_fail"
)

type accountStatusDisplay struct {
	Kind              accountStatusKind
	Text              string
	LocalAllowsAccess bool
}

type accountStatusDisplayInput struct {
	User          model.User
	Now           time.Time
	Mode          accountStatusDisplayMode
	ActiveFreeze  *model.ListeningAbuseRecord
	FreezeReadErr error
	AbsActive     *bool
	AbsReadErr    error
}

func ResolveUserAccountStatusDisplay(u model.User, now time.Time, mode accountStatusDisplayMode, checkAbs bool) accountStatusDisplay {
	freeze, freezeErr := storage.ActiveListeningAbuseFreeze(u.TelegramID, u.AbsUserID)
	if freezeErr != nil {
		log.Printf("⚠️ 读取播放异常临停状态失败: user=%d abs=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(freezeErr))
	}

	var absActive *bool
	var absErr error
	if checkAbs && strings.TrimSpace(u.AbsUserID) != "" && abs.DefaultClient != nil {
		active, err := abs.DefaultClient.GetUserActiveStatus(u.AbsUserID)
		if err != nil {
			absErr = err
			log.Printf("⚠️ 读取 ABS 账号活跃状态失败: user=%d abs=%s err=%s", u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err))
		} else {
			absActive = &active
		}
	}

	return buildAccountStatusDisplay(accountStatusDisplayInput{
		User:          u,
		Now:           now,
		Mode:          mode,
		ActiveFreeze:  freeze,
		FreezeReadErr: freezeErr,
		AbsActive:     absActive,
		AbsReadErr:    absErr,
	})
}

func buildAccountStatusDisplay(in accountStatusDisplayInput) accountStatusDisplay {
	kind, text, localAllowsAccess := localAccountStatusText(in.User, in.ActiveFreeze, in.Now, in.Mode)

	if in.FreezeReadErr != nil {
		if localAllowsAccess {
			kind = accountStatusListeningAbuseReadFail
			text = "⚠️ 风控状态读取失败，本地显示：" + text
		} else {
			text += "（风控状态读取失败）"
		}
		localAllowsAccess = false
	}

	if in.AbsReadErr != nil {
		if localAllowsAccess {
			kind = accountStatusUnknownNeedsRetry
			text = "⚠️ ABS 状态读取失败，本地显示：" + text
		} else {
			text += "（ABS 状态读取失败）"
		}
		return accountStatusDisplay{Kind: kind, Text: text, LocalAllowsAccess: localAllowsAccess}
	}

	if in.AbsActive != nil {
		if !*in.AbsActive {
			if localAllowsAccess {
				return accountStatusDisplay{
					Kind:              accountStatusAbsDisabledOutOfSync,
					Text:              "🛑 ABS 服务端已停用（本地待核查）",
					LocalAllowsAccess: false,
				}
			}
			if kind != accountStatusNoAccount && kind != accountStatusInactiveProfile {
				text += "（ABS 已停用）"
			}
		} else if !localAllowsAccess && kind != accountStatusNoAccount {
			text += "（ABS 仍启用，需核查）"
		}
	}

	return accountStatusDisplay{Kind: kind, Text: text, LocalAllowsAccess: localAllowsAccess}
}

func localAccountStatusText(u model.User, activeFreeze *model.ListeningAbuseRecord, now time.Time, mode accountStatusDisplayMode) (accountStatusKind, string, bool) {
	if strings.TrimSpace(u.AbsUserID) == "" {
		return accountStatusNoAccount, "👻 幽灵钱包 (尚未绑定听书账号)", false
	}

	if storage.UserProfileStatusInactive(u) {
		if mode == AccountStatusDisplayAdmin {
			return accountStatusInactiveProfile, fmt.Sprintf("⛔ 档案已停用（本地状态：`%s`）", textfmt.EscapeMarkdown(strings.TrimSpace(u.Status))), false
		}
		return accountStatusInactiveProfile, "⛔ 档案已停用", false
	}

	if u.IsSuspended {
		return accountStatusSuspended, "🛑 已封禁/暂停", false
	}

	if !u.IsWhitelist && storage.IsUserExpiredAt(u, now) {
		return accountStatusExpired, "⏳ 已过期", false
	}

	if activeFreeze != nil {
		return accountStatusListeningAbuseFreeze, listeningAbuseFreezeStatusText(*activeFreeze, now), false
	}

	if u.IsWhitelist {
		return accountStatusWhitelist, "🏳️ 白名单 (永久免保号清理)", true
	}

	if u.ExpireAt != nil {
		return accountStatusExpiring, "📅 " + u.ExpireAt.Format("2006-01-02") + " 到期", true
	}

	return accountStatusPermanent, "✅ 永久有效", true
}

func listeningAbuseFreezeStatusText(record model.ListeningAbuseRecord, now time.Time) string {
	if record.FreezeEndAt == nil {
		return "⛔ 播放异常临时暂停"
	}
	endText := storage.DailyOperationsLocalTime(*record.FreezeEndAt).Format("2006-01-02 15:04")
	if record.FreezeEndAt.After(now) {
		return fmt.Sprintf("⛔ 播放异常临时暂停（预计 `%s` 恢复）", endText)
	}
	return fmt.Sprintf("⛔ 播放异常临时暂停（已到 `%s`，等待后台恢复）", endText)
}

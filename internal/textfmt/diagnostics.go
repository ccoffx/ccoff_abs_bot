package textfmt

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	AuditTargetMaxRunes = 200
	AuditDetailMaxRunes = 1000
)

func FormatAuditTextForStorage(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	formatted := FormatAuditTextForDisplay(text)
	runes := []rune(formatted)
	if len(runes) <= maxRunes {
		return formatted
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func FormatAuditTextForDisplay(text string) string {
	return FormatDiagnosticTextForDisplay(text)
}

func normalizeAuditTextForReadability(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	lastSpace := false
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if r <= ' ' || r == 0x7f || r == '\u2028' || r == '\u2029' {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

func FormatDiagnosticTextForDisplay(text string) string {
	return normalizeAuditTextForReadability(redactSensitiveAuditText(text))
}

func redactSensitiveAuditText(text string) string {
	if text == "" {
		return ""
	}
	text = stripUnicodeFormatControls(text)

	patterns := []struct {
		re   *regexp.Regexp
		repl string
	}{
		{
			re:   regexp.MustCompile("(?i)(\"?(?:password|token|api[_-]?key|authorization|secret|security[_-]?code|backup_encrypt_key|security_pepper|telegram_bot_token)\"?\\s*[:=]\\s*)(\"[^\"]*\"|`[^`]*`|bearer\\s+[A-Za-z0-9._~+/=-]+|[^\\s&,，;；}]+)"),
			repl: "${1}***",
		},
		{
			re:   regexp.MustCompile("((?:密码|安全码|卡密|邀请码|续期卡|备份密钥|安全密钥)\\s*[:：=]\\s*)(`[^`]*`|[^\\s,，;；。]+)"),
			repl: "${1}***",
		},
		{
			re:   regexp.MustCompile("(?i)\\b(TELEGRAM_BOT_TOKEN|ABS_API_KEY|BACKUP_ENCRYPT_KEY|SECURITY_PEPPER)\\s*[:=]\\s*[^\\s&,，;；]+"),
			repl: "${1}=***",
		},
		{
			re:   regexp.MustCompile("(?i)\\b(token|api[_-]?key|password|secret|authorization|security[_-]?code|backup_encrypt_key|security_pepper|telegram_bot_token)=([^&\\s\"'`,;]+)"),
			repl: "${1}=***",
		},
		{
			re:   regexp.MustCompile(`(?i)(https://api\.telegram\.org/)bot[^/\s]+/`),
			repl: "${1}bot***:***/",
		},
		{
			re:   regexp.MustCompile("(?i)(https?://)([^\\s/@:]+):([^\\s/@]+)@"),
			repl: "${1}***:***@",
		},
		{
			re:   regexp.MustCompile("(?i)bearer\\s+[A-Za-z0-9._~+/=-]+"),
			repl: "Bearer ***",
		},
	}

	redacted := text
	for _, pattern := range patterns {
		redacted = pattern.re.ReplaceAllString(redacted, pattern.repl)
	}
	return redacted
}

func stripUnicodeFormatControls(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	changed := false
	for _, r := range text {
		if unicode.Is(unicode.Cf, r) {
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	if !changed {
		return text
	}
	return b.String()
}

func FormatMarkdownError(err error) string {
	if err == nil {
		return ""
	}
	return EscapeMarkdown(TruncateRunes(FormatDiagnosticTextForDisplay(err.Error()), 500))
}

func FormatPlainError(err error) string {
	if err == nil {
		return ""
	}
	return FormatPlainValue(err)
}

func FormatPlainValue(value any) string {
	return TruncateRunes(FormatDiagnosticTextForDisplay(fmt.Sprint(value)), 500)
}

func FormatTelegramSendError(err error) string {
	return FormatPlainError(err)
}

func FormatSystemConfigErrorForMarkdown(text string) string {
	if strings.TrimSpace(text) == "" {
		return "无"
	}
	return EscapeMarkdown(TruncateRunes(FormatDiagnosticTextForDisplay(text), 500))
}

func pointTransactionTypeText(txType string) string {
	switch txType {
	case "sign_in":
		return "每日签到"
	case "sign_streak_bonus":
		return "连签奖励"
	case "blind_box_cost":
		return "盲盒消费"
	case "code_cashout":
		return "本服卡密回收"
	case "exchange_invite":
		return "兑换邀请码"
	case "exchange_renew":
		return "兑换续期卡"
	case "redpacket_send":
		return "发红包"
	case "redpacket_grab":
		return "抢红包"
	case "admin_adjust":
		return "管理员调账"
	case "treasure_cost":
		return "寻宝开箱"
	case "treasure_reward":
		return "寻宝奖励"
	case "book_request_cost":
		return "求书工单"
	case "book_request_refund":
		return "求书工单退款"
	case "breakthrough_auto_buy":
		return "突破代购"
	case "breakthrough_refund":
		return "突破返还"
	case "breakthrough_fail_penalty":
		return "突破失败惩罚"
	case "breakthrough_splash_penalty":
		return "雷劫外溢惩罚"
	case "breakthrough_treasure_fee":
		return "至宝突破渡劫费"
	case "sect_create":
		return "创建宗门"
	case "sect_join":
		return "加入宗门"
	case "sect_donate":
		return "宗门捐献"
	case "sect_shop_points":
		return "宗门商店"
	case "sect_secret_realm_reward":
		return "宗门秘境"
	case "sect_horn":
		return "宗门喇叭"
	case "world_horn":
		return "世界喇叭"
	case "shop_buy_item":
		return "聚宝斋购买"
	case "marketplace_buy":
		return "交易行购买"
	case "marketplace_sell":
		return "交易行售出"
	case "marketplace_fee":
		return "交易行手续费"
	case "garden_plot_open":
		return "药园开垦"
	case "garden_seed_buy":
		return "购买种子"
	case "garden_herb_sell":
		return "药铺回收"
	case "garden_recipe_unlock":
		return "参悟丹方"
	case "garden_alchemy_cost":
		return "炼丹炉火"
	case "world_boss_reward":
		return "世界Boss奖励"
	case "lottery_reward":
		return "积分抽奖奖励"
	case "lottery_entry_cost":
		return "积分抽奖参与"
	case "lottery_loser_refund":
		return "抽奖未中奖返还"
	case "lottery_cancel_refund":
		return "抽奖取消退款"
	case "service_outage_compensation":
		return "服务异常补偿"
	case "referral_reward":
		return "邀请拉新奖励"
	default:
		return txType
	}
}

func PointTransactionTypeMarkdown(txType string) string {
	return EscapeMarkdown(pointTransactionTypeText(txType))
}

func PointTransactionDescriptionMarkdown(description string) string {
	description = strings.TrimSpace(FormatDiagnosticTextForDisplay(description))
	if description == "" {
		return "-"
	}
	return EscapeMarkdownPreservingEscapes(description)
}

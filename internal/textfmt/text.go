package textfmt

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func GetTelegramDisplayName(user *tgbotapi.User) string {
	if user == nil {
		return "未知用户"
	}
	if user.UserName != "" {
		return "@" + user.UserName
	}
	if strings.TrimSpace(user.FirstName) != "" {
		return strings.TrimSpace(user.FirstName)
	}
	return fmt.Sprintf("%d", user.ID)
}

func ContainsDisallowedControl(text string, allowNewline bool) bool {
	for _, r := range text {
		if r == '\n' && allowNewline {
			continue
		}
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

var lotteryDrawTimeLocation = time.FixedZone("CST", 8*3600)

func ParseLotteryDrawTimeInput(raw string, now time.Time) (time.Time, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "", fmt.Errorf("开奖时间不能为空，请输入分钟数或固定时间，例如：2026-06-07 20:00:00")
	}

	if minutes, err := strconv.Atoi(raw); err == nil {
		if minutes < 1 || minutes > 10080 {
			return time.Time{}, "", fmt.Errorf("分钟数需为 1-10080，请重新输入：")
		}
		drawAt := now.Add(time.Duration(minutes) * time.Minute)
		return drawAt, fmt.Sprintf("%d 分钟后开奖（%s）", minutes, drawAt.Format("2006-01-02 15:04:05")), nil
	}

	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		drawAt, err := time.ParseInLocation(layout, raw, lotteryDrawTimeLocation)
		if err != nil {
			continue
		}
		if !drawAt.After(now) {
			return time.Time{}, "", fmt.Errorf("开奖时间必须晚于当前时间。当前时间：%s", now.Format("2006-01-02 15:04:05"))
		}
		return drawAt, "固定时间开奖：" + drawAt.Format("2006-01-02 15:04:05"), nil
	}

	return time.Time{}, "", fmt.Errorf("时间格式不正确。请发送分钟数，或固定时间格式：YYYY-MM-DD HH:MM:SS，例如：2026-06-07 20:00:00")
}

func LotteryDisplayText(text string, maxLen int, fallback string) string {
	text = strings.Map(func(r rune) rune {
		if ContainsDisallowedControl(string(r), false) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return fallback
	}
	return TruncateRunes(text, maxLen)
}

func MinInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

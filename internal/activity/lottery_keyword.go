package activity

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func validateLotteryJoinKeyword(keyword string) error {
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) < 2 || utf8.RuneCountInString(keyword) > 40 || textfmt.ContainsDisallowedControl(keyword, false) {
		return fmt.Errorf("参与口令需为 2–40 个字，且不能包含换行或控制字符")
	}
	allDigits := true
	for _, r := range keyword {
		if !unicode.IsDigit(r) {
			allDigits = false
		}
		if unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("参与口令不能包含不可见格式字符")
		}
	}
	if allDigits || textfmt.IsReservedBotInput(keyword) {
		return fmt.Errorf("参与口令不能是纯数字、菜单或 Bot 指令，请换一个口令")
	}
	return nil
}

func lotteryJoinInput(activity model.LotteryActivity) string {
	if activity.JoinKeyword != "" {
		return activity.JoinKeyword
	}
	return fmt.Sprintf("参加抽奖 %d", activity.ID)
}

func lotteryUserActivityLabel(activityID uint) string {
	var activity model.LotteryActivity
	if err := storage.DB.Select("title").First(&activity, activityID).Error; err != nil {
		return "抽奖"
	}
	return "抽奖《" + textfmt.LotteryDisplayText(activity.Title, 80, "-") + "》"
}

// Only called from an idle session. Existing command handlers retain priority.
func handleLotteryKeyword(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if validateLotteryJoinKeyword(text) != nil {
		return false
	}
	if storage.DB == nil {
		return false
	}
	var activity model.LotteryActivity
	err := storage.DB.Where("join_keyword = ? AND status IN ?", text, []string{lotteryStatusActive, lotteryStatusDrawing}).First(&activity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		return false
	}
	telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	joinLotteryByID(bot, msg, activity.ID)
	return true
}

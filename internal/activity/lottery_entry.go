package activity

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const lotteryEntryCallbackPrefix = "lottery_enter:"

func lotteryStartPayload(text string) string {
	fields := strings.Fields(text)
	if len(fields) != 2 || !telegram.IsTelegramCommandText(fields[0], "/start") {
		return ""
	}
	return fields[1]
}
func IsLotteryStart(text string) bool {
	p := lotteryStartPayload(text)
	return strings.HasPrefix(p, "lottery_")
}
func HandleLotteryStart(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) {
	p := lotteryStartPayload(text)
	if p == "lottery_claim" {
		claimAllLotteryRewards(bot, msg.Chat.ID, msg.From.ID)
		return
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(p, "lottery_"), 10, 32)
	if err != nil || id == 0 {
		telegram.SendPlainText(bot, msg.Chat.ID, "抽奖链接无效。")
		return
	}
	var a model.LotteryActivity
	if err = storage.DB.First(&a, uint(id)).Error; err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, "抽奖不存在或暂时无法读取。")
		return
	}
	if a.Status != lotteryStatusActive || (a.DrawAt != nil && !time.Now().Before(*a.DrawAt)) {
		telegram.SendPlainText(bot, msg.Chat.ID, "该抽奖已结束或正在开奖，不能参与。")
		return
	}
	var prizes []model.LotteryPrize
	if err = storage.DB.Where("activity_id = ?", a.ID).Order("id asc").Find(&prizes).Error; err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, "奖品暂时无法读取，请稍后重试。")
		return
	}
	rule := fmt.Sprintf("满 %d 人开奖", a.ParticipantLimit)
	if a.DrawAt != nil {
		rule = "开奖时间：" + a.DrawAt.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
	}
	textOut := fmt.Sprintf("🎲 %s\n\n%s\n参与消耗：%d 积分\n确认后才扣分并报名，开奖后 24 小时内私聊领奖。\n\n奖品：", textfmt.LotteryDisplayText(a.Title, 80, "-"), rule, a.EntryCost)
	for _, p := range prizes {
		textOut += fmt.Sprintf("\n- %s × %d", textfmt.LotteryDisplayText(p.DisplayName, 80, "-"), p.Quantity)
	}
	m := tgbotapi.NewMessage(msg.Chat.ID, textOut)
	m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("确认参与", fmt.Sprintf("%s%d:%d", lotteryEntryCallbackPrefix, a.ID, msg.From.ID))))
	_, _ = telegram.SendNoAutoDelete(bot, m)
}

// App dispatch applies group/follow gates before calling this handler.
func HandleLotteryEntryCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, lotteryEntryCallbackPrefix) {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || !cb.Message.Chat.IsPrivate() || cb.Message.Chat.ID != cb.From.ID {
		telegram.AnswerCallback(bot, cb.ID, "请本人在 Bot 私聊中确认参与。")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(cb.Data, lotteryEntryCallbackPrefix), ":")
	if len(parts) != 2 {
		telegram.AnswerCallback(bot, cb.ID, "参与按钮无效。")
		return true
	}
	id, e1 := strconv.ParseUint(parts[0], 10, 32)
	owner, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || id == 0 || owner != cb.From.ID {
		telegram.AnswerCallback(bot, cb.ID, "参与按钮仅限本人操作。")
		return true
	}
	telegram.AnswerCallback(bot, cb.ID, "正在确认参与结果。")
	joinLotteryByID(bot, &tgbotapi.Message{From: cb.From, Chat: cb.Message.Chat}, uint(id))
	return true
}

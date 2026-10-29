package activity

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

// 面板更新串行读取最新快照，避免并发开箱的旧画面覆盖新画面；不持有资产锁发网络请求。
var treasurePanelMutex sync.Mutex

func treasureHuntErrorText(err error) string {
	for _, expected := range []error{ErrTreasureBusy, ErrTreasureDailyLimit, ErrTreasureCooldown, ErrTreasureExpired, ErrTreasurePicked, ErrTreasureBoxTaken, ErrTreasurePanel} {
		if errors.Is(err, expected) {
			return expected.Error()
		}
	}
	if errors.Is(err, storage.ErrPointsNotEnough) {
		return "积分不足，开箱需要 3 积分；本次未扣分"
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "本局不存在或面板已失效"
	}
	log.Printf("⚠️ 寻宝操作失败: %s", textfmt.FormatPlainError(err))
	return "寻宝暂时繁忙，本次操作未完成，请稍后重试"
}

func RenderTreasurePanel(round model.TreasureRound, picks []model.TreasurePick) (string, tgbotapi.InlineKeyboardMarkup, error) {
	rewards, err := TreasureRewards(round)
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	opened := make(map[int]model.TreasurePick, len(picks))
	spent, won := 0, 0
	for _, pick := range picks {
		opened[pick.BoxIndex] = pick
		spent += pick.Cost
		won += pick.Reward
	}
	var text strings.Builder
	fmt.Fprintf(&text, "🗺 群内寻宝 #%d\n\n每人限开一箱，每箱消耗 %d 积分。\n奖励：空箱×4、2分×3、5分×1、10分×1。\n开箱即到账，有赚有亏，最多亏 3 分。\n", round.ID, round.EntryCost)
	if round.Status == TreasureClosed {
		fmt.Fprintf(&text, "\n🏁 本局已结束 · 已开 %d/9 箱\n总消耗 %d 分，总奖励 %d 分。\n净回收 %d 分已进入天道奖池。\n", len(picks), spent, won, round.PoolInjected)
		if won > spent {
			fmt.Fprintf(&text, "本局系统补贴 %d 分，未扣天道奖池。\n", won-spent)
		}
	} else {
		fmt.Fprintf(&text, "\n⏳ 截止 %s（北京时间）· 已开 %d/9 箱\n点击下方未开启的宝箱即可开箱并扣分。\n", round.ExpiresAt.In(time.FixedZone("CST", 8*3600)).Format("15:04:05"), len(picks))
	}
	if len(picks) > 0 {
		text.WriteString("\n🔎 寻宝记录\n")
	}
	for _, pick := range picks {
		fmt.Fprintf(&text, "%d号 · %s：获得%d分（净%+d）\n", pick.BoxIndex+1, textfmt.LotteryDisplayText(pick.UserName, 24, "道友"), pick.Reward, pick.Reward-pick.Cost)
	}
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, 3)
	for row := 0; row < 3; row++ {
		buttons := make([]tgbotapi.InlineKeyboardButton, 0, 3)
		for column := 0; column < 3; column++ {
			index := row*3 + column
			label := fmt.Sprintf("🎁 %d · 3分", index+1)
			if pick, ok := opened[index]; ok {
				label = fmt.Sprintf("%d号 ✓ %d分", index+1, pick.Reward)
			} else if round.Status == TreasureClosed {
				label = fmt.Sprintf("%d号 未开·%d分", index+1, rewards[index])
			}
			buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("hunt:%d:%d", round.ID, index)))
		}
		rows = append(rows, buttons)
	}
	return text.String(), tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

func loadTreasurePanel(roundID uint) (model.TreasureRound, []model.TreasurePick, error) {
	var round model.TreasureRound
	if err := storage.DB.First(&round, roundID).Error; err != nil {
		return round, nil, err
	}
	var picks []model.TreasurePick
	if err := storage.DB.Where("round_id = ?", roundID).Order("id").Find(&picks).Error; err != nil {
		return round, nil, err
	}
	return round, picks, nil
}

func refreshTreasurePanel(bot *tgbotapi.BotAPI, roundID uint) {
	treasurePanelMutex.Lock()
	defer treasurePanelMutex.Unlock()
	round, picks, err := loadTreasurePanel(roundID)
	if err != nil {
		log.Printf("⚠️ 读取藏宝图失败: round=%d err=%s", roundID, textfmt.FormatPlainError(err))
		return
	}
	if round.MessageID <= 0 || (round.Status != TreasureActive && round.Status != TreasureClosed) {
		return
	}
	text, markup, err := RenderTreasurePanel(round, picks)
	if err != nil {
		log.Printf("⚠️ 渲染藏宝图失败: round=%d err=%s", roundID, textfmt.FormatPlainError(err))
		return
	}
	edit := tgbotapi.NewEditMessageText(round.ChatID, round.MessageID, text)
	edit.ReplyMarkup = &markup
	if _, err := bot.Request(edit); err != nil && !strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
		log.Printf("⚠️ 刷新藏宝图失败，资产结果以流水为准: round=%d err=%s", roundID, textfmt.FormatTelegramSendError(err))
	}
}

func finishTreasureRound(bot *tgbotapi.BotAPI, roundID uint, now time.Time) bool {
	closed, burst, err := CloseTreasureRound(roundID, now)
	if err != nil {
		log.Printf("⚠️ 寻宝结算失败，将重试: round=%d err=%s", roundID, textfmt.FormatPlainError(err))
		return false
	}
	if closed {
		refreshTreasurePanel(bot, roundID)
		if burst {
			var round model.TreasureRound
			if err := storage.DB.First(&round, roundID).Error; err == nil {
				telegram.NotifyFusionPoolBurst(bot, round.ChatID, "群内寻宝净回收积分注入天道")
			}
		}
	}
	return closed
}

func HandleTreasureHuntCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if text != "发起寻宝" && text != "寻宝" && text != "寻宝状态" {
		return false
	}
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return true
	}
	if msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "🗺 寻宝是群内共享玩法，请在群里发送："+textfmt.Copyable("发起寻宝"))
		return true
	}
	telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	ProcessExpiredTreasureRounds(bot, time.Now())
	if text == "寻宝状态" {
		var round model.TreasureRound
		if err := storage.DB.Where("chat_id = ? AND status IN ?", msg.Chat.ID, []string{TreasureActive, TreasureClosed}).Order("id DESC").First(&round).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				telegram.SendGroupAutoDeleteMessage(bot, msg.Chat.ID, "🗺 暂无藏宝图，发送 `发起寻宝` 开始一局。")
			} else {
				telegram.SendPlainText(bot, msg.Chat.ID, treasureHuntErrorText(err))
			}
			return true
		}
		refreshTreasurePanel(bot, round.ID)
		_, picks, err := loadTreasurePanel(round.ID)
		if err != nil {
			telegram.SendPlainText(bot, msg.Chat.ID, treasureHuntErrorText(err))
			return true
		}
		text, _, err := RenderTreasurePanel(round, picks)
		if err == nil {
			reply := tgbotapi.NewMessage(msg.Chat.ID, text+"\n开箱请点击原藏宝图中的宝箱。")
			reply.ReplyToMessageID = round.MessageID
			reply.AllowSendingWithoutReply = true
			if _, err := telegram.SendAutoDelete(bot, reply); err != nil {
				log.Printf("⚠️ 发送寻宝状态失败: %s", textfmt.FormatTelegramSendError(err))
			}
		}
		return true
	}
	round, err := CreateTreasureRound(msg.Chat.ID, time.Now())
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, treasureHuntErrorText(err))
		return true
	}
	text, markup, err := RenderTreasurePanel(round, nil)
	if err == nil {
		panel := tgbotapi.NewMessage(round.ChatID, text)
		panel.ReplyMarkup = markup
		var sent tgbotapi.Message
		sent, err = telegram.SendAutoDelete(bot, panel)
		if err == nil {
			err = ActivateTreasureRound(round.ID, sent.MessageID, time.Now())
		}
	}
	if err != nil {
		treasureMutex.Lock()
		res := storage.DB.Model(&model.TreasureRound{}).Where("id = ? AND status = ?", round.ID, treasurePublishing).Update("status", TreasureCancelled)
		treasureMutex.Unlock()
		if res.Error != nil {
			log.Printf("⚠️ 取消未发布藏宝图失败: round=%d err=%s", round.ID, textfmt.FormatPlainError(res.Error))
		}
		log.Printf("⚠️ 发布藏宝图失败，未扣除积分: round=%d err=%s", round.ID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, msg.Chat.ID, "藏宝图发布失败，本次未扣除积分，请稍后再试。")
	}
	return true
}

func HandleTreasureHuntCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, "hunt:") {
		return false
	}
	if cb.Message == nil || cb.Message.Chat == nil || cb.From == nil || cb.Message.Chat.IsPrivate() {
		telegram.AnswerCallback(bot, cb.ID, ErrTreasurePanel.Error())
		return true
	}

	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 {
		telegram.AnswerCallback(bot, cb.ID, ErrTreasurePanel.Error())
		return true
	}
	id, err := strconv.ParseUint(parts[1], 10, 32)
	box, boxErr := strconv.Atoi(parts[2])
	if err != nil || id == 0 || boxErr != nil {
		telegram.AnswerCallback(bot, cb.ID, ErrTreasurePanel.Error())
		return true
	}
	pick, err := PickTreasureBox(uint(id), cb.Message.Chat.ID, cb.Message.MessageID, box, cb.From)
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, treasureHuntErrorText(err))
		if errors.Is(err, ErrTreasurePanel) || errors.Is(err, gorm.ErrRecordNotFound) {
			return true
		}
	} else {
		telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("获得 %d 分，消耗 %d 分，净变化 %+d 分", pick.Reward, pick.Cost, pick.Reward-pick.Cost))
	}
	if !finishTreasureRound(bot, uint(id), time.Now()) {
		refreshTreasurePanel(bot, uint(id))
	}
	return true
}

func ProcessExpiredTreasureRounds(bot *tgbotapi.BotAPI, now time.Time) {
	// 发布未完成的局没有任何扣费，过期后释放群内名额。
	treasureMutex.Lock()
	err := storage.DB.Model(&model.TreasureRound{}).Where("status = ? AND expires_at <= ?", treasurePublishing, now).Update("status", TreasureCancelled).Error
	treasureMutex.Unlock()
	if err != nil {
		log.Printf("⚠️ 清理未发布藏宝图失败: %s", textfmt.FormatPlainError(err))
	}
	var rounds []model.TreasureRound
	if err := storage.DB.Where("status = ?", TreasureActive).Order("id").Find(&rounds).Error; err != nil {
		log.Printf("⚠️ 寻宝巡检失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, round := range rounds {
		finishTreasureRound(bot, round.ID, now)
	}
}

func StartTreasureHuntScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ 寻宝巡检异常: %s", textfmt.FormatPlainValue(r))
					}
				}()
				ProcessExpiredTreasureRounds(bot, time.Now())
			}()
			<-ticker.C
		}
	}()
}

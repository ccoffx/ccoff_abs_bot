package activity

import (
	"errors"
	"fmt"
	"log"
	"math"
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

type tribulationPanelLock struct {
	mu   sync.Mutex
	refs int
}

var tribulationPanelLocks = struct {
	sync.Mutex
	entries map[uint]*tribulationPanelLock
}{entries: make(map[uint]*tribulationPanelLock)}

func lockTribulationPanel(id uint) func() {
	tribulationPanelLocks.Lock()
	entry := tribulationPanelLocks.entries[id]
	if entry == nil {
		entry = &tribulationPanelLock{}
		tribulationPanelLocks.entries[id] = entry
	}
	entry.refs++
	tribulationPanelLocks.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		tribulationPanelLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(tribulationPanelLocks.entries, id)
		}
		tribulationPanelLocks.Unlock()
	}
}

func tribulationErrorText(err error) string {
	for _, expected := range []error{ErrTribulationBusy, ErrTribulationPanel, ErrTribulationExpired, ErrTribulationJoined, ErrTribulationJoinClosed, ErrTribulationSwitchDisabled, ErrTribulationTitleNotHeld} {
		if errors.Is(err, expected) {
			return expected.Error()
		}
	}
	if errors.Is(err, storage.ErrPointsNotEnough) {
		return "积分不足，入阵需要 5 积分；本次未扣分"
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "本局不存在或面板已失效"
	}
	log.Printf("⚠️ 避雷操作失败: %s", textfmt.FormatPlainError(err))
	return "操作暂未完成，请稍后重试；积分结果可在我的流水中确认"
}

func tribulationSideName(side string) string {
	if side == TribulationYang {
		return "☀️ 阳阵"
	}
	return "🌙 阴阵"
}

func RenderTribulationPanel(round model.TribulationRound, entries []model.TribulationEntry, now time.Time) (string, tgbotapi.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "⚡ 天雷猜阵 #%d\n\n入场 %d 积分，每人限入阵一次。\n成功到账 %d 分（净赚 %d），失败亏 %d 分。\n阴阳两阵获胜概率各 50%%，人数不影响概率，入阵后不能修改选择。\n", round.ID, round.EntryCost, round.EntryCost*2, round.EntryCost, round.EntryCost)
	counts := map[string]int{}
	for _, entry := range entries {
		counts[entry.Side]++
	}
	fmt.Fprintf(&b, "\n☀️ 阳阵：%d 人\n🌙 阴阵：%d 人\n", counts[TribulationYang], counts[TribulationYin])
	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	if round.Status == TribulationClosed {
		fmt.Fprintf(&b, "\n⚡ 天雷击中%s，%s避雷成功！\n成功 %d 人，失败 %d 人。\n", tribulationSideName(oppositeTribulationSide(round.WinningSide)), tribulationSideName(round.WinningSide), counts[round.WinningSide], len(entries)-counts[round.WinningSide])
		limit := len(entries)
		if limit > 12 {
			limit = 12
		}
		b.WriteString("\n📜 避雷结果\n")
		for _, entry := range entries[:limit] {
			fmt.Fprintf(&b, "%s：净%+d 分", textfmt.LotteryDisplayText(entry.UserName, 20, "道友"), entry.Reward-entry.Cost)
			b.WriteByte('\n')
		}
		if len(entries) > limit {
			fmt.Fprintf(&b, "其余 %d 人可点击下方查看本人结果。\n", len(entries)-limit)
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📜 我的结果", fmt.Sprintf("trib:result:%d", round.ID))))
	} else if round.Status == TribulationCancelled {
		b.WriteString("\n本局已取消，无人扣分。发送「猜阵」开始下一局。\n")
	} else if round.Status == tribulationPublishing {
		// 发送成功和激活入库之间可能失败或重启。未激活的面板不可招募，
		// 即使外部发送结果不确定、无法保存消息编号，也不会留下可点击的假入口。
		b.WriteString("\n⏳ 猜阵面板准备中，开放后才能入阵；尚未扣分。\n若一直未开放，请重新发送「猜阵」。\n")
	} else if round.Status == TribulationSettling || !now.Before(round.JoinUntil) || !now.Before(round.ExpiresAt) {
		b.WriteString("\n⚡ 天雷已至，正在结算，请稍候……\n")
	} else {
		fmt.Fprintf(&b, "\n⏳ 还有 %d 秒可以入阵，不限人数，一人也能玩。\n每局开放选阵 30 秒，选定后不能修改，到时直接开奖。\n", int(math.Ceil(round.JoinUntil.Sub(now).Seconds())))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("☀️ 阳阵 · 5积分", fmt.Sprintf("trib:join:%d:yang", round.ID)),
			tgbotapi.NewInlineKeyboardButtonData("🌙 阴阵 · 5积分", fmt.Sprintf("trib:join:%d:yin", round.ID)),
		))
	}
	if round.Status == TribulationActive || round.Status == tribulationPublishing {
		if len(entries) > 0 {
			b.WriteString("\n最近入阵\n")
			// 只展示最近六条，参与人数不受 Telegram 单条消息长度限制。
			start := len(entries) - 6
			if start < 0 {
				start = 0
			}
			for _, entry := range entries[start:] {
				fmt.Fprintf(&b, "%s加入%s\n", textfmt.LotteryDisplayText(entry.UserName, 20, "道友"), tribulationSideName(entry.Side))
			}
		}
	}
	// 保留空数组；库构造函数会将零行转换为 null，Telegram 会拒绝该格式。
	// 准备、结算和取消阶段均需要无按钮面板，编辑时空数组也会清除旧按钮。
	return b.String(), tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func oppositeTribulationSide(side string) string {
	if side == TribulationYang {
		return TribulationYin
	}
	return TribulationYang
}

func loadTribulationPanel(id uint) (model.TribulationRound, []model.TribulationEntry, error) {
	var round model.TribulationRound
	var entries []model.TribulationEntry
	// 同一快照读取，避免最终状态与结算前的 reward 混在一个面板里。
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&round, id).Error; err != nil {
			return err
		}
		return tx.Where("round_id = ?", id).Order("COALESCE(switched_at, joined_at), id").Find(&entries).Error
	})
	return round, entries, err
}

func refreshTribulationPanel(bot *tgbotapi.BotAPI, id uint, now func() time.Time) {
	unlock := lockTribulationPanel(id)
	defer unlock()
	round, entries, err := loadTribulationPanel(id)
	if err != nil {
		log.Printf("⚠️ 读取猜阵面板失败: %s", textfmt.FormatPlainError(err))
		return
	}
	if round.MessageID <= 0 {
		return
	}
	// 取得锁、读完快照后再取时钟，前一个群的慢请求不能令本群显示旧阶段。
	text, markup := RenderTribulationPanel(round, entries, now())
	edit := tgbotapi.NewEditMessageText(round.ChatID, round.MessageID, text)
	edit.ReplyMarkup = &markup
	if _, err := bot.Request(edit); err != nil && !strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
		if (round.Status == TribulationClosed || round.Status == TribulationCancelled) && telegram.IsTelegramMessageUnavailableForEdit(err) {
			// 群消息会自动清理。已确认删除的终局面板不再无限重试，
			// 历史结果仍可通过「猜阵状态」与积分流水读取。
			if saveErr := storage.DB.Model(&model.TribulationRound{}).Where("id = ? AND status = ?", id, round.Status).Update("panel_dirty", false).Error; saveErr != nil {
				log.Printf("⚠️ 保存猜阵面板清理状态失败: %s", textfmt.FormatPlainError(saveErr))
			}
			return
		}
		log.Printf("⚠️ 刷新猜阵面板失败，稍后重试: round=%d err=%s", id, textfmt.FormatTelegramSendError(err))
		return
	}
	// 状态在外部请求期间发生变化时，不清除新版面板的重试标记。
	if err := storage.DB.Model(&model.TribulationRound{}).Where("id = ? AND status = ?", id, round.Status).Update("panel_dirty", false).Error; err != nil {
		log.Printf("⚠️ 保存猜阵面板状态失败: %s", textfmt.FormatPlainError(err))
	}
}

func HandleTribulationCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	switch text {
	case "猜阵", "天雷猜阵", "发起猜阵", "猜阵状态":
	default:
		return false
	}
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return true
	}
	if msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "⚡ 请在群内发送「猜阵」，点击阴阳阵法参与。每次 5 积分，成功净赚 5 分，失败亏 5 分。")
		return true
	}
	telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	settleTribulationRounds(time.Now())
	if text == "猜阵状态" {
		var round model.TribulationRound
		if err := storage.DB.Where("chat_id = ? AND message_id > 0", msg.Chat.ID).Order("id DESC").First(&round).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				telegram.SendPlainText(bot, msg.Chat.ID, "暂无猜阵记录，发送「猜阵」开始一局。")
			} else {
				telegram.SendPlainText(bot, msg.Chat.ID, tribulationErrorText(err))
			}
			return true
		}
		refreshTribulationPanel(bot, round.ID, time.Now)
		snapshot, entries, err := loadTribulationPanel(round.ID)
		if err != nil {
			telegram.SendPlainText(bot, msg.Chat.ID, tribulationErrorText(err))
			return true
		}
		statusText, _ := RenderTribulationPanel(snapshot, entries, time.Now())
		reply := tgbotapi.NewMessage(msg.Chat.ID, statusText+"\n参与请点击原面板；个人积分结果也可在我的流水中确认。")
		reply.ReplyToMessageID = round.MessageID
		reply.AllowSendingWithoutReply = true
		if _, err := telegram.SendAutoDelete(bot, reply); err != nil {
			log.Printf("⚠️ 发送猜阵状态失败: %s", textfmt.FormatTelegramSendError(err))
		}
		return true
	}
	round, err := CreateTribulationRound(msg.Chat.ID, msg.Chat.Title, time.Now())
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, tribulationErrorText(err))
		return true
	}
	textOut, markup := RenderTribulationPanel(round, nil, time.Now())
	panel := tgbotapi.NewMessage(round.ChatID, textOut)
	panel.ReplyMarkup = markup
	sent, err := telegram.SendAutoDelete(bot, panel)
	if err == nil {
		err = ActivateTribulationRound(round.ID, sent.MessageID, time.Now())
	}
	if err == nil {
		// 立即开放新面板，不依赖其他群的后台编辑队列；同局编辑仍串行。
		refreshTribulationPanel(bot, round.ID, time.Now)
	}
	if err != nil {
		tribulationMutex.Lock()
		updates := map[string]any{"status": TribulationCancelled, "closed_at": time.Now()}
		if sent.MessageID > 0 {
			// 请求返回前可能已被超时巡检取消。仍需保存已发出的消息，
			// 让后台在编辑失败或重启后继续把它更新为取消状态。
			updates["message_id"] = sent.MessageID
			updates["panel_dirty"] = true
		}
		cancelErr := storage.DB.Model(&model.TribulationRound{}).Where("id = ? AND status IN ?", round.ID, []string{tribulationPublishing, TribulationCancelled}).Updates(updates).Error
		tribulationMutex.Unlock()
		if cancelErr != nil {
			log.Printf("⚠️ 取消未发布避雷失败: %s", textfmt.FormatPlainError(cancelErr))
		}
		log.Printf("⚠️ 发布避雷失败: %s", textfmt.FormatTelegramSendError(err))
		telegram.SendPlainText(bot, msg.Chat.ID, "猜阵面板发布失败，本次未扣分，请稍后重试。")
	}
	return true
}

func HandleTribulationCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, "trib:") {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || cb.Message.Chat.IsPrivate() {
		telegram.AnswerCallback(bot, cb.ID, ErrTribulationPanel.Error())
		return true
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 3 {
		telegram.AnswerCallback(bot, cb.ID, ErrTribulationPanel.Error())
		return true
	}
	id, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil || id == 0 {
		telegram.AnswerCallback(bot, cb.ID, ErrTribulationPanel.Error())
		return true
	}
	if parts[1] == "result" && len(parts) == 3 {
		round, entries, err := loadTribulationPanel(uint(id))
		if err != nil || round.ChatID != cb.Message.Chat.ID || round.MessageID != cb.Message.MessageID {
			telegram.AnswerCallback(bot, cb.ID, ErrTribulationPanel.Error())
			return true
		}
		if round.Status != TribulationClosed {
			telegram.AnswerCallback(bot, cb.ID, "本局尚未结算")
			return true
		}
		for _, entry := range entries {
			if entry.UserID == cb.From.ID {
				telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("本局投入 %d 分，到账 %d 分，净%+d 分", entry.Cost, entry.Reward, entry.Reward-entry.Cost))
				return true
			}
		}
		telegram.AnswerCallback(bot, cb.ID, "你未参与本局")
		return true
	}
	if len(parts) != 4 || (parts[1] != "join" && parts[1] != "switch") {
		telegram.AnswerCallback(bot, cb.ID, ErrTribulationPanel.Error())
		return true
	}
	_, err = ChooseTribulationSide(uint(id), cb.Message.Chat.ID, cb.Message.MessageID, parts[3], parts[1] == "switch", cb.From, time.Now())
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, tribulationErrorText(err))
	} else {
		telegram.AnswerCallback(bot, cb.ID, "入阵成功，扣除 5 积分；本局选择已锁定")
	}
	// 面板由独立巡检刷新，避免慢请求持有用户回调锁。
	return true
}

func ProcessTribulationRounds(bot *tgbotapi.BotAPI, now time.Time) {
	started := time.Now()
	// 先完成所有群的数据库结算，任何面板的网络请求都不能挡住其他群发奖。
	settleTribulationRounds(now)
	refreshTribulationPanels(bot, func() time.Time { return now.Add(time.Since(started)) })
}

func settleTribulationRounds(now time.Time) {
	tribulationMutex.Lock()
	err := storage.DB.Model(&model.TribulationRound{}).Where("status = ? AND expires_at <= ?", tribulationPublishing, now).Update("status", TribulationCancelled).Error
	tribulationMutex.Unlock()
	if err != nil {
		log.Printf("⚠️ 清理未发布避雷失败: %s", textfmt.FormatPlainError(err))
	}
	var rounds []model.TribulationRound
	if err := storage.DB.Where("status IN ?", []string{TribulationActive, TribulationSettling}).Order("id").Find(&rounds).Error; err != nil {
		log.Printf("⚠️ 避雷巡检失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, round := range rounds {
		if _, err := CloseTribulationRound(round.ID, now); err != nil {
			log.Printf("⚠️ 避雷结算失败，将按已保存结果重试: round=%d err=%s", round.ID, textfmt.FormatPlainError(err))
		}
	}
}

func refreshTribulationPanels(bot *tgbotapi.BotAPI, now func() time.Time) {
	var rounds []model.TribulationRound
	if err := storage.DB.Where("status IN ? OR panel_dirty = ?", []string{TribulationActive, TribulationSettling}, true).Order("id").Find(&rounds).Error; err != nil {
		log.Printf("⚠️ 猜阵面板巡检失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, round := range rounds {
		refreshTribulationPanel(bot, round.ID, now)
	}
}

func startTribulationTask(name string, task func()) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ 避雷%s巡检异常: %s", name, textfmt.FormatPlainValue(r))
					}
				}()
				task()
			}()
			<-ticker.C
		}
	}()
}

func StartTribulationScheduler(bot *tgbotapi.BotAPI) {
	startTribulationTask("结算", func() { settleTribulationRounds(time.Now()) })
	startTribulationTask("面板", func() { refreshTribulationPanels(bot, time.Now) })
}

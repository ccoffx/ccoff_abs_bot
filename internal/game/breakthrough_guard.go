package game

import (
	"encoding/json"
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

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	BreakthroughRitualPublishing = "publishing"
	BreakthroughRitualWaiting    = "waiting"
	BreakthroughRitualSettled    = "settled"
	BreakthroughRitualCancelled  = "cancelled"
	BreakthroughGuardCost        = 5
	BreakthroughGuardPayout      = 10 // Includes the five points paid on joining: net +5.
	BreakthroughGuardMax         = 5
	BreakthroughGuardDailyMax    = 3
	BreakthroughGuardWindow      = time.Minute
)

var (
	ErrBreakthroughRitualPending     = errors.New("您已有待结算的互道突破，请等待本局结束")
	ErrBreakthroughRitualClosed      = errors.New("本次互道已结束或尚未开放")
	ErrBreakthroughRitualNotDue      = errors.New("互道招募尚未结束")
	ErrBreakthroughGuardUnavailable  = errors.New("必成或保底突破不开放互道")
	ErrBreakthroughGuardIneligible   = errors.New("互道需要有效且未暂停的正式听书账号")
	ErrBreakthroughGuardSelf         = errors.New("不能为自己的突破互道")
	ErrBreakthroughGuardFull         = errors.New("本次互道人数已满")
	ErrBreakthroughGuardJoined       = errors.New("您已参加本次互道，请勿重复点击")
	ErrBreakthroughGuardDailyLimit   = errors.New("您今天已参加三次互道，请明天再来")
	ErrBreakthroughGuardRulesInvalid = errors.New("互道规则快照异常，本次突破取消")
	breakthroughGuardSweepMu         sync.Mutex
	breakthroughGuardPanelMu         sync.Mutex
)

func decodeBreakthroughRitualRules(ritual model.BreakthroughRitual) (BreakthroughSetting, error) {
	var rules BreakthroughSetting
	if err := json.Unmarshal([]byte(ritual.RulesJSON), &rules); err != nil {
		return rules, ErrBreakthroughGuardRulesInvalid
	}
	if rules.FromMajorRealm != ritual.FromMajor || rules.ToMajorRealm != rules.FromMajorRealm+1 || rules.FromMajorRealm < 1 || rules.MinHours <= 0 || rules.PointsCost <= 0 || rules.SuccessRate <= 0 || rules.SuccessRate > .95 || rules.Cooldown < time.Hour || rules.GuaranteeFailCount < 0 || rules.RefundRate < 0 || rules.RefundRate > 1 || rules.FailPenaltyPoints < 0 || strings.TrimSpace(rules.PillName) == "" {
		return rules, ErrBreakthroughGuardRulesInvalid
	}
	return rules, nil
}

func BreakthroughGuardSuccessRate(base float64, count int) float64 {
	base = math.Round(base*100) / 100
	if count <= 0 {
		return base
	}
	if count > BreakthroughGuardMax {
		count = BreakthroughGuardMax
	}
	return math.Min(.95, math.Round((base*100+float64(count)))/100)
}

func CreateBreakthroughRitual(userID int64, name string, originChatID, groupID int64, mode string, now time.Time) (*model.BreakthroughRitual, error) {
	var ritual model.BreakthroughRitual
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var active int64
		if err := tx.Model(&model.BreakthroughRitual{}).Where("user_id = ? AND status IN ?", userID, []string{BreakthroughRitualPublishing, BreakthroughRitualWaiting}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return ErrBreakthroughRitualPending
		}
		var cul model.Cultivation
		if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
			return err
		}
		req, ok := getBreakthroughSettingFromRules(cul.MajorRealm)
		if !ok || isMaxCultivationRealm(cul.MajorRealm) {
			return storage.ErrMaxRealmReached
		}
		if math.Round(req.SuccessRate*100) >= 100 || (req.GuaranteeFailCount > 0 && cul.TribulationFails >= req.GuaranteeFailCount) {
			return ErrBreakthroughGuardUnavailable
		}
		if groupID == 0 {
			return ErrBreakthroughGuardUnavailable
		}
		if now.Before(cul.ConsolidateUntil) {
			return storage.ErrConsolidating
		}
		if cul.MajorRealm != 0 && cul.MinorRealm != 4 {
			return storage.ErrBreakthroughNotReady
		}
		if cul.TotalAudioTime+cul.PillAudioTime < req.MinHours {
			return storage.ErrInsufficientCultivation
		}
		var user model.User
		if err := tx.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		usable, err := storage.UserHasUsableLocalAbsAccountTxAt(tx, user, now)
		if err != nil {
			return err
		}
		if !usable || !storage.IsFormalAccount(user) {
			return ErrBreakthroughGuardIneligible
		}
		if mode == "AUTO_BUY" {
			if req.PointsCost <= 0 || IsCultivationTreasureItemName(req.PillName) {
				return storage.ErrInvalidBreakthroughMode
			}
			if user.Points < req.PointsCost {
				return storage.ErrInsufficientPoints
			}
		} else if mode == "USE_INVENTORY" {
			if req.PointsCost > 0 {
				var item model.Inventory
				if err := tx.Where("user_id = ? AND item_name = ?", userID, req.PillName).First(&item).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return storage.ErrNoBreakthroughPill
					}
					return err
				}
				if item.Quantity < 1 {
					return storage.ErrNoBreakthroughPill
				}
				if IsCultivationTreasureItemName(req.PillName) && user.Points < req.PointsCost {
					return storage.ErrInsufficientPoints
				}
			}
		} else {
			return storage.ErrInvalidBreakthroughMode
		}
		payload, err := json.Marshal(req)
		if err != nil {
			return err
		}
		ritual = model.BreakthroughRitual{UserID: userID, UserName: name, OriginChatID: originChatID, ChatID: groupID, Mode: mode, FromMajor: cul.MajorRealm, TribulationFails: cul.TribulationFails, RulesJSON: string(payload), Status: BreakthroughRitualPublishing, Deadline: now.Add(BreakthroughGuardWindow)}
		return tx.Create(&ritual).Error
	})
	return &ritual, err
}

func ActivateBreakthroughRitual(id uint, messageID int, now time.Time) error {
	res := storage.DB.Model(&model.BreakthroughRitual{}).Where("id = ? AND status = ?", id, BreakthroughRitualPublishing).
		Updates(map[string]interface{}{"status": BreakthroughRitualWaiting, "message_id": messageID, "deadline": now.Add(BreakthroughGuardWindow)})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrBreakthroughRitualClosed
	}
	return nil
}

func JoinBreakthroughGuard(ritualID uint, chatID int64, messageID int, user *tgbotapi.User, now time.Time) error {
	if user == nil {
		return storage.ErrTelegramUserMissing
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var ritual model.BreakthroughRitual
		if err := tx.First(&ritual, ritualID).Error; err != nil {
			return err
		}
		if ritual.Status != BreakthroughRitualWaiting || !now.Before(ritual.Deadline) || ritual.ChatID != chatID || ritual.MessageID != messageID {
			return ErrBreakthroughRitualClosed
		}
		if ritual.UserID == user.ID {
			return ErrBreakthroughGuardSelf
		}
		var joined int64
		if err := tx.Model(&model.BreakthroughGuard{}).Where("ritual_id = ? AND user_id = ?", ritualID, user.ID).Count(&joined).Error; err != nil {
			return err
		}
		if joined > 0 {
			return ErrBreakthroughGuardJoined
		}
		if err := tx.Model(&model.BreakthroughGuard{}).Where("ritual_id = ? AND status = ?", ritualID, "joined").Count(&joined).Error; err != nil {
			return err
		}
		if joined >= BreakthroughGuardMax {
			return ErrBreakthroughGuardFull
		}
		var account model.User
		if err := tx.Where("telegram_id = ?", user.ID).First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrBreakthroughGuardIneligible
			}
			return err
		}
		usable, err := storage.UserHasUsableLocalAbsAccountTxAt(tx, account, now)
		if err != nil {
			return err
		}
		if !usable || !storage.IsFormalAccount(account) {
			return ErrBreakthroughGuardIneligible
		}
		var cul model.Cultivation
		if err := tx.Where("user_id = ?", user.ID).First(&cul).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrBreakthroughGuardIneligible
			}
			return err
		}
		if cul.MajorRealm < 1 {
			return ErrBreakthroughGuardIneligible
		}
		var dailyCount int64
		if err := tx.Model(&model.BreakthroughGuard{}).Where("user_id = ? AND day_key = ? AND status <> ?", user.ID, SectDayKey(now), "cancelled").Count(&dailyCount).Error; err != nil {
			return err
		}
		if dailyCount >= BreakthroughGuardDailyMax {
			return ErrBreakthroughGuardDailyLimit
		}
		name := user.UserName
		if name == "" {
			name = user.FirstName
		}
		guard := model.BreakthroughGuard{RitualID: ritualID, UserID: user.ID, UserName: name, DayKey: SectDayKey(now), Status: "joined"}
		if err := tx.Create(&guard).Error; err != nil {
			return err
		}
		if err := storage.ApplyPointDeltaInTx(tx, user.ID, -BreakthroughGuardCost, "breakthrough_guard_stake", "参与互道，投入5积分；成功到账10积分，失败不返还", "breakthrough_guard", fmt.Sprint(guard.ID)); err != nil {
			return err
		}
		if joined+1 == BreakthroughGuardMax {
			return tx.Model(&ritual).Update("deadline", now).Error
		}
		return nil
	})
}

func settleBreakthroughGuardPaymentsInTx(tx *gorm.DB, ritualID uint, status string, payout int) error {
	var guards []model.BreakthroughGuard
	if err := tx.Where("ritual_id = ? AND status = ?", ritualID, "joined").Order("id").Find(&guards).Error; err != nil {
		return err
	}
	for _, guard := range guards {
		if payout > 0 {
			txType, detail := "breakthrough_guard_reward", "互道成功，到账10积分（含投入的5积分），净赚5积分"
			if status == "cancelled" {
				txType, detail = "breakthrough_guard_refund", "突破取消，退还互道投入5积分"
			}
			if err := storage.ApplyPointDeltaInTx(tx, guard.UserID, payout, txType, detail, "breakthrough_guard", fmt.Sprint(guard.ID)); err != nil {
				// A deliberately deleted account cannot receive a refund or reward;
				// keep the settlement record without blocking everyone else's funds.
				if !errors.Is(err, storage.ErrUserNotFound) {
					return err
				}
			}
		}
		if err := tx.Model(&guard).Update("status", status).Error; err != nil {
			return err
		}
	}
	return nil
}

func settleBreakthroughRitualInTx(tx *gorm.DB, ritual *model.BreakthroughRitual, attemptID uint, success bool) error {
	if ritual == nil {
		return nil
	}
	payout, status := 0, "failed"
	if success {
		payout, status = BreakthroughGuardPayout, "success"
	}
	if err := settleBreakthroughGuardPaymentsInTx(tx, ritual.ID, status, payout); err != nil {
		return err
	}
	res := tx.Model(ritual).Where("status = ?", BreakthroughRitualWaiting).Updates(map[string]interface{}{"status": BreakthroughRitualSettled, "attempt_id": attemptID})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrBreakthroughRitualClosed
	}
	return storage.WriteAuditLogInTx(tx, ritual.UserID, "SETTLE_BREAKTHROUGH_GUARDS", fmt.Sprint(ritual.ID), 0, fmt.Sprintf("attempt=%d success=%t", attemptID, success))
}

func CancelBreakthroughRitual(id uint, ownerID int64) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var ritual model.BreakthroughRitual
		if err := tx.First(&ritual, id).Error; err != nil {
			return err
		}
		if ritual.UserID != ownerID {
			return ErrBreakthroughGuardSelf
		}
		if ritual.Status == BreakthroughRitualCancelled {
			return nil
		}
		if ritual.Status != BreakthroughRitualPublishing && ritual.Status != BreakthroughRitualWaiting {
			return ErrBreakthroughRitualClosed
		}
		if err := settleBreakthroughGuardPaymentsInTx(tx, id, "cancelled", BreakthroughGuardCost); err != nil {
			return err
		}
		return tx.Model(&ritual).Update("status", BreakthroughRitualCancelled).Error
	})
}

func isBreakthroughCancellationError(err error) bool {
	for _, target := range []error{gorm.ErrRecordNotFound, storage.ErrCultivationNotFound, storage.ErrMaxRealmReached, storage.ErrConsolidating, storage.ErrBreakthroughNotReady, storage.ErrInsufficientCultivation, storage.ErrInsufficientPoints, storage.ErrNoBreakthroughPill, storage.ErrInvalidBreakthroughMode, storage.ErrCultivationStateChanged, storage.ErrUserNotFound, ErrBreakthroughGuardIneligible, ErrBreakthroughGuardRulesInvalid} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func formatBreakthroughRitualPanel(ritual model.BreakthroughRitual, guards []model.BreakthroughGuard) string {
	var rules BreakthroughSetting
	if err := json.Unmarshal([]byte(ritual.RulesJSON), &rules); err != nil {
		return "互道规则读取失败，请稍后再试。"
	}
	text := fmt.Sprintf("⚡ 互道招募 #%d\n\n道友：%s\n基础成功率：%.0f%% → 当前成功率：%.0f%%\n互道人数：%d/%d（每人 +1 个百分点，最高95%%）\n截止：%s（北京时间）；满5人提前渡劫\n\n参与先扣5积分；成功到账10积分，净赚5分；失败不返还，净损失5分。每人每天最多3次。\n未能执行突破时，互道投入原额退还。", ritual.ID, ritual.UserName, BreakthroughGuardSuccessRate(rules.SuccessRate, 0)*100, BreakthroughGuardSuccessRate(rules.SuccessRate, len(guards))*100, len(guards), BreakthroughGuardMax, ritual.Deadline.In(storage.DailyOperationsLocation).Format("15:04:05"))
	for _, guard := range guards {
		text += "\n🤝 " + guard.UserName
	}
	return text
}

func breakthroughRitualMarkup(id uint) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🤝 互道（投入5积分）", fmt.Sprintf("btguard:%d:join", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("立即渡劫（仅发起人）", fmt.Sprintf("btguard:%d:start", id)), tgbotapi.NewInlineKeyboardButtonData("取消突破", fmt.Sprintf("btguard:%d:cancel", id))),
	)
}

func BeginBreakthroughWithGuards(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, mode string) {
	groupID := int64(0)
	if config.AppConfig != nil {
		groupID = config.AppConfig.NoticeGroupID
	}
	name := msg.From.UserName
	if name == "" {
		name = msg.From.FirstName
	}
	ritual, err := CreateBreakthroughRitual(msg.From.ID, name, msg.Chat.ID, groupID, mode, time.Now())
	if errors.Is(err, ErrBreakthroughGuardUnavailable) {
		ExecuteBreakthrough(bot, msg, mode)
		return
	}
	if err != nil {
		if errors.Is(err, ErrBreakthroughRitualPending) || errors.Is(err, ErrBreakthroughGuardIneligible) {
			telegram.ReplyTextToMessage(bot, msg, err.Error())
		} else {
			telegram.ReplyTextToMessage(bot, msg, "❌ 无法发起突破，请检查修为、冷却和资源后重试。")
		}
		return
	}
	panel := tgbotapi.NewMessage(groupID, formatBreakthroughRitualPanel(*ritual, nil))
	panel.ReplyMarkup = breakthroughRitualMarkup(ritual.ID)
	sent, err := bot.Send(panel)
	if err == nil {
		err = ActivateBreakthroughRitual(ritual.ID, sent.MessageID, time.Now())
	}
	if err != nil {
		if cancelErr := CancelBreakthroughRitual(ritual.ID, ritual.UserID); cancelErr != nil {
			log.Printf("互道招募发布后取消失败: ritual=%d err=%s", ritual.ID, textfmt.FormatPlainError(cancelErr))
		}
		telegram.ReplyTextToMessage(bot, msg, "❌ 互道招募未能完成，突破未执行、资源未扣除，请稍后重试。")
		return
	}
	telegram.ReplyTextToMessage(bot, msg, "⚡ 已在交流群发起60秒互道招募，结束后自动渡劫；无人互道也会按基础成功率执行。可在群面板立即渡劫或取消。")
}

func refreshBreakthroughRitualPanel(bot *tgbotapi.BotAPI, id uint, result *BreakthroughOutcome) {
	breakthroughGuardPanelMu.Lock()
	defer breakthroughGuardPanelMu.Unlock()
	var ritual model.BreakthroughRitual
	if err := storage.DB.First(&ritual, id).Error; err != nil {
		return
	}
	if ritual.MessageID == 0 {
		return
	}
	if ritual.Status == BreakthroughRitualSettled && result == nil {
		var attempt model.BreakthroughAttempt
		if err := storage.DB.First(&attempt, ritual.AttemptID).Error; err != nil {
			return
		}
		result = &BreakthroughOutcome{IsSuccess: attempt.Result == "success", GuardCount: attempt.GuardCount, SuccessRate: attempt.SuccessRate, Roll: attempt.Roll, AttemptID: attempt.ID}
	}
	var guards []model.BreakthroughGuard
	if err := storage.DB.Where("ritual_id = ?", id).Order("id").Find(&guards).Error; err != nil {
		return
	}
	text := formatBreakthroughRitualPanel(ritual, guards)
	markup := tgbotapi.NewInlineKeyboardMarkup()
	if ritual.Status == BreakthroughRitualWaiting {
		markup = breakthroughRitualMarkup(id)
	} else if ritual.Status == BreakthroughRitualCancelled {
		text = fmt.Sprintf("互道突破 #%d 已取消，参与积分已退还。", id)
	} else if result != nil {
		outcome := "失败；互道者各损失投入的5积分"
		if result.IsSuccess {
			outcome = "成功；互道者各到账10积分（净赚5积分）"
		}
		text = fmt.Sprintf("⚡ 互道突破 #%d 已结算\n道友：%s\n结果：%s\n互道人数：%d\n最终成功率：%.0f%%，掷点：%d/100\n突破记录：#%d", id, ritual.UserName, outcome, result.GuardCount, result.SuccessRate*100, result.Roll, result.AttemptID)
	}
	edit := tgbotapi.NewEditMessageText(ritual.ChatID, ritual.MessageID, text)
	edit.ReplyMarkup = &markup
	if _, err := bot.Send(edit); err != nil {
		log.Printf("互道面板刷新失败: ritual=%d err=%s", id, textfmt.FormatTelegramSendError(err))
	}
}

func finishBreakthroughRitualPanel(bot *tgbotapi.BotAPI, id uint, result *BreakthroughOutcome) {
	refreshBreakthroughRitualPanel(bot, id, result)
}

func HandleBreakthroughGuardCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, "btguard:") {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil {
		return true
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 {
		telegram.AnswerCallback(bot, cb.ID, "无效互道面板")
		return true
	}
	parsed, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || parsed == 0 {
		telegram.AnswerCallback(bot, cb.ID, "无效互道面板")
		return true
	}
	id := uint(parsed)
	var ritual model.BreakthroughRitual
	if err := storage.DB.First(&ritual, id).Error; err != nil {
		telegram.AnswerCallback(bot, cb.ID, "互道状态暂时读取失败，请稍后重试")
		return true
	}
	if ritual.ChatID != cb.Message.Chat.ID || ritual.MessageID != cb.Message.MessageID {
		telegram.AnswerCallback(bot, cb.ID, "请在原始交流群面板操作")
		return true
	}
	if parts[2] == "join" {
		err = JoinBreakthroughGuard(id, ritual.ChatID, ritual.MessageID, cb.From, time.Now())
	} else if parts[2] == "cancel" && cb.From.ID == ritual.UserID {
		err = CancelBreakthroughRitual(id, cb.From.ID)
	} else if parts[2] == "start" && cb.From.ID == ritual.UserID {
		res := storage.DB.Model(&ritual).Where("status = ?", BreakthroughRitualWaiting).Update("deadline", time.Now())
		err = res.Error
		if err == nil && res.RowsAffected != 1 {
			err = ErrBreakthroughRitualClosed
		}
	} else {
		telegram.AnswerCallback(bot, cb.ID, "此操作仅限突破发起人")
		return true
	}
	if err != nil {
		message := "操作暂时失败，未扣除积分，请稍后重试"
		for _, known := range []error{ErrBreakthroughRitualClosed, ErrBreakthroughGuardSelf, ErrBreakthroughGuardFull, ErrBreakthroughGuardJoined, ErrBreakthroughGuardDailyLimit, ErrBreakthroughGuardIneligible} {
			if errors.Is(err, known) {
				message = known.Error()
				break
			}
		}
		if errors.Is(err, storage.ErrPointsNotEnough) {
			message = "积分不足，互道需投入5积分"
		}
		telegram.AnswerCallback(bot, cb.ID, message)
		return true
	}
	message := "操作成功"
	if parts[2] == "join" {
		message = "互道成功加入，已扣5分；突破成功到账10分，失败不返还"
	}
	telegram.AnswerCallback(bot, cb.ID, message)
	refreshBreakthroughRitualPanel(bot, id, nil)
	return true
}

func ProcessDueBreakthroughRituals(bot *tgbotapi.BotAPI, now time.Time) {
	if !breakthroughGuardSweepMu.TryLock() {
		return
	}
	defer breakthroughGuardSweepMu.Unlock()
	var rituals []model.BreakthroughRitual
	if err := storage.DB.Where("(status = ? AND deadline <= ?) OR (status = ? AND created_at <= ?)", BreakthroughRitualWaiting, now, BreakthroughRitualPublishing, now.Add(-5*time.Minute)).Order("id").Limit(30).Find(&rituals).Error; err != nil {
		log.Printf("互道到期巡检失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, ritual := range rituals {
		if ritual.Status == BreakthroughRitualPublishing {
			if err := CancelBreakthroughRitual(ritual.ID, ritual.UserID); err != nil {
				log.Printf("互道未发布活动回收失败: ritual=%d err=%s", ritual.ID, textfmt.FormatPlainError(err))
			}
			continue
		}
		message := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: ritual.OriginChatID}, From: &tgbotapi.User{ID: ritual.UserID, FirstName: ritual.UserName}}
		executeBreakthrough(bot, message, ritual.Mode, ritual.ID)
	}
}

func StartBreakthroughGuardScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.Printf("互道巡检异常: %v", recovered)
					}
				}()
				ProcessDueBreakthroughRituals(bot, now)
			}()
		}
	}()
}

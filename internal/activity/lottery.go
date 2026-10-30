package activity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/announcements"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	lotteryModeCount = "count"
	lotteryModeTime  = "time"

	lotteryStatusActive  = "active"
	lotteryStatusDrawing = "drawing"
	lotteryStatusDrawn   = "drawn"
	lotteryStatusClosed  = "closed"

	lotteryWinnerPending = "pending"
	lotteryWinnerClaimed = "claimed"
	lotteryWinnerExpired = "expired"

	lotteryPrizePoints     = "points"
	lotteryPrizeInvite     = "invite"
	lotteryPrizeRenew      = "renew"
	lotteryPrizeWhitelist  = "whitelist"
	lotteryPrizePill       = "pill"
	lotteryPrizeManual     = "manual_contact"
	lotteryPrizeCustomCode = "custom_code"

	lotteryClaimValidDuration = 24 * time.Hour

	lotteryCustomCodeMaxRunes = 200
	lotteryPrizeNameMaxRunes  = 80
)

func createLotteryClaimLogInTx(tx *gorm.DB, logEntry *model.LotteryClaimLog) error {
	if tx == nil || logEntry == nil {
		return fmt.Errorf("LOTTERY_CLAIM_LOG_INVALID")
	}
	entry := *logEntry
	entry.Action = textfmt.FormatPlainValue(entry.Action)
	entry.Detail = textfmt.FormatPlainValue(entry.Detail)
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("LOTTERY_CLAIM_LOG_CREATE_MISSED")
	}
	return nil
}

func createLotteryActivityInTx(tx *gorm.DB, activity *model.LotteryActivity) error {
	if tx == nil || activity == nil {
		return fmt.Errorf("LOTTERY_ACTIVITY_INVALID")
	}
	entry := *activity
	if config.AppConfig != nil && entry.AnnounceChatID == 0 {
		entry.AnnounceChatID = announcements.Target()
		entry.AnnounceIsChannel = config.AppConfig.AnnouncementChannelID != 0
		if entry.AnnounceIsChannel {
			entry.AnnounceReminderGroupID = config.AppConfig.NoticeGroupID
		}
	}
	entry.Title = textfmt.LotteryDisplayText(entry.Title, 80, "-")
	entry.Mode = textfmt.FormatPlainValue(entry.Mode)
	entry.Status = textfmt.FormatPlainValue(entry.Status)
	entry.CreatedBy = textfmt.FormatPlainValue(entry.CreatedBy)
	entry.ResultNote = textfmt.FormatPlainValue(entry.ResultNote)
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("LOTTERY_ACTIVITY_CREATE_MISSED")
	}
	*activity = entry
	return nil
}

func createLotteryPrizeInTx(tx *gorm.DB, prize *model.LotteryPrize) error {
	if tx == nil || prize == nil {
		return fmt.Errorf("LOTTERY_PRIZE_INVALID")
	}
	entry := *prize
	entry.PrizeType = textfmt.FormatPlainValue(entry.PrizeType)
	entry.DisplayName = textfmt.LotteryDisplayText(entry.DisplayName, 80, "")
	if entry.PrizeType == lotteryPrizeCustomCode {
		if entry.Quantity != 1 || !strings.HasPrefix(strings.TrimSpace(entry.SecretEncrypted), "gcm$") {
			return fmt.Errorf("LOTTERY_CUSTOM_CODE_PRIZE_INVALID")
		}
	} else if strings.TrimSpace(entry.SecretEncrypted) != "" {
		return fmt.Errorf("LOTTERY_PRIZE_UNEXPECTED_SECRET")
	}
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("LOTTERY_PRIZE_CREATE_MISSED")
	}
	*prize = entry
	return nil
}

func createLotteryParticipantInTx(tx *gorm.DB, participant *model.LotteryParticipant) error {
	if tx == nil || participant == nil {
		return fmt.Errorf("LOTTERY_PARTICIPANT_INVALID")
	}
	entry := *participant
	entry.UserName = textfmt.FormatPlainValue(entry.UserName)
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrLotteryAlreadyJoined
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("LOTTERY_PARTICIPANT_CREATE_MISSED")
	}
	*participant = entry
	return nil
}

func createLotteryWinnerInTx(tx *gorm.DB, winner *model.LotteryWinner) (bool, error) {
	if tx == nil || winner == nil {
		return false, fmt.Errorf("LOTTERY_WINNER_INVALID")
	}
	entry := *winner
	entry.UserName = textfmt.FormatPlainValue(entry.UserName)
	entry.PrizeType = textfmt.FormatPlainValue(entry.PrizeType)
	entry.Status = textfmt.FormatPlainValue(entry.Status)
	entry.PrizeCodePreview = textfmt.FormatPlainValue(entry.PrizeCodePreview)
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return false, nil
		}
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, fmt.Errorf("LOTTERY_WINNER_CREATE_MISSED")
	}
	*winner = entry
	return true, nil
}

func createLotteryLocalUserIfMissingInTx(tx *gorm.DB, user *model.User) error {
	if tx == nil || user == nil {
		return fmt.Errorf("LOTTERY_LOCAL_USER_INVALID")
	}
	entry := *user
	entry.Username = textfmt.FormatPlainValue(entry.Username)
	if entry.TelegramID == 0 {
		return fmt.Errorf("LOTTERY_LOCAL_USER_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*user = entry
	return nil
}

type lotteryPrizeSpec struct {
	PrizeType       string
	Amount          int
	Quantity        int
	DisplayName     string
	SecretValue     string
	SecretEncrypted string
	SecretPreview   string
}

type lotteryDrawWinnerInfo struct {
	UserID      int64
	UserName    string
	PrizeType   string
	Amount      int
	DisplayName string
}

func HandleLotteryCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string, session *telegram.SessionState, role string) bool {
	if bot == nil || msg == nil || msg.From == nil || msg.Chat == nil || session == nil {
		return false
	}

	step := session.GetStep()
	if strings.HasPrefix(step, "WAITING_LOTTERY_") {
		handleLotteryCreateStep(bot, msg, text, session, role)
		return true
	}

	userID := msg.From.ID
	chatID := msg.Chat.ID
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}

	if !msg.Chat.IsPrivate() {
		switch {
		case trimmed == "/lottery" || trimmed == "积分抽奖" || trimmed == "抽奖列表":
			telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
			showActiveLotteries(bot, chatID)
			return true
		}
		return false
	}

	if role == "super_admin" {
		switch {
		case trimmed == "🎲 积分抽奖" || trimmed == "抽奖管理":
			showLotteryAdminHelp(bot, chatID)
			return true
		case trimmed == "创建积分抽奖" || trimmed == "创建抽奖":
			startLotteryCreateWizard(bot, msg, session)
			return true
		case strings.HasPrefix(trimmed, "抽奖详情"):
			handleLotteryDetailCommand(bot, chatID, trimmed)
			return true
		case strings.HasPrefix(trimmed, "强制开奖"):
			handleForceDrawLotteryCommand(bot, chatID, userID, trimmed)
			return true
		case strings.HasPrefix(trimmed, "取消抽奖"):
			handleCancelLotteryCommand(bot, chatID, userID, trimmed)
			return true
		}
	}

	switch {
	case trimmed == "/lottery" || trimmed == "积分抽奖" || trimmed == "抽奖列表":
		showLotteryList(bot, chatID, role == "super_admin")
		return true
	default:
		return false
	}
}

func startLotteryCreateWizard(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, session *telegram.SessionState) {
	if !telegram.RequireSuperAdmin(bot, msg.Chat.ID, msg.From.ID) {
		return
	}
	session.SetTemp("lottery_title", "积分抽奖 "+time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05"))
	session.SetStep("WAITING_LOTTERY_MODE")
	telegram.SendPlainText(bot, msg.Chat.ID, "🎲 创建积分抽奖\n\n活动名称自动生成，用户通过按钮参与和领奖。\n第一步：请选择开奖模式。\n\n发送："+textfmt.Copyable("人数")+"\n或发送："+textfmt.Copyable("时间")+"\n发送“"+textfmt.Copyable("取消")+"”可退出。")
	telegram.UserSessions.Store(msg.From.ID, session)
}

func showLotteryAdminHelp(bot *tgbotapi.BotAPI, chatID int64) {
	commands := []string{"创建积分抽奖", "抽奖列表", "抽奖详情 1", "强制开奖 1", "取消抽奖 1"}
	var b strings.Builder
	b.WriteString("🎲 积分抽奖管理\n\n可用命令（示例编号请替换）：\n")
	for _, command := range commands {
		b.WriteString(textfmt.Copyable(command) + "\n")
	}
	b.WriteString("\n用户点击活动按钮参与，中奖后点击「领取奖励」按钮私聊领奖。")
	telegram.SendPlainText(bot, chatID, b.String())
}

func handleLotteryCreateStep(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string, session *telegram.SessionState, role string) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	text = strings.TrimSpace(text)

	if text == "取消" {
		telegram.ClearSession(userID)
		telegram.SendMenu(bot, chatID, "已取消创建积分抽奖。", telegram.SuperAdminMenu)
		return
	}

	if role != "super_admin" {
		telegram.ClearSession(userID)
		telegram.SendPlainText(bot, chatID, "❌ 权限不足：创建抽奖仅限超级管理员。")
		return
	}

	switch session.GetStep() {
	case "WAITING_LOTTERY_MODE":
		if text == "人数" || text == "1" {
			session.SetTemp("lottery_mode", lotteryModeCount)
			session.SetStep("WAITING_LOTTERY_LIMIT")
			telegram.SendPlainText(bot, chatID, "第二步：请输入满多少人开奖，范围 2-100000：")
			return
		}
		if text == "时间" || text == "2" {
			session.SetTemp("lottery_mode", lotteryModeTime)
			session.SetStep("WAITING_LOTTERY_DRAW_MINUTES")
			telegram.SendPlainText(bot, chatID, "第二步：请输入开奖时间。\n\n支持两种格式：\n"+"1. 分钟数"+"，例如："+textfmt.Copyable("60")+"\n2. 固定时间，例如："+textfmt.Copyable("2026-06-07 20:00:00"))
			return
		}
		telegram.SendPlainText(bot, chatID, "模式不识别，请发送“"+textfmt.Copyable("人数")+"”或“"+textfmt.Copyable("时间")+"”。")

	case "WAITING_LOTTERY_LIMIT":
		limit, err := strconv.Atoi(text)
		if err != nil || limit < 2 || limit > 100000 {
			telegram.SendPlainText(bot, chatID, "人数限制需为 2-100000 的整数，请重新输入：")
			return
		}
		session.SetTemp("lottery_limit", strconv.Itoa(limit))
		session.SetStep("WAITING_LOTTERY_ENTRY_COST")
		telegram.SendPlainText(bot, chatID, "第三步：请输入参与消耗积分，0 表示免费，范围 0-10000：")

	case "WAITING_LOTTERY_DRAW_MINUTES":
		drawAt, drawDesc, err := textfmt.ParseLotteryDrawTimeInput(text, time.Now())
		if err != nil {
			telegram.SendPlainText(bot, chatID, textfmt.FormatPlainError(err))
			return
		}
		session.SetTemp("lottery_draw_at", strconv.FormatInt(drawAt.Unix(), 10))
		session.SetTemp("lottery_draw_desc", drawDesc)
		session.SetStep("WAITING_LOTTERY_ENTRY_COST")
		telegram.SendPlainText(bot, chatID, "第三步：请输入参与消耗积分，0 表示免费，范围 0-10000：")

	case "WAITING_LOTTERY_ENTRY_COST":
		entryCost, err := strconv.Atoi(text)
		if err != nil || entryCost < 0 || entryCost > 10000 {
			telegram.SendPlainText(bot, chatID, "参与消耗积分需为 0-10000 的整数，请重新输入：")
			return
		}
		session.SetTemp("lottery_entry_cost", strconv.Itoa(entryCost))
		session.SetStep("WAITING_LOTTERY_PRIZES")
		telegram.SendPlainText(bot, chatID,
			"第四步：请在同一条消息中发送全部奖品配置，每行一种，可混合多种类型。每位中奖者随机获得一份奖品，全部奖品合计最多 100 份。\n\n"+
				"格式示例（可整块复制后修改）：\n"+
				textfmt.Copyable("积分 100 3\n续期 30 2\n邀请码 1\n白名单 1\n丹药 回气丹 1\n人工 实体纪念品 2\n卡密 Steam礼品卡|AAAA-BBBB-CCCC")+"\n\n"+
				"说明：\n"+
				"积分 100 3 = 100 积分奖品 3 份\n"+
				"续期 30 2 = 30 天续期卡 2 份\n"+
				"邀请码 1 = 邀请码 1 份\n"+
				"白名单 1 = 永久白名单卡 1 份，领取后通过「兑换白名单」使用\n"+
				"邀请码、续期卡、白名单卡均在领奖时自动生成；创建及开奖时不生成\n"+
				"丹药 回气丹 1 = 回气丹 1 份\n"+
				"人工 实体纪念品 2 = 中奖者点击「领取奖励」按钮确认后联系管理员领奖\n"+
				"卡密 奖品名称|卡密内容 = 每行导入一份自定义卡密；卡密只加密保存，中奖者点击「领取奖励」按钮私聊领取")

	case "WAITING_LOTTERY_PRIZES":
		specs, err := parseLotteryPrizeSpecs(text)
		if err != nil {
			telegram.SendPlainText(bot, chatID, "奖品配置有误："+textfmt.FormatPlainError(err)+"\n\n请重新发送奖品配置。")
			return
		}
		session.SetTemp("lottery_prizes", text)
		session.SetStep("WAITING_LOTTERY_CONFIRM")
		telegram.SendPlainText(bot, chatID, formatLotteryCreateConfirm(session, specs))

	case "WAITING_LOTTERY_CONFIRM":
		if text != "确认创建抽奖" {
			telegram.ClearSession(userID)
			telegram.SendPlainText(bot, chatID, "已取消创建积分抽奖。")
			return
		}
		activityID, err := createLotteryActivityFromSession(msg, session)
		if err != nil {
			telegram.ClearSession(userID)
			telegram.SendPlainText(bot, chatID, "❌ 创建失败："+textfmt.FormatPlainError(err))
			return
		}
		telegram.ClearSession(userID)
		telegram.SendPlainText(bot, chatID, fmt.Sprintf("✅ 积分抽奖创建成功，活动 ID：%d\n\n用户点击活动按钮参与和领奖。", activityID))
		announceLotteryCreated(bot, chatID, activityID)
	}
}

func parseLotteryPrizeSpecs(raw string) ([]lotteryPrizeSpec, error) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	specs := make([]lotteryPrizeSpec, 0, len(lines))
	totalQuantity := 0
	seenCustomCodes := make(map[string]struct{})

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("每行至少需要奖品类型和数量或内容")
		}

		switch fields[0] {
		case "积分":
			if len(fields) != 3 {
				return nil, fmt.Errorf("积分格式应为：积分 100 3")
			}
			amount, err := strconv.Atoi(fields[1])
			if err != nil || amount < 1 || amount > 100000 {
				return nil, fmt.Errorf("积分奖品数量需为 1-100000")
			}
			qty, err := strconv.Atoi(fields[2])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("积分奖品份数需为 1-100")
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizePoints,
				Amount:      amount,
				Quantity:    qty,
				DisplayName: fmt.Sprintf("%d 积分", amount),
			})
			totalQuantity += qty

		case "续期":
			if len(fields) != 3 {
				return nil, fmt.Errorf("续期格式应为：续期 30 2")
			}
			days, err := strconv.Atoi(fields[1])
			if err != nil || days < 1 || days > 365 {
				return nil, fmt.Errorf("续期天数需为 1-365")
			}
			qty, err := strconv.Atoi(fields[2])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("续期卡份数需为 1-100")
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizeRenew,
				Amount:      days,
				Quantity:    qty,
				DisplayName: fmt.Sprintf("%d 天续期卡", days),
			})
			totalQuantity += qty

		case "邀请码":
			if len(fields) != 2 {
				return nil, fmt.Errorf("邀请码格式应为：邀请码 1")
			}
			qty, err := strconv.Atoi(fields[1])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("邀请码份数需为 1-100")
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizeInvite,
				Amount:      1,
				Quantity:    qty,
				DisplayName: "邀请码",
			})
			totalQuantity += qty

		case "白名单", "白名单卡":
			if len(fields) != 2 {
				return nil, fmt.Errorf("白名单格式应为：白名单 1")
			}
			qty, err := strconv.Atoi(fields[1])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("白名单卡份数需为 1-100")
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizeWhitelist,
				Amount:      1,
				Quantity:    qty,
				DisplayName: "永久白名单卡",
			})
			totalQuantity += qty

		case "丹药":
			if len(fields) != 3 {
				return nil, fmt.Errorf("丹药格式应为：丹药 回气丹 1")
			}
			itemName := strings.TrimSpace(fields[1])
			if !isLotteryPillPrizeName(itemName) {
				return nil, fmt.Errorf("不支持的丹药：%s", itemName)
			}
			qty, err := strconv.Atoi(fields[2])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("丹药份数需为 1-100")
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizePill,
				Amount:      1,
				Quantity:    qty,
				DisplayName: itemName,
			})
			totalQuantity += qty

		case "人工", "人工领奖":
			if len(fields) < 3 {
				return nil, fmt.Errorf("人工领奖格式应为：人工 实体纪念品 2")
			}
			qty, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil || qty < 1 || qty > 100 {
				return nil, fmt.Errorf("人工领奖奖品份数需为 1-100")
			}
			name := strings.Join(fields[1:len(fields)-1], " ")
			if !validLotteryPrizeName(name) {
				return nil, fmt.Errorf("人工领奖奖品名称需为 1-%d 个字且不能包含控制字符", lotteryPrizeNameMaxRunes)
			}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:   lotteryPrizeManual,
				Amount:      1,
				Quantity:    qty,
				DisplayName: name,
			})
			totalQuantity += qty

		case "卡密", "自定义卡密":
			payload := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
			parts := strings.SplitN(payload, "|", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("自定义卡密格式应为：卡密 Steam礼品卡|AAAA-BBBB-CCCC")
			}
			name := strings.TrimSpace(parts[0])
			secret := strings.TrimSpace(parts[1])
			if !validLotteryPrizeName(name) {
				return nil, fmt.Errorf("自定义卡密奖品名称需为 1-%d 个字且不能包含控制字符", lotteryPrizeNameMaxRunes)
			}
			if !validLotteryCustomCode(secret) {
				return nil, fmt.Errorf("自定义卡密需为 1-%d 个字且不能包含控制字符", lotteryCustomCodeMaxRunes)
			}
			if _, exists := seenCustomCodes[secret]; exists {
				return nil, fmt.Errorf("存在重复自定义卡密：%s", security.MaskSecret(secret))
			}
			seenCustomCodes[secret] = struct{}{}
			specs = append(specs, lotteryPrizeSpec{
				PrizeType:     lotteryPrizeCustomCode,
				Amount:        1,
				Quantity:      1,
				DisplayName:   name,
				SecretValue:   secret,
				SecretPreview: security.MaskSecret(secret),
			})
			totalQuantity++

		default:
			return nil, fmt.Errorf("未知奖品类型：%s", fields[0])
		}
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("至少配置一个奖品")
	}
	if totalQuantity > 100 {
		return nil, fmt.Errorf("单个活动总奖品份数最多 100")
	}
	return specs, nil
}

func validLotteryPrizeName(name string) bool {
	name = strings.TrimSpace(name)
	length := len([]rune(name))
	return length >= 1 && length <= lotteryPrizeNameMaxRunes && !textfmt.ContainsDisallowedControl(name, false)
}

func validLotteryCustomCode(code string) bool {
	code = strings.TrimSpace(code)
	length := len([]rune(code))
	return length >= 1 && length <= lotteryCustomCodeMaxRunes && !textfmt.ContainsDisallowedControl(code, false)
}

func isLotteryPillPrizeName(itemName string) bool {
	switch strings.TrimSpace(itemName) {
	case "回气丹", "青灵养气丹", "万载灵髓",
		"聚气化旋", "凝元丹", "三纹清灵丹", "斗灵丹", "皇极丹":
		return true
	default:
		return false
	}
}

func parseLotterySessionInt(session *telegram.SessionState, key string, minValue, maxValue int) (int, error) {
	raw := strings.TrimSpace(session.GetTemp(key))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid lottery session integer: key=%s value=%s err=%w", textfmt.FormatPlainValue(key), textfmt.FormatPlainValue(raw), err)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("lottery session integer out of range: key=%s value=%d range=%d-%d", textfmt.FormatPlainValue(key), value, minValue, maxValue)
	}
	return value, nil
}

func parseLotterySessionUnixTime(session *telegram.SessionState, key string, now time.Time) (time.Time, error) {
	raw := strings.TrimSpace(session.GetTemp(key))
	unixValue, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid lottery session time: key=%s value=%s err=%w", textfmt.FormatPlainValue(key), textfmt.FormatPlainValue(raw), err)
	}
	drawAt := time.Unix(unixValue, 0)
	if !drawAt.After(now) {
		return time.Time{}, fmt.Errorf("lottery session time is not in future: key=%s value=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainValue(raw))
	}
	return drawAt, nil
}

func formatLotteryCreateConfirm(session *telegram.SessionState, specs []lotteryPrizeSpec) string {
	mode := session.GetTemp("lottery_mode")
	modeText := "满人开奖"
	ruleText := fmt.Sprintf("满 %s 人开奖", session.GetTemp("lottery_limit"))
	if mode == lotteryModeTime {
		modeText = "定时开奖"
		ruleText = session.GetTemp("lottery_draw_desc")
	}

	var b strings.Builder
	b.WriteString("请核对抽奖活动：\n\n")
	b.WriteString("名称：" + session.GetTemp("lottery_title") + "\n")
	b.WriteString("模式：" + modeText + "\n")
	b.WriteString("规则：" + ruleText + "\n")
	entryCost, entryCostErr := parseLotterySessionInt(session, "lottery_entry_cost", 0, 10000)
	if entryCostErr != nil {
		b.WriteString("参与消耗：读取失败\n")
	} else {
		b.WriteString(fmt.Sprintf("参与消耗：%d 积分\n", entryCost))
	}
	if entryCostErr == nil && entryCost > 0 {
		b.WriteString("天道奖池：按总参与积分的 10% 注入\n")
	}
	b.WriteString("领奖期限：开奖后 24 小时\n")
	b.WriteString("私聊领奖：点击「领取奖励」按钮\n逾期未领取自动失效。\n\n")
	b.WriteString("奖品：\n")
	for _, spec := range specs {
		b.WriteString(fmt.Sprintf("- %s x %d\n", spec.DisplayName, spec.Quantity))
	}
	b.WriteString("\n确认创建请回复：" + textfmt.Copyable("确认创建抽奖") + "\n取消请回复：" + textfmt.Copyable("取消"))
	return b.String()
}

func createLotteryActivityFromSession(msg *tgbotapi.Message, session *telegram.SessionState) (uint, error) {
	specs, err := parseLotteryPrizeSpecs(session.GetTemp("lottery_prizes"))
	if err != nil {
		return 0, err
	}

	for i := range specs {
		if specs[i].PrizeType != lotteryPrizeCustomCode {
			continue
		}
		specs[i].SecretEncrypted, err = encryptLotteryCustomCode(specs[i].SecretValue)
		if err != nil {
			return 0, fmt.Errorf("自定义卡密加密失败")
		}
		specs[i].SecretValue = ""
	}

	mode := session.GetTemp("lottery_mode")
	entryCost, err := parseLotterySessionInt(session, "lottery_entry_cost", 0, 10000)
	if err != nil {
		return 0, err
	}

	activity := model.LotteryActivity{
		Title:       session.GetTemp("lottery_title"),
		Mode:        mode,
		Status:      lotteryStatusActive,
		CreatedByID: msg.From.ID,
		CreatedBy:   textfmt.GetTelegramDisplayName(msg.From),
		EntryCost:   entryCost,
	}

	if mode == lotteryModeCount {
		limit, err := parseLotterySessionInt(session, "lottery_limit", 2, 100000)
		if err != nil {
			return 0, err
		}
		activity.ParticipantLimit = limit
	} else if mode == lotteryModeTime {
		drawAt, err := parseLotterySessionUnixTime(session, "lottery_draw_at", time.Now())
		if err != nil {
			return 0, err
		}
		activity.DrawAt = &drawAt
	} else {
		return 0, fmt.Errorf("开奖模式异常")
	}

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := createLotteryActivityInTx(tx, &activity); err != nil {
			return err
		}
		for _, spec := range specs {
			if err := createLotteryPrizeInTx(tx, &model.LotteryPrize{
				ActivityID:      activity.ID,
				PrizeType:       spec.PrizeType,
				Amount:          spec.Amount,
				Quantity:        spec.Quantity,
				DisplayName:     spec.DisplayName,
				SecretEncrypted: spec.SecretEncrypted,
			}); err != nil {
				return err
			}
		}
		if err := storage.WriteAuditLogInTx(tx, msg.From.ID, "CREATE_LOTTERY", fmt.Sprintf("%d", activity.ID), 0, fmt.Sprintf("超级管理员创建积分抽奖活动，标题：%s", textfmt.FormatPlainValue(activity.Title))); err != nil {
			return err
		}
		return queueLotteryIntro(tx, activity)
	})
	if err != nil {
		return 0, err
	}
	return activity.ID, nil
}

func showActiveLotteries(bot *tgbotapi.BotAPI, chatID int64) {
	showLotteryList(bot, chatID, false)
}

func showLotteryList(bot *tgbotapi.BotAPI, chatID int64, showIDs bool) {
	var activities []model.LotteryActivity
	if err := storage.DB.Where("status IN ?", []string{lotteryStatusActive, lotteryStatusDrawn}).
		Order("id desc").
		Limit(20).
		Find(&activities).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 查询抽奖活动失败，请稍后再试。")
		return
	}
	if len(activities) == 0 {
		telegram.SendPlainText(bot, chatID, "当前没有可参与或可领奖的积分抽奖。")
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	var b strings.Builder
	b.WriteString("🎲 积分抽奖活动\n领奖规则：开奖后 24 小时内点击「领取奖励」按钮私聊领取；逾期未领取自动失效。\n\n")
	for _, activity := range activities {
		participantCount, participantErr := countLotteryParticipants(activity.ID)
		statusText := lotteryStatusText(activity.Status)
		if showIDs {
			b.WriteString(fmt.Sprintf("#%d ", activity.ID))
		}
		b.WriteString(textfmt.LotteryDisplayText(activity.Title, 80, "-") + "\n")
		b.WriteString(fmt.Sprintf("状态：%s\n", statusText))
		if activity.Status == lotteryStatusActive {
			if activity.Mode == lotteryModeCount {
				if participantErr != nil {
					b.WriteString("进度：读取失败\n")
				} else {
					b.WriteString(fmt.Sprintf("进度：%d/%d 人\n", participantCount, activity.ParticipantLimit))
				}
			} else if activity.DrawAt != nil {
				b.WriteString(fmt.Sprintf("开奖时间：%s\n", activity.DrawAt.Format("2006-01-02 15:04")))
			}
			b.WriteString("参与：点击下方对应活动按钮\n\n")
			rows = append(rows, lotteryLinkRow(bot, "参与抽奖："+textfmt.LotteryDisplayText(activity.Title, 40, "抽奖"), fmt.Sprintf("lottery_%d", activity.ID)))
		} else {
			b.WriteString("已开奖：中奖者请点击「领取奖励」按钮私聊领取。\n领奖截止：" + formatLotteryClaimDeadline(activity) + "\n逾期未领取自动失效。\n\n")
		}
	}
	rows = append(rows, lotteryLinkRow(bot, "领取奖励", "lottery_claim"))
	m := tgbotapi.NewMessage(chatID, b.String())
	m.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	_, _ = telegram.SendAutoDelete(bot, m)
}

func announceLotteryCreated(bot *tgbotapi.BotAPI, adminChatID int64, activityID uint) {
	var snapshot model.LotteryActivity
	if err := storage.DB.First(&snapshot, activityID).Error; err != nil {
		telegram.SendPlainText(bot, adminChatID, "抽奖已创建，公告读取失败。")
		return
	}
	if snapshot.AnnounceIsChannel {
		return
	}

	if snapshot.AnnounceChatID == 0 {
		telegram.SendPlainText(bot, adminChatID, "⚠️ 抽奖已创建，但未配置 NOTICE_GROUP_ID，无法自动发布到群内。")
		return
	}

	var activity model.LotteryActivity
	if err := storage.DB.First(&activity, activityID).Error; err != nil {
		telegram.SendPlainText(bot, adminChatID, "⚠️ 抽奖已创建，但读取活动信息失败，无法自动发布到群内。")
		return
	}

	var prizes []model.LotteryPrize
	if err := storage.DB.Where("activity_id = ? AND deleted_at IS NULL", activityID).Order("id asc").Find(&prizes).Error; err != nil {
		telegram.SendPlainText(bot, adminChatID, "⚠️ 抽奖已创建，但读取奖品信息失败，无法自动发布到群内。")
		return
	}

	ruleText := ""
	if activity.Mode == lotteryModeCount {
		ruleText = fmt.Sprintf("满 %d 人自动开奖", activity.ParticipantLimit)
	} else if activity.DrawAt != nil {
		ruleText = "开奖时间：" + activity.DrawAt.Format("2006-01-02 15:04:05")
	} else {
		ruleText = "定时开奖"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("🎲 积分抽奖开启：%s\n\n", textfmt.LotteryDisplayText(activity.Title, 80, "-")))
	b.WriteString("规则：" + ruleText + "\n")
	b.WriteString(fmt.Sprintf("参与消耗：%d 积分\n", activity.EntryCost))
	if activity.EntryCost > 0 {
		b.WriteString("天道奖池：本期总参与积分的 10%\n")
	}
	b.WriteString("领奖期限：开奖后 24 小时\n\n")
	b.WriteString("奖品：\n")
	for _, prize := range prizes {
		b.WriteString(fmt.Sprintf("- %s x %d\n", textfmt.LotteryDisplayText(prize.DisplayName, 80, "-"), prize.Quantity))
	}
	b.WriteString("\n点击「参与抽奖」进入 Bot，确认后才扣分报名。")

	text := b.String()
	if telegram.EnqueueTelegramAsync(telegram.TelegramAsyncJob{
		Kind:        "lottery_intro_announce",
		DedupeKey:   fmt.Sprintf("lottery_intro:%d", activityID),
		Priority:    telegram.TelegramAsyncPriorityNormal,
		MaxAttempts: 1,
		Send: func() error {
			return sendLotteryIntroAnnouncementSync(bot, adminChatID, activityID, snapshot.AnnounceChatID, text)
		},
	}) {
		return
	}

	log.Printf("⚠️ 抽奖创建群内公告异步入队失败，改为同步发送: activity=%d chat=%d", activityID, snapshot.AnnounceChatID)
	if err := sendLotteryIntroAnnouncementSync(bot, adminChatID, activityID, snapshot.AnnounceChatID, text); err != nil {
		log.Printf("⚠️ 抽奖创建群内公告同步发送失败: activity=%d chat=%d err=%s", activityID, snapshot.AnnounceChatID, textfmt.FormatTelegramSendError(err))
	}
}

func sendLotteryIntroAnnouncementSync(bot *tgbotapi.BotAPI, adminChatID int64, activityID uint, targetChatID int64, text string) error {
	sentMsg, err := sendLotteryButtonText(bot, targetChatID, text, "参与抽奖", fmt.Sprintf("lottery_%d", activityID))
	if err != nil {
		log.Printf("⚠️ 抽奖创建群内公告发送失败: activity=%d chat=%d err=%s", activityID, targetChatID, textfmt.FormatTelegramSendError(err))
		if adminChatID != 0 {
			telegram.SendPlainText(bot, adminChatID, "⚠️ 抽奖已创建，但群内公告发送失败，请检查 Bot 群权限。")
		}
		return err
	}

	introPinned := pinLotteryMessage(bot, targetChatID, sentMsg.MessageID, activityID, "intro")
	res := storage.DB.Model(&model.LotteryActivity{}).
		Where("id = ?", activityID).
		Updates(map[string]interface{}{
			"announce_chat_id": targetChatID,
			"intro_message_id": sentMsg.MessageID,
			"intro_pinned":     introPinned,
		})
	if res.Error != nil {
		err := res.Error
		log.Printf("⚠️ 抽奖公告群记录失败: activity=%d chat=%d err=%s", activityID, targetChatID, textfmt.FormatPlainError(err))
	}
	if res.Error == nil && res.RowsAffected == 0 {
		log.Printf("lottery intro announcement record update missed: activity=%d chat=%d message=%d", activityID, targetChatID, sentMsg.MessageID)
	}
	return nil
}

func joinLotteryByID(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, id uint) {
	sourceChatID := int64(0)
	if !msg.Chat.IsPrivate() {
		sourceChatID = msg.Chat.ID
	}
	joinedCount, shouldDraw, err := joinLotteryActivity(id, msg.From, sourceChatID)
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, lotteryJoinErrorText(err))
		return
	}

	successText := fmt.Sprintf("✅ 已成功参与%s，当前已成功参与 %d 人。", lotteryUserActivityLabel(id), joinedCount)
	if !msg.Chat.IsPrivate() {
		sendLotteryReplyPlainText(bot, msg, successText)
	} else {
		telegram.SendPlainText(bot, msg.Chat.ID, successText)
	}
	if shouldDraw {
		go func() {
			if err := drawLotteryActivity(bot, uint(id), "participant_limit", 0); err != nil {
				log.Printf("⚠️ 满人抽奖自动开奖失败: activity=%d err=%s", id, textfmt.FormatPlainError(err))
				telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 满人抽奖 #%d 自动开奖失败：%s", id, textfmt.FormatPlainError(err)))
			}
		}()
	}
}

func joinLotteryActivity(activityID uint, tgUser *tgbotapi.User, sourceChatID int64) (int64, bool, error) {
	if tgUser == nil {
		return 0, false, storage.ErrUserNotFound
	}

	now := time.Now()
	userID := tgUser.ID
	userName := textfmt.GetTelegramDisplayName(tgUser)
	var joinedCount int64
	shouldDraw := false

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var txJoinedCount int64
		txShouldDraw := false
		var activity model.LotteryActivity
		if err := tx.Where("id = ? AND status = ?", activityID, lotteryStatusActive).First(&activity).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrLotteryNotActive
			}
			return err
		}

		if activity.Mode == lotteryModeTime && activity.DrawAt != nil && !now.Before(*activity.DrawAt) {
			return storage.ErrLotteryWaitingDraw
		}

		if sourceChatID != 0 && activity.AnnounceChatID == 0 {
			res := tx.Model(&model.LotteryActivity{}).
				Where("id = ? AND status = ? AND announce_chat_id = 0", activityID, lotteryStatusActive).
				Update("announce_chat_id", sourceChatID)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var current model.LotteryActivity
				if err := tx.Select("id", "status", "announce_chat_id").First(&current, activityID).Error; err != nil {
					return err
				}
				if current.Status != lotteryStatusActive {
					return storage.ErrLotteryNotActive
				}
				if current.AnnounceChatID == 0 {
					return fmt.Errorf("LOTTERY_JOIN_ANNOUNCE_CHAT_UPDATE_MISSED")
				}
			}
		}

		var existingUser model.User
		if err := tx.Where("telegram_id = ?", userID).First(&existingUser).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			existingUser = model.User{
				TelegramID: userID,
				Username:   lotteryUniqueUsername(tgUser),
				AbsUserID:  "",
				Points:     0,
			}
			if err := createLotteryLocalUserIfMissingInTx(tx, &existingUser); err != nil {
				return err
			}
		}

		if activity.Mode == lotteryModeCount {
			var currentCount int64
			if err := tx.Model(&model.LotteryParticipant{}).
				Where("activity_id = ? AND deleted_at IS NULL", activityID).
				Count(&currentCount).Error; err != nil {
				return err
			}
			if currentCount >= int64(activity.ParticipantLimit) {
				return storage.ErrLotteryFull
			}
		}

		participant := model.LotteryParticipant{
			ActivityID: activityID,
			UserID:     userID,
			UserName:   userName,
			JoinedAt:   now,
			EntryCost:  activity.EntryCost,
		}
		if err := createLotteryParticipantInTx(tx, &participant); err != nil {
			return err
		}

		if activity.EntryCost > 0 {
			if err := storage.ApplyPointDeltaInTx(
				tx,
				userID,
				-activity.EntryCost,
				"lottery_entry_cost",
				fmt.Sprintf("参与积分抽奖《%s》，消耗 %d 积分", lotteryPointDescriptionTitle(activity.Title), activity.EntryCost),
				"lottery",
				fmt.Sprintf("%d", activity.ID),
			); err != nil {
				return err
			}
		}

		if err := tx.Model(&model.LotteryParticipant{}).
			Where("activity_id = ? AND deleted_at IS NULL", activityID).
			Count(&txJoinedCount).Error; err != nil {
			return err
		}
		if activity.Mode == lotteryModeCount && txJoinedCount > int64(activity.ParticipantLimit) {
			return storage.ErrLotteryFull
		}

		txShouldDraw = activity.Mode == lotteryModeCount && txJoinedCount >= int64(activity.ParticipantLimit)
		joinedCount = txJoinedCount
		shouldDraw = txShouldDraw
		return nil
	})

	if err != nil {
		return 0, false, err
	}
	return joinedCount, shouldDraw, nil
}

func lotteryUniqueUsername(tgUser *tgbotapi.User) string {
	if tgUser == nil {
		return "tg_unknown"
	}
	if strings.TrimSpace(tgUser.UserName) != "" {
		return strings.TrimSpace(tgUser.UserName)
	}
	return fmt.Sprintf("tg_%d", tgUser.ID)
}

func lotteryJoinErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrLotteryNotActive):
		return "LOTTERY_NOT_ACTIVE"
	case errors.Is(err, storage.ErrLotteryWaitingDraw):
		return "LOTTERY_WAITING_DRAW"
	case errors.Is(err, storage.ErrLotteryFull):
		return "LOTTERY_FULL"
	case errors.Is(err, storage.ErrLotteryAlreadyJoined):
		return "ALREADY_JOINED"
	case errors.Is(err, storage.ErrPointsNotEnough):
		return "POINTS_NOT_ENOUGH"
	case errors.Is(err, storage.ErrUserNotFound):
		return "USER_NOT_FOUND"
	case err != nil:
		return storage.FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func lotteryJoinErrorText(err error) string {
	if err == nil {
		return ""
	}
	switch lotteryJoinErrorCode(err) {
	case "LOTTERY_NOT_ACTIVE":
		return "❌ 该抽奖不存在或已不可参与。"
	case "LOTTERY_WAITING_DRAW":
		return "⏳ 该抽奖已到开奖时间，正在等待系统开奖。"
	case "LOTTERY_FULL":
		return "❌ 该抽奖人数已满，正在等待开奖。"
	case "ALREADY_JOINED":
		return "你已经参与过这个抽奖了。"
	case "POINTS_NOT_ENOUGH":
		return "❌ 你的积分不足，无法参与本次抽奖。"
	default:
		log.Printf("⚠️ 参与抽奖失败: err=%s", textfmt.FormatPlainError(err))
		return "❌ 参与抽奖失败，请稍后再试。"
	}
}

func drawLotteryActivity(bot *tgbotapi.BotAPI, activityID uint, reason string, actorID int64) error {
	var activity model.LotteryActivity
	var participantCount int
	var winnerCount int
	var drawWinners []lotteryDrawWinnerInfo
	totalEntryCost := 0
	fusionPoolPoints := 0
	fusionBurst := false

	err := storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.LotteryActivity{}).
			Where("id = ? AND status = ?", activityID, lotteryStatusActive).
			Update("status", lotteryStatusDrawing)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrLotteryNotActive
		}

		if err := tx.First(&activity, activityID).Error; err != nil {
			return err
		}
		// Never reroll an activity that already has settlement evidence.
		if activity.DrawnAt != nil || activity.ClaimDeadlineAt != nil || activity.WinnersCount != 0 || activity.TotalEntryCost != 0 || activity.TotalRefundPoints != 0 || activity.FusionPoolPoints != 0 {
			return fmt.Errorf("抽奖已有结算记录，请人工核查状态，不能重复开奖")
		}
		var existingWinners int64
		if err := tx.Unscoped().Model(&model.LotteryWinner{}).Where("activity_id = ?", activityID).Count(&existingWinners).Error; err != nil {
			return err
		}
		if existingWinners > 0 {
			return fmt.Errorf("抽奖已有中奖记录，请人工核查状态，不能重复开奖")
		}

		var participants []model.LotteryParticipant
		if err := tx.Where("activity_id = ? AND deleted_at IS NULL", activityID).
			Order("id asc").
			Find(&participants).Error; err != nil {
			return err
		}
		participantCount = len(participants)
		for _, participant := range participants {
			if participant.EntryCost > 0 {
				totalEntryCost += participant.EntryCost
			}
		}

		now := time.Now()
		claimDeadline := lotteryClaimDeadline(activity, now)
		activity.DrawnAt = &now
		activity.ClaimDeadlineAt = &claimDeadline
		activity.Status = lotteryStatusDrawn

		if len(participants) == 0 {
			activity.Status = lotteryStatusClosed
			closeRes := tx.Model(&model.LotteryActivity{}).
				Where("id = ?", activityID).
				Updates(map[string]interface{}{
					"status":            lotteryStatusClosed,
					"drawn_at":          &now,
					"claim_deadline_at": &claimDeadline,
					"result_note":       "无人参与，活动自动关闭",
				})
			if closeRes.Error != nil {
				return closeRes.Error
			}
			if closeRes.RowsAffected == 0 {
				return fmt.Errorf("LOTTERY_DRAW_CLOSE_UPDATE_MISSED")
			}
			if reason == "manual" && actorID != 0 {
				if err := storage.WriteAuditLogInTx(tx, actorID, "FORCE_DRAW_LOTTERY", fmt.Sprintf("%d", activityID), 0, "超级管理员手动触发抽奖开奖，活动无人参与并关闭"); err != nil {
					return err
				}
			}
			return queueLotteryResult(tx, activity, participantCount, winnerCount, drawWinners)
		}

		var prizes []model.LotteryPrize
		if err := tx.Where("activity_id = ? AND deleted_at IS NULL", activityID).
			Order("id asc").
			Find(&prizes).Error; err != nil {
			return err
		}

		prizeSlots := make([]model.LotteryPrize, 0)
		for _, prize := range prizes {
			for i := 0; i < prize.Quantity; i++ {
				prizeSlots = append(prizeSlots, prize)
			}
		}
		if len(prizeSlots) == 0 {
			return fmt.Errorf("NO_PRIZES")
		}

		shuffleLotteryParticipants(participants)
		shuffleLotteryPrizes(prizeSlots)

		winnerCount = len(prizeSlots)
		if len(participants) < winnerCount {
			winnerCount = len(participants)
		}

		for i := 0; i < winnerCount; i++ {
			p := participants[i]
			prize := prizeSlots[i]
			winner := model.LotteryWinner{
				ActivityID: activityID,
				UserID:     p.UserID,
				UserName:   p.UserName,
				PrizeID:    prize.ID,
				PrizeType:  prize.PrizeType,
				Amount:     prize.Amount,
				Status:     lotteryWinnerPending,
			}
			created, err := createLotteryWinnerInTx(tx, &winner)
			if err != nil {
				return err
			}
			if !created {
				continue
			}
			drawWinners = append(drawWinners, lotteryDrawWinnerInfo{
				UserID:      p.UserID,
				UserName:    p.UserName,
				PrizeType:   prize.PrizeType,
				Amount:      prize.Amount,
				DisplayName: prize.DisplayName,
			})
		}

		fusionPoolPoints = totalEntryCost / 10
		if fusionPoolPoints > 0 {
			var err error
			_, fusionBurst, err = storage.AddPointsToFusionPoolInTx(tx, fusionPoolPoints)
			if err != nil {
				return err
			}
		}
		activity.TotalEntryCost = totalEntryCost
		activity.TotalRefundPoints = 0
		activity.FusionPoolPoints = fusionPoolPoints

		drawRes := tx.Model(&model.LotteryActivity{}).
			Where("id = ?", activityID).
			Updates(map[string]interface{}{
				"status":              lotteryStatusDrawn,
				"drawn_at":            &now,
				"claim_deadline_at":   &claimDeadline,
				"winners_count":       winnerCount,
				"total_entry_cost":    totalEntryCost,
				"total_refund_points": 0,
				"fusion_pool_points":  fusionPoolPoints,
				"result_note":         fmt.Sprintf("reason=%s participants=%d winners=%d", textfmt.FormatPlainValue(reason), participantCount, winnerCount),
			})
		if drawRes.Error != nil {
			return drawRes.Error
		}
		if drawRes.RowsAffected == 0 {
			return fmt.Errorf("LOTTERY_DRAW_RESULT_UPDATE_MISSED")
		}
		if reason == "manual" && actorID != 0 {
			if err := storage.WriteAuditLogInTx(tx, actorID, "FORCE_DRAW_LOTTERY", fmt.Sprintf("%d", activityID), 0, fmt.Sprintf("超级管理员手动触发抽奖开奖，参与=%d 中奖=%d", participantCount, winnerCount)); err != nil {
				return err
			}
		}
		return queueLotteryResult(tx, activity, participantCount, winnerCount, drawWinners)
	})

	if err != nil {
		return err
	}

	notifyLotteryDrawn(bot, activity, participantCount, winnerCount, drawWinners)
	if fusionBurst {
		telegram.NotifyFusionPoolBurst(bot, config.AppConfig.NoticeGroupID, "积分抽奖福泽回流")
	}
	return nil
}

func lotteryClaimDuration(_ model.LotteryActivity) time.Duration {
	return lotteryClaimValidDuration
}

func lotteryClaimDeadline(activity model.LotteryActivity, now time.Time) time.Time {
	return now.Add(lotteryClaimDuration(activity))
}

func lotteryResultText(activity model.LotteryActivity, participantCount int, winnerCount int, winners []lotteryDrawWinnerInfo) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🎲 积分抽奖《%s》已开奖\n\n", textfmt.LotteryDisplayText(activity.Title, 80, "-")))
	b.WriteString(fmt.Sprintf("参与人数：%d\n", participantCount))
	b.WriteString(fmt.Sprintf("中奖名额：%d\n", winnerCount))
	if activity.TotalEntryCost > 0 {
		b.WriteString(fmt.Sprintf("总参与积分：%d\n", activity.TotalEntryCost))
		b.WriteString(fmt.Sprintf("注入天道奖池：%d\n", activity.FusionPoolPoints))
	}
	if activity.ClaimDeadlineAt != nil {
		b.WriteString("领奖截止：" + formatLotteryClaimDeadline(activity) + "\n")
	}
	if len(winners) > 0 {
		b.WriteString("\n中奖者：\n")
		for i, winner := range winners {
			b.WriteString(fmt.Sprintf("%d. %s - %s\n", i+1, textfmt.LotteryDisplayText(winner.UserName, 80, "-"), lotteryPrizeDisplayText(winner.PrizeType, winner.Amount, winner.DisplayName)))
		}
		b.WriteString("\n领奖方式：点击「领取奖励」按钮进入 Bot，点击 Start / 开始后领取。\n开奖后 24 小时内领取，逾期未领取自动失效。")
	} else {
		b.WriteString("\n本期无人中奖。")
	}

	return b.String()
}
func notifyLotteryDrawn(bot *tgbotapi.BotAPI, activity model.LotteryActivity, participantCount int, winnerCount int, winners []lotteryDrawWinnerInfo) {
	text := lotteryResultText(activity, participantCount, winnerCount, winners)
	targetChatID := activity.AnnounceChatID
	if targetChatID == 0 {
		targetChatID = config.AppConfig.NoticeGroupID
	}
	if targetChatID != 0 && !activity.AnnounceIsChannel {
		activityID := activity.ID
		if !telegram.EnqueueTelegramAsync(telegram.TelegramAsyncJob{
			Kind:        "lottery_result_announce",
			DedupeKey:   fmt.Sprintf("lottery_result:%d", activityID),
			Priority:    telegram.TelegramAsyncPriorityNormal,
			MaxAttempts: 1,
			Send: func() error {
				return sendLotteryResultAnnouncementSync(bot, activityID, targetChatID, text)
			},
		}) {
			log.Printf("⚠️ 抽奖开奖结果公告异步入队失败，改为同步发送: activity=%d chat=%d", activityID, targetChatID)
			if err := sendLotteryResultAnnouncementSync(bot, activityID, targetChatID, text); err != nil {
				log.Printf("⚠️ 抽奖开奖结果公告同步发送失败: activity=%d chat=%d err=%s", activityID, targetChatID, textfmt.FormatTelegramSendError(err))
			}
		}
	}
	telegram.NotifySuperAdminsPlain(bot, text)
	notifyLotteryWinnersPrivately(bot, activity, winners)
}

func sendLotteryResultAnnouncementSync(bot *tgbotapi.BotAPI, activityID uint, targetChatID int64, text string) error {
	sentMsg, err := sendLotteryButtonText(bot, targetChatID, text, "领取奖励", "lottery_claim")
	if err != nil {
		log.Printf("⚠️ 抽奖开奖群内公告发送失败: activity=%d chat=%d err=%s", activityID, targetChatID, textfmt.FormatTelegramSendError(err))
		return err
	}

	resultPinned := pinLotteryMessage(bot, targetChatID, sentMsg.MessageID, activityID, "result")
	res := storage.DB.Model(&model.LotteryActivity{}).
		Where("id = ?", activityID).
		Updates(map[string]interface{}{
			"announce_chat_id":  targetChatID,
			"result_message_id": sentMsg.MessageID,
			"result_pinned":     resultPinned,
		})
	if res.Error != nil {
		err := res.Error
		log.Printf("⚠️ 抽奖结果公告记录失败: activity=%d chat=%d message=%d err=%s", activityID, targetChatID, sentMsg.MessageID, textfmt.FormatPlainError(err))
	}
	if res.Error == nil && res.RowsAffected == 0 {
		log.Printf("lottery result announcement record update missed: activity=%d chat=%d message=%d", activityID, targetChatID, sentMsg.MessageID)
	}
	return nil
}

func notifyLotteryWinnersPrivately(bot *tgbotapi.BotAPI, activity model.LotteryActivity, winners []lotteryDrawWinnerInfo) {
	if len(winners) == 0 {
		return
	}

	for _, winner := range winners {
		text := fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：%s\n领奖截止：%s\n\n请点击「领取奖励」按钮私聊领取。\n开奖后 24 小时内领取，逾期未领取自动失效。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), lotteryPrizeDisplayText(winner.PrizeType, winner.Amount, winner.DisplayName), formatLotteryClaimDeadline(activity))
		if _, err := sendLotteryButtonText(bot, winner.UserID, text, "领取奖励", "lottery_claim"); err != nil {
			log.Printf("⚠️ 抽奖中奖私聊提醒失败: activity=%d user=%d err=%s", activity.ID, winner.UserID, textfmt.FormatTelegramSendError(err))
			telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 抽奖 #%d 中奖用户 %d 私聊提醒失败，请人工通知。", activity.ID, winner.UserID))
		}
	}
}

func lotteryPrizeText(prizeType string, amount int) string {
	switch prizeType {
	case lotteryPrizePoints:
		return fmt.Sprintf("%d 积分", amount)
	case lotteryPrizeInvite:
		return "邀请码"
	case lotteryPrizeWhitelist:
		return "永久白名单卡"
	case lotteryPrizeRenew:
		return fmt.Sprintf("%d 天续期卡", amount)
	case lotteryPrizePill:
		return "丹药"
	case lotteryPrizeManual:
		return "联系管理员领奖"
	case lotteryPrizeCustomCode:
		return "自定义卡密"
	default:
		return prizeType
	}
}

func lotteryPrizeDisplayText(prizeType string, amount int, displayName string) string {
	name := strings.TrimSpace(displayName)
	switch prizeType {
	case lotteryPrizePill:
		return fmt.Sprintf("丹药【%s】", textfmt.LotteryDisplayText(name, 80, "丹药"))
	case lotteryPrizeManual:
		return fmt.Sprintf("人工奖品【%s】", textfmt.LotteryDisplayText(name, 80, "特殊奖品"))
	case lotteryPrizeCustomCode:
		return fmt.Sprintf("自定义卡密【%s】", textfmt.LotteryDisplayText(name, 80, "卡密奖品"))
	default:
		return lotteryPrizeText(prizeType, amount)
	}
}

func lotteryPointDescriptionTitle(title string) string {
	return textfmt.LotteryDisplayText(title, 80, "-")
}

func claimLotteryWinner(activity model.LotteryActivity, winner model.LotteryWinner) (string, error) {
	var reply string
	expiredInTx := false

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND status = ?", activity.ID, lotteryStatusDrawn).First(&activity).Error; err != nil {
			return err
		}
		if activity.ClaimDeadlineAt == nil {
			return fmt.Errorf("LOTTERY_CLAIM_DEADLINE_MISSING")
		}
		var current model.LotteryWinner
		if err := tx.Where("id = ? AND activity_id = ? AND user_id = ?", winner.ID, activity.ID, winner.UserID).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("WINNER_NOT_PENDING")
			}
			return err
		}

		if current.Status == lotteryWinnerClaimed {
			if current.RewardDeliveredAt != nil {
				return errLotteryAlreadyClaimed
			}
			var err error
			reply, err = decryptLotteryRewardResult(current)
			return err
		}
		if current.Status == lotteryWinnerExpired {
			return storage.ErrLotteryClaimExpired
		}
		if current.Status != lotteryWinnerPending {
			return fmt.Errorf("LOTTERY_WINNER_NOT_PENDING")
		}
		now := time.Now()
		if lotteryWinnerShouldExpire(activity, current, now) {
			expired, err := expireLotteryWinnerInTx(tx, activity.ID, current.ID, current.UserID, "claim deadline exceeded in claim tx")
			if err != nil {
				return err
			}
			expiredInTx = expired
			return nil
		}

		markRes := tx.Model(&model.LotteryWinner{}).
			Where("id = ? AND activity_id = ? AND user_id = ? AND status = ?", current.ID, activity.ID, current.UserID, lotteryWinnerPending).
			Updates(map[string]interface{}{
				"status":     lotteryWinnerClaimed,
				"claimed_at": &now,
			})
		if markRes.Error != nil {
			return markRes.Error
		}
		if markRes.RowsAffected == 0 {
			return fmt.Errorf("LOTTERY_WINNER_CLAIM_MARK_MISSED")
		}
		current.Status = lotteryWinnerClaimed
		current.ClaimedAt = &now

		switch current.PrizeType {
		case lotteryPrizePoints:
			if err := storage.ApplyPointDeltaInTx(
				tx,
				current.UserID,
				current.Amount,
				"lottery_reward",
				fmt.Sprintf("积分抽奖《%s》中奖奖励", lotteryPointDescriptionTitle(activity.Title)),
				"lottery",
				fmt.Sprintf("%d", activity.ID),
			); err != nil {
				return err
			}
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：%d 积分\n\n灵石已自动入账。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), current.Amount)

		case lotteryPrizeInvite:
			code, err := createLotteryInviteCodeInTx(tx)
			if err != nil {
				return err
			}
			current.PrizeCodePreview = security.MaskSecret(code)
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：邀请码\n\n你的专属邀请码：%s", textfmt.LotteryDisplayText(activity.Title, 80, "-"), textfmt.Copyable(code))

		case lotteryPrizeRenew:
			code, err := createLotteryRenewCodeInTx(tx, current.Amount)
			if err != nil {
				return err
			}
			current.PrizeCodePreview = security.MaskSecret(code)
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：%d 天续期卡\n\n你的专属续期卡：%s", textfmt.LotteryDisplayText(activity.Title, 80, "-"), current.Amount, textfmt.Copyable(code))

		case lotteryPrizeWhitelist:
			code, err := createLotteryWhitelistCodeInTx(tx, activity.CreatedByID)
			if err != nil {
				return err
			}
			current.PrizeCodePreview = security.MaskSecret(code)
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：永久白名单卡\n\n你的专属白名单卡：%s\n\n请私聊发送 %s，按提示兑换。兑换需已有正式听书账号；已是白名单或超级管理员不能兑换，卡密不会消耗。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), textfmt.Copyable(code), textfmt.Copyable("兑换白名单"))

		case lotteryPrizePill:
			itemName, err := getLotteryPrizeDisplayNameInTx(tx, activity.ID, current)
			if err != nil {
				return err
			}
			if !isLotteryPillPrizeName(itemName) {
				return fmt.Errorf("INVALID_PILL_PRIZE")
			}
			if err := grantLotteryInventoryItemInTx(tx, current.UserID, itemName, 1); err != nil {
				return err
			}
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：丹药【%s】 x1\n\n丹药已放入乾坤袋。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), textfmt.LotteryDisplayText(itemName, 80, "丹药"))

		case lotteryPrizeManual:
			prizeName, err := getLotteryPrizeDisplayNameInTx(tx, activity.ID, current)
			if err != nil {
				return err
			}
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：%s\n\n该奖品由管理员人工派发，请联系管理员领奖。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), textfmt.LotteryDisplayText(prizeName, 80, "特殊奖品"))

		case lotteryPrizeCustomCode:
			prize, err := getLotteryPrizeInTx(tx, activity.ID, current)
			if err != nil {
				return err
			}
			code, err := decryptLotteryCustomCode(prize.SecretEncrypted)
			if err != nil {
				return err
			}
			current.PrizeCodePreview = security.MaskSecret(code)
			reply = fmt.Sprintf("🎲 恭喜中奖！\n\n活动：%s\n奖品：%s\n\n你的专属卡密：%s\n\n请妥善保存，Bot 不会在群内公开卡密。", textfmt.LotteryDisplayText(activity.Title, 80, "-"), textfmt.LotteryDisplayText(prize.DisplayName, 80, "卡密奖品"), textfmt.Copyable(code))

		default:
			return fmt.Errorf("UNKNOWN_PRIZE_TYPE")
		}

		encrypted, err := encryptLotteryRewardResult(reply, current)
		if err != nil {
			return err
		}
		resultRes := tx.Model(&model.LotteryWinner{}).Where("id = ? AND activity_id = ? AND user_id = ? AND status = ?", current.ID, activity.ID, current.UserID, lotteryWinnerClaimed).Updates(map[string]interface{}{"reward_result_encrypted": encrypted, "prize_code_preview": current.PrizeCodePreview})
		if resultRes.Error != nil {
			return resultRes.Error
		}
		if resultRes.RowsAffected == 0 {
			return fmt.Errorf("LOTTERY_REWARD_RESULT_SAVE_MISSED")
		}

		return createLotteryClaimLogInTx(tx, &model.LotteryClaimLog{
			ActivityID: activity.ID,
			WinnerID:   current.ID,
			UserID:     current.UserID,
			Action:     "claimed",
			Detail:     fmt.Sprintf("prize_type=%s amount=%d", current.PrizeType, current.Amount),
		})
	})

	if err != nil {
		return "", err
	}
	if expiredInTx {
		return "", storage.ErrLotteryClaimExpired
	}
	return reply, nil
}

func getLotteryPrizeDisplayNameInTx(tx *gorm.DB, activityID uint, winner model.LotteryWinner) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("DB_TX_EMPTY")
	}
	if activityID == 0 || winner.PrizeID == 0 {
		return "", fmt.Errorf("PRIZE_ID_EMPTY")
	}

	var prize model.LotteryPrize
	if err := tx.Select("id", "activity_id", "prize_type", "display_name").
		Where("id = ? AND activity_id = ? AND deleted_at IS NULL", winner.PrizeID, activityID).
		First(&prize).Error; err != nil {
		return "", err
	}
	if !lotteryPrizeMatchesWinner(prize, activityID, winner) {
		return "", fmt.Errorf("LOTTERY_PRIZE_MISMATCH")
	}

	itemName := strings.TrimSpace(prize.DisplayName)
	if itemName == "" {
		return "", fmt.Errorf("PRIZE_DISPLAY_EMPTY")
	}
	return itemName, nil
}

func getLotteryPrizeInTx(tx *gorm.DB, activityID uint, winner model.LotteryWinner) (model.LotteryPrize, error) {
	if tx == nil {
		return model.LotteryPrize{}, fmt.Errorf("DB_TX_EMPTY")
	}
	if activityID == 0 || winner.PrizeID == 0 {
		return model.LotteryPrize{}, fmt.Errorf("PRIZE_ID_EMPTY")
	}

	var prize model.LotteryPrize
	if err := tx.Where("id = ? AND activity_id = ? AND deleted_at IS NULL", winner.PrizeID, activityID).
		First(&prize).Error; err != nil {
		return model.LotteryPrize{}, err
	}
	if !lotteryPrizeMatchesWinner(prize, activityID, winner) {
		return model.LotteryPrize{}, fmt.Errorf("LOTTERY_PRIZE_MISMATCH")
	}
	return prize, nil
}

func lotteryPrizeMatchesWinner(prize model.LotteryPrize, activityID uint, winner model.LotteryWinner) bool {
	return activityID != 0 &&
		prize.ID == winner.PrizeID &&
		prize.ActivityID == activityID &&
		strings.TrimSpace(prize.PrizeType) == strings.TrimSpace(winner.PrizeType)
}

func grantLotteryInventoryItemInTx(tx *gorm.DB, userID int64, itemName string, quantity int) error {
	if tx == nil {
		return fmt.Errorf("DB_TX_EMPTY")
	}
	itemName = strings.TrimSpace(itemName)
	if userID == 0 || itemName == "" || quantity <= 0 {
		return fmt.Errorf("INVALID_INVENTORY_GRANT")
	}

	res := tx.Clauses(game.InventoryQuantityUpsertClause(quantity)).Create(&model.Inventory{
		UserID:   userID,
		ItemName: itemName,
		Quantity: quantity,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("LOTTERY_INVENTORY_GRANT_MISSED")
	}
	return nil
}

func sendLotteryPlainText(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := telegram.SendAutoDelete(bot, msg)
	return err
}

func SendLotteryGroupPlainText(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := telegram.SendAutoDelete(bot, msg)
	return err
}

func sendLotteryReplyPlainText(bot *tgbotapi.BotAPI, sourceMsg *tgbotapi.Message, text string) {
	if bot == nil || sourceMsg == nil || sourceMsg.Chat == nil {
		return
	}

	msg := tgbotapi.NewMessage(sourceMsg.Chat.ID, text)
	msg.ReplyToMessageID = sourceMsg.MessageID
	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 抽奖回复消息发送失败: chat=%d message=%d err=%s", sourceMsg.Chat.ID, sourceMsg.MessageID, textfmt.FormatTelegramSendError(err))
		telegram.SendPlainText(bot, sourceMsg.Chat.ID, text)
	}
}

func pinLotteryMessage(bot *tgbotapi.BotAPI, chatID int64, messageID int, activityID uint, label string) bool {
	if bot == nil || chatID == 0 || messageID == 0 {
		return false
	}

	cfg := tgbotapi.PinChatMessageConfig{
		ChatID:              chatID,
		MessageID:           messageID,
		DisableNotification: true,
	}
	if _, err := bot.Request(cfg); err != nil {
		safeLabel := textfmt.FormatPlainValue(label)
		log.Printf("⚠️ 抽奖消息置顶失败: activity=%d label=%s chat=%d message=%d err=%s", activityID, safeLabel, chatID, messageID, textfmt.FormatTelegramSendError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 抽奖 #%d 的%s消息发送成功，但置顶失败。请检查 Bot 是否有群置顶权限。\n\nchat=%d message=%d err=%s", activityID, safeLabel, chatID, messageID, textfmt.FormatPlainError(err)))
		return false
	}
	return true
}

func unpinLotteryMessage(bot *tgbotapi.BotAPI, chatID int64, messageID int, activityID uint, label string) bool {
	if bot == nil || chatID == 0 || messageID == 0 {
		return true
	}

	cfg := tgbotapi.UnpinChatMessageConfig{
		ChatID:    chatID,
		MessageID: messageID,
	}
	if _, err := bot.Request(cfg); err != nil {
		safeLabel := textfmt.FormatPlainValue(label)
		log.Printf("⚠️ 抽奖消息取消置顶失败: activity=%d label=%s chat=%d message=%d err=%s", activityID, safeLabel, chatID, messageID, textfmt.FormatTelegramSendError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 抽奖 #%d 的%s消息取消置顶失败，请人工检查。\n\nchat=%d message=%d err=%s", activityID, safeLabel, chatID, messageID, textfmt.FormatPlainError(err)))
		return false
	}
	return true
}

func encryptLotteryCustomCode(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !validLotteryCustomCode(raw) {
		return "", fmt.Errorf("INVALID_LOTTERY_CUSTOM_CODE")
	}

	pepper := security.GetSensitivePepper()
	if pepper == "" {
		return "", storage.ErrSecurityPepperNotConfigured
	}

	key := sha256.Sum256([]byte("lottery-custom-code:" + pepper))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(raw), nil)
	return "gcm$" + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptLotteryCustomCode(encrypted string) (string, error) {
	encrypted = strings.TrimSpace(encrypted)
	if !strings.HasPrefix(encrypted, "gcm$") {
		return "", fmt.Errorf("INVALID_LOTTERY_CUSTOM_CODE_CIPHER")
	}

	pepper := security.GetSensitivePepper()
	if pepper == "" {
		return "", storage.ErrSecurityPepperNotConfigured
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encrypted, "gcm$"))
	if err != nil {
		return "", err
	}

	key := sha256.Sum256([]byte("lottery-custom-code:" + pepper))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(payload) <= gcm.NonceSize() {
		return "", fmt.Errorf("INVALID_LOTTERY_CUSTOM_CODE_PAYLOAD")
	}

	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	code := string(plain)
	if !validLotteryCustomCode(code) {
		return "", fmt.Errorf("INVALID_LOTTERY_CUSTOM_CODE_PLAIN")
	}
	return code, nil
}

func lotteryWinnerShouldExpire(activity model.LotteryActivity, winner model.LotteryWinner, now time.Time) bool {
	return winner.Status == lotteryWinnerPending &&
		activity.ClaimDeadlineAt != nil &&
		!now.Before(*activity.ClaimDeadlineAt)
}

func createLotteryInviteCodeInTx(tx *gorm.DB) (string, error) {
	for i := 0; i < 5; i++ {
		code := "YQM-" + security.GenerateRandomCode(12)
		if err := storage.CreateInviteCodeRecord(tx, code); err == nil {
			return code, nil
		} else if !storage.IsUniqueConstraintError(err) {
			return "", err
		}
	}
	return "", storage.ErrCreateInviteCodeFailed
}

func createLotteryWhitelistCodeInTx(tx *gorm.DB, createdByID int64) (string, error) {
	for i := 0; i < 5; i++ {
		code := "WL-" + security.GenerateRandomCode(32)
		hash := security.HashSensitiveToken(code)
		if hash == "" {
			return "", storage.ErrSecurityPepperNotConfigured
		}
		record := model.WhitelistCode{CodeHash: hash, CodePreview: security.MaskSecret(code), CreatedByID: createdByID}
		res := tx.Create(&record)
		if res.Error != nil {
			if storage.IsUniqueConstraintError(res.Error) {
				continue
			}
			return "", res.Error
		}
		if res.RowsAffected != 1 {
			return "", fmt.Errorf("LOTTERY_WHITELIST_CODE_CREATE_MISSED")
		}
		return code, nil
	}
	return "", fmt.Errorf("LOTTERY_WHITELIST_CODE_COLLISION_LIMIT")
}

func createLotteryRenewCodeInTx(tx *gorm.DB, days int) (string, error) {
	for i := 0; i < 5; i++ {
		code := fmt.Sprintf("R%d-%s", days, security.GenerateRandomCode(16))
		if err := storage.CreateRenewCodeRecord(tx, code, days); err == nil {
			return code, nil
		} else if !storage.IsUniqueConstraintError(err) {
			return "", err
		}
	}
	return "", storage.ErrCreateRenewCodeFailed
}

func expireLotteryWinnerInTx(tx *gorm.DB, activityID uint, winnerID uint, userID int64, detail string) (bool, error) {
	if tx == nil || activityID == 0 || winnerID == 0 || userID == 0 {
		return false, fmt.Errorf("LOTTERY_EXPIRE_INVALID")
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		detail = "claim deadline exceeded"
	}

	res := tx.Model(&model.LotteryWinner{}).
		Where("id = ? AND activity_id = ? AND user_id = ? AND status = ?", winnerID, activityID, userID, lotteryWinnerPending).
		Update("status", lotteryWinnerExpired)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	return true, createLotteryClaimLogInTx(tx, &model.LotteryClaimLog{
		ActivityID: activityID,
		WinnerID:   winnerID,
		UserID:     userID,
		Action:     "expired",
		Detail:     detail,
	})
}

func handleLotteryDetailCommand(bot *tgbotapi.BotAPI, chatID int64, text string) {
	id, ok := parseTrailingUint(text)
	if !ok {
		telegram.SendPlainText(bot, chatID, "请按格式发送："+textfmt.Copyable("抽奖详情 活动ID"))
		return
	}

	var activity model.LotteryActivity
	if err := storage.DB.First(&activity, uint(id)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			telegram.SendPlainText(bot, chatID, "抽奖活动不存在。")
		} else {
			log.Printf("lottery detail activity read failed: activity=%d err=%s", id, textfmt.FormatPlainError(err))
			telegram.SendPlainText(bot, chatID, "抽奖详情读取失败，请稍后再试。")
		}
		return
	}

	var prizes []model.LotteryPrize
	if err := storage.DB.Where("activity_id = ? AND deleted_at IS NULL", activity.ID).Find(&prizes).Error; err != nil {
		log.Printf("⚠️ 抽奖详情读取奖品失败: activity=%d err=%s", activity.ID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, chatID, "抽奖详情暂时读取失败：奖品记录不可用，请稍后重试。")
		return
	}

	var winners []model.LotteryWinner
	if err := storage.DB.Where("activity_id = ? AND deleted_at IS NULL", activity.ID).Order("id asc").Find(&winners).Error; err != nil {
		log.Printf("⚠️ 抽奖详情读取中奖记录失败: activity=%d err=%s", activity.ID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, chatID, "抽奖详情暂时读取失败：中奖记录不可用，请稍后重试。")
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("抽奖 #%d：%s\n", activity.ID, textfmt.LotteryDisplayText(activity.Title, 80, "-")))
	b.WriteString("状态：" + lotteryStatusText(activity.Status) + "\n")
	if participantCount, err := countLotteryParticipants(activity.ID); err != nil {
		b.WriteString("参与人数：读取失败\n")
	} else {
		b.WriteString(fmt.Sprintf("参与人数：%d\n", participantCount))
	}
	if activity.Mode == lotteryModeCount {
		b.WriteString(fmt.Sprintf("开奖规则：满 %d 人\n", activity.ParticipantLimit))
	} else if activity.DrawAt != nil {
		b.WriteString("开奖时间：" + activity.DrawAt.Format("2006-01-02 15:04") + "\n")
	}
	b.WriteString("领奖规则：开奖后 24 小时内点击「领取奖励」按钮私聊领取；逾期未领取自动失效。\n")
	if activity.ClaimDeadlineAt != nil {
		b.WriteString("领奖截止：" + formatLotteryClaimDeadline(activity) + "\n")
	}
	b.WriteString("\n奖品：\n")
	for _, p := range prizes {
		b.WriteString(fmt.Sprintf("- %s x %d\n", textfmt.LotteryDisplayText(p.DisplayName, 80, "-"), p.Quantity))
	}
	if len(winners) > 0 {
		b.WriteString("\n中奖记录：\n")
		for _, w := range winners {
			delivery := ""
			if w.Status == lotteryWinnerClaimed {
				if w.RewardDeliveredAt == nil {
					delivery = " 领取结果待重发"
				} else {
					delivery = " 领取结果已送达"
				}
			}
			b.WriteString(fmt.Sprintf("- %s(%d) %s %d 状态:%s%s\n", textfmt.LotteryDisplayText(w.UserName, 80, "-"), w.UserID, lotteryPrizeText(w.PrizeType, w.Amount), w.Amount, lotteryStatusText(w.Status), delivery))
		}
	}
	telegram.SendPlainText(bot, chatID, b.String())
}

func handleForceDrawLotteryCommand(bot *tgbotapi.BotAPI, chatID int64, actorID int64, text string) {
	id, ok := parseTrailingUint(text)
	if !ok {
		telegram.SendPlainText(bot, chatID, "请按格式发送："+textfmt.Copyable("强制开奖 活动ID"))
		return
	}
	if err := drawLotteryActivity(bot, uint(id), "manual", actorID); err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 强制开奖失败："+textfmt.FormatPlainError(err))
		return
	}
	telegram.SendPlainText(bot, chatID, fmt.Sprintf("✅ 抽奖 #%d 已强制开奖。", id))
}

func handleCancelLotteryCommand(bot *tgbotapi.BotAPI, chatID int64, actorID int64, text string) {
	id, ok := parseTrailingUint(text)
	if !ok {
		telegram.SendPlainText(bot, chatID, "请按格式发送："+textfmt.Copyable("取消抽奖 活动ID"))
		return
	}

	refundTotal, err := cancelLotteryActivityWithFullRefund(uint(id), actorID)
	if err != nil {
		if errors.Is(err, storage.ErrLotteryNotActive) {
			telegram.SendPlainText(bot, chatID, "该抽奖不存在或已不能取消。")
			return
		}
		log.Printf("⚠️ 取消抽奖失败: activity=%d actor=%d err=%s", id, actorID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, chatID, "❌ 取消抽奖失败，请稍后再试。")
		return
	}
	if refundTotal < 0 {
		telegram.SendPlainText(bot, chatID, "该抽奖不存在或已不能取消。")
		return
	}
	telegram.SendPlainText(bot, chatID, fmt.Sprintf("✅ 抽奖 #%d 已取消，已全额退还参与积分 %d。", id, refundTotal))
}

func cancelLotteryActivityWithFullRefund(activityID uint, actorID int64) (int, error) {
	txRefundTotal := 0
	now := time.Now()

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		txRefundTotal = 0
		res := tx.Model(&model.LotteryActivity{}).
			Where("id = ? AND status = ?", activityID, lotteryStatusActive).
			Updates(map[string]interface{}{
				"status":            lotteryStatusClosed,
				"claim_deadline_at": &now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrLotteryNotActive
		}

		var activity model.LotteryActivity
		if err := tx.First(&activity, activityID).Error; err != nil {
			return err
		}

		var participants []model.LotteryParticipant
		if err := tx.Where("activity_id = ? AND deleted_at IS NULL", activityID).Find(&participants).Error; err != nil {
			return err
		}

		for _, participant := range participants {
			if participant.EntryCost <= 0 || participant.IsRefunded {
				continue
			}

			refundRes := tx.Model(&model.LotteryParticipant{}).
				Where("id = ? AND activity_id = ? AND is_refunded = ?", participant.ID, activity.ID, false).
				Updates(map[string]interface{}{
					"refund_points": participant.EntryCost,
					"is_refunded":   true,
					"refunded_at":   &now,
				})
			if refundRes.Error != nil {
				return refundRes.Error
			}
			if refundRes.RowsAffected == 0 {
				return fmt.Errorf("LOTTERY_CANCEL_REFUND_MARK_MISSED")
			}

			if err := storage.ApplyPointDeltaInTx(
				tx,
				participant.UserID,
				participant.EntryCost,
				"lottery_cancel_refund",
				fmt.Sprintf("积分抽奖《%s》取消，全额退还 %d 积分", lotteryPointDescriptionTitle(activity.Title), participant.EntryCost),
				"lottery",
				fmt.Sprintf("%d", activity.ID),
			); err != nil {
				return err
			}

			txRefundTotal += participant.EntryCost
		}

		totalRes := tx.Model(&model.LotteryActivity{}).
			Where("id = ?", activityID).
			Updates(map[string]interface{}{
				"total_refund_points": txRefundTotal,
			})
		if totalRes.Error != nil {
			return totalRes.Error
		}
		if totalRes.RowsAffected == 0 {
			return fmt.Errorf("LOTTERY_CANCEL_TOTAL_REFUND_UPDATE_MISSED")
		}
		if err := storage.WriteAuditLogInTx(tx, actorID, "CANCEL_LOTTERY", fmt.Sprintf("%d", activityID), 0, fmt.Sprintf("超级管理员取消积分抽奖活动，退还参与积分 %d", txRefundTotal)); err != nil {
			return err
		}
		if activity.AnnounceIsChannel {
			return announcements.Queue(tx, model.AnnouncementDelivery{Key: fmt.Sprintf("lottery:%d:cancel", activity.ID), Kind: "lottery_cancel", TargetChatID: activity.AnnounceChatID, ActorID: actorID, Text: fmt.Sprintf("🎲 抽奖《%s》已取消\n已全额退还参与积分，无需手动申请。", textfmt.LotteryDisplayText(activity.Title, 80, "-"))})
		}
		return nil
	})

	if err != nil {
		return -1, err
	}
	return txRefundTotal, nil
}

func parseTrailingUint(text string) (uint64, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	return id, err == nil && id > 0
}

func countLotteryParticipants(activityID uint) (int64, error) {
	var count int64
	if err := storage.DB.Model(&model.LotteryParticipant{}).
		Where("activity_id = ? AND deleted_at IS NULL", activityID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func lotteryStatusText(status string) string {
	switch status {
	case lotteryStatusActive:
		return "进行中"
	case lotteryStatusDrawing:
		return "开奖中"
	case lotteryStatusDrawn:
		return "已开奖"
	case lotteryStatusClosed:
		return "已关闭"
	default:
		return "未知状态"
	}
}

func shuffleLotteryParticipants(items []model.LotteryParticipant) {
	for i := len(items) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			continue
		}
		j := int(n.Int64())
		items[i], items[j] = items[j], items[i]
	}
}

func shuffleLotteryPrizes(items []model.LotteryPrize) {
	for i := len(items) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			continue
		}
		j := int(n.Int64())
		items[i], items[j] = items[j], items[i]
	}
}

func StartLotteryScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			runLotterySchedulerTick(bot)
		}
	}()
	log.Println("✅ 积分抽奖定时开奖调度器已启动。")
}

func runLotterySchedulerTick(bot *tgbotapi.BotAPI) {
	now := time.Now()
	var dueActivities []model.LotteryActivity
	if err := storage.DB.Where("status = ? AND mode = ? AND draw_at IS NOT NULL AND draw_at <= ?", lotteryStatusActive, lotteryModeTime, now).
		Order("draw_at asc").
		Limit(20).
		Find(&dueActivities).Error; err != nil {
		log.Printf("⚠️ 查询到期抽奖失败: %s", textfmt.FormatPlainError(err))
		return
	}

	for _, activity := range dueActivities {
		if err := drawLotteryActivity(bot, activity.ID, "scheduled", 0); err != nil && !errors.Is(err, storage.ErrLotteryNotActive) {
			log.Printf("⚠️ 定时抽奖开奖失败: activity=%d err=%s", activity.ID, textfmt.FormatPlainError(err))
			telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 定时抽奖 #%d 开奖失败：%s", activity.ID, textfmt.FormatPlainError(err)))
		}
	}

	expirePendingLotteryRewards(now)
	unpinExpiredLotteryMessages(bot, now)
}

func unpinExpiredLotteryMessages(bot *tgbotapi.BotAPI, now time.Time) {
	var activities []model.LotteryActivity
	if err := storage.DB.Where("status IN ? AND claim_deadline_at IS NOT NULL AND claim_deadline_at <= ? AND pins_unpinned = ?", []string{lotteryStatusDrawn, lotteryStatusClosed}, now, false).
		Order("claim_deadline_at asc").
		Limit(20).
		Find(&activities).Error; err != nil {
		log.Printf("⚠️ 查询待取消置顶抽奖失败: %s", textfmt.FormatPlainError(err))
		return
	}

	for _, activity := range activities {
		chatID := activity.AnnounceChatID
		if chatID == 0 {
			chatID = config.AppConfig.NoticeGroupID
		}
		if chatID != 0 {
			if activity.IntroMessageID > 0 && activity.IntroPinned {
				unpinLotteryMessage(bot, chatID, activity.IntroMessageID, activity.ID, "intro")
			}
			if activity.ResultMessageID > 0 && activity.ResultPinned {
				unpinLotteryMessage(bot, chatID, activity.ResultMessageID, activity.ID, "result")
			}
		}

		res := storage.DB.Model(&model.LotteryActivity{}).
			Where("id = ? AND status IN ? AND claim_deadline_at IS NOT NULL AND claim_deadline_at <= ? AND pins_unpinned = ?", activity.ID, []string{lotteryStatusDrawn, lotteryStatusClosed}, now, false).
			Update("pins_unpinned", true)
		if res.Error != nil {
			log.Printf("⚠️ 标记抽奖置顶清理完成失败: activity=%d err=%s", activity.ID, textfmt.FormatPlainError(res.Error))
			continue
		}
		if res.RowsAffected == 0 {
			log.Printf("⚠️ 抽奖置顶清理完成标记未命中: activity=%d", activity.ID)
		}
	}
}

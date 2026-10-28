package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func createSectInTx(tx *gorm.DB, sect *model.Sect) error {
	if tx == nil || sect == nil {
		return fmt.Errorf("SECT_INVALID")
	}
	entry := *sect
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrSectNameExists
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_CREATE_MISSED")
	}
	*sect = entry
	return nil
}

func createSectMemberInTx(tx *gorm.DB, member *model.SectMember) error {
	if tx == nil || member == nil {
		return fmt.Errorf("SECT_MEMBER_INVALID")
	}
	entry := *member
	res := tx.Create(&entry)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrAlreadyInSect
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_MEMBER_CREATE_MISSED")
	}
	*member = entry
	return nil
}

func readSectFundsInTx(tx *gorm.DB, sectID int64) (int, error) {
	if tx == nil || sectID <= 0 {
		return 0, fmt.Errorf("INVALID_SECT_FUNDS_READ")
	}

	var sect model.Sect
	if err := tx.Select("funds").Where("id = ?", sectID).First(&sect).Error; err != nil {
		return 0, err
	}
	return sect.Funds, nil
}

func readSectPrestigeInTx(tx *gorm.DB, sectID int64) (int, error) {
	if tx == nil || sectID <= 0 {
		return 0, fmt.Errorf("INVALID_SECT_PRESTIGE_READ")
	}

	var sect model.Sect
	if err := tx.Select("prestige").Where("id = ?", sectID).First(&sect).Error; err != nil {
		return 0, err
	}
	return sect.Prestige, nil
}

func readSectMemberContributionInTx(tx *gorm.DB, memberID uint) (int, error) {
	if tx == nil || memberID == 0 {
		return 0, fmt.Errorf("INVALID_SECT_MEMBER_CONTRIBUTION_READ")
	}

	var member model.SectMember
	if err := tx.Select("contribution").Where("id = ?", memberID).First(&member).Error; err != nil {
		return 0, err
	}
	return member.Contribution, nil
}

func loadSectMemberByUserInTx(tx *gorm.DB, userID int64, member *model.SectMember, forUpdate bool) error {
	if tx == nil || member == nil || userID <= 0 {
		return fmt.Errorf("INVALID_SECT_MEMBER_READ")
	}
	query := tx
	if forUpdate {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.Where("user_id = ?", userID).First(member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return storage.ErrNotInSect
		}
		return err
	}
	return nil
}

func loadTargetSectMemberByUserInTx(tx *gorm.DB, userID int64, sectID int64, member *model.SectMember) error {
	if tx == nil || member == nil || userID <= 0 || sectID <= 0 {
		return fmt.Errorf("INVALID_TARGET_SECT_MEMBER_READ")
	}
	if err := tx.Where("user_id = ? AND sect_id = ?", userID, sectID).First(member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return storage.ErrTargetNotInSect
		}
		return err
	}
	return nil
}

func replySectMemberReadError(bot *tgbotapi.BotAPI, chatID int64, userID int64, err error, notInSectText string, logLabel string) {
	if errors.Is(err, storage.ErrNotInSect) {
		telegram.ReplyText(bot, chatID, notInSectText)
		return
	}
	log.Printf("⚠️ %s读取宗门成员失败: user=%d err=%s", textfmt.FormatPlainValue(logLabel), userID, textfmt.FormatPlainError(err))
	telegram.ReplyText(bot, chatID, "宗门成员档案读取失败，请稍后再试。")
}

func createSectDailyTaskClaimInTx(tx *gorm.DB, claim *model.SectDailyTaskClaim) error {
	if tx == nil || claim == nil {
		return fmt.Errorf("SECT_DAILY_TASK_CLAIM_INVALID")
	}
	res := tx.Create(claim)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_DAILY_TASK_CLAIM_CREATE_MISSED")
	}
	return nil
}

func createSectWeeklyTaskSettlementInTx(tx *gorm.DB, settlement *model.SectWeeklyTaskSettlement) error {
	if tx == nil || settlement == nil {
		return fmt.Errorf("SECT_WEEKLY_TASK_SETTLEMENT_INVALID")
	}
	settlement.WeekKey = textfmt.FormatPlainValue(settlement.WeekKey)
	settlement.SettledByName = textfmt.FormatPlainValue(settlement.SettledByName)
	res := tx.Create(settlement)
	if res.Error != nil {
		if storage.IsUniqueConstraintError(res.Error) {
			return storage.ErrSectWeeklyTaskAlreadySettled
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_WEEKLY_TASK_SETTLEMENT_CREATE_MISSED")
	}
	return nil
}

func createSectShopPurchaseInTx(tx *gorm.DB, purchase *model.SectShopPurchase) error {
	if tx == nil || purchase == nil {
		return fmt.Errorf("SECT_SHOP_PURCHASE_INVALID")
	}
	purchase.ExchangeType = textfmt.FormatPlainValue(purchase.ExchangeType)
	purchase.DayKey = textfmt.FormatPlainValue(purchase.DayKey)
	res := tx.Create(purchase)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_SHOP_PURCHASE_CREATE_MISSED")
	}
	return nil
}

func createSectCaveRetreatInTx(tx *gorm.DB, retreat *model.SectCaveRetreat) error {
	if tx == nil || retreat == nil {
		return fmt.Errorf("SECT_CAVE_RETREAT_INVALID")
	}
	retreat.Mode = textfmt.FormatPlainValue(retreat.Mode)
	retreat.Status = textfmt.FormatPlainValue(retreat.Status)
	retreat.StartedByName = textfmt.FormatPlainValue(retreat.StartedByName)
	res := tx.Create(retreat)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_CAVE_RETREAT_CREATE_MISSED")
	}
	return nil
}

func canViewSectListeningStats(member model.SectMember) bool {
	return member.Role == SectRoleOwner || member.Role == SectRoleElder
}

type sectContributionRankRow struct {
	UserID             int64
	UserName           string
	Role               string
	JoinedAt           time.Time
	Contribution       int
	WeeklyContribution int
	TotalContribution  int
}

const (
	sectCreateCost               = 100
	sectJoinCost                 = 10
	sectRenameCost               = 50
	sectInitialMaxUsers          = 20
	sectMaxLevel                 = 10
	sectMemberListPageSize       = 30
	sectMemberPageCallbackPrefix = "sect_members_page:"

	SectRoleOwner  = "owner"
	SectRoleElder  = "elder"
	sectRoleMember = "member"

	sectContributionToPrestigeCost = 3 // 3 贡献 = 1 宗门声望 + 1 个人声望
	sectShopMaxExchangeAmount      = 10000
	sectShopRenewExchangeType      = "renew_7d"
	sectShopRenewDays              = 7
	sectShopRenewContributionCost  = 105
	sectShopRenewPersonalMonthMax  = 1
	sectShopRenewMinJoinedDays     = 7
	sectShopRenewMinTotalContrib   = 300
	sectShopRenewMaxExpireDays     = 45

	sectCaveUnlockLevel            = 4
	sectCaveUnlockContributionCost = 100
	sectCavePersonalRetreatCost    = 8
	sectCavePersonalRetreatHours   = 2
	sectCaveSectRetreatBaseCost    = 60
	sectCaveSectRetreatLevelCost   = 10
	sectCaveSectRetreatHours       = 4
	sectRetreatStatusActive        = "active"
	sectRetreatModePersonal        = "personal"
	sectRetreatModeSect            = "sect"

	sectNameInvalidText = "宗门名需为 2-12 个字符，且不能包含空格、控制字符或 Markdown 特殊符号。"
)

func sectPointDescriptionName(name string) string {
	return textfmt.LotteryDisplayText(name, 80, "-")
}

func sectContributionLogReason(reason string) string {
	return strings.TrimSpace(textfmt.FormatPlainValue(reason))
}

func createSectContributionLogInTx(tx *gorm.DB, logEntry *model.SectContributionLog) error {
	if tx == nil || logEntry == nil {
		return fmt.Errorf("SECT_CONTRIBUTION_LOG_INVALID")
	}
	entry := *logEntry
	entry.Reason = sectContributionLogReason(entry.Reason)
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_CONTRIBUTION_LOG_CREATE_MISSED")
	}
	return nil
}

func HandleSectCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil {
		return false
	}

	text = strings.TrimSpace(text)
	var run func()

	switch {
	case strings.HasPrefix(text, "确认创建宗门 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "确认创建宗门 "))
		run = func() { handleConfirmCreateSect(bot, msg, name) }
	case strings.HasPrefix(text, "创建宗门 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "创建宗门 "))
		run = func() { handleCreateSect(bot, msg, name) }
	case strings.HasPrefix(text, "确认加入宗门 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "确认加入宗门 "))
		run = func() { handleConfirmJoinSect(bot, msg, name) }
	case strings.HasPrefix(text, "加入宗门 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "加入宗门 "))
		run = func() { handleJoinSect(bot, msg, name) }
	case strings.HasPrefix(text, "捐献宗门 "):
		amount := strings.TrimSpace(strings.TrimPrefix(text, "捐献宗门 "))
		run = func() { HandleDonateSect(bot, msg, amount) }
	case strings.HasPrefix(text, "贡献换积分 "):
		run = func() { handleDisabledSectContributionForPoints(bot, msg) }
	case strings.HasPrefix(text, "贡献换声望 "):
		amount := strings.TrimSpace(strings.TrimPrefix(text, "贡献换声望 "))
		run = func() { handleExchangeSectContributionForPrestige(bot, msg, amount) }
	case text == "宗门七日续期":
		run = func() { handleExchangeSectRenew(bot, msg, false) }
	case text == "确认宗门七日续期":
		run = func() { handleExchangeSectRenew(bot, msg, true) }
	case text == "确认解锁洞府":
		run = func() { handleUnlockSectCave(bot, msg, true) }
	case text == "解锁洞府":
		run = func() { handleUnlockSectCave(bot, msg, false) }
	case text == "确认闭关":
		run = func() { handleStartPersonalSectCaveRetreat(bot, msg, true) }
	case text == "闭关":
		run = func() { handleStartPersonalSectCaveRetreat(bot, msg, false) }
	case text == "确认宗门闭关":
		run = func() { handleStartSectCaveRetreat(bot, msg, true) }
	case text == "宗门闭关":
		run = func() { handleStartSectCaveRetreat(bot, msg, false) }
	case strings.HasPrefix(text, "任命长老 "):
		target := strings.TrimSpace(strings.TrimPrefix(text, "任命长老 "))
		run = func() { handleAppointSectRole(bot, msg, target, SectRoleElder) }
	case strings.HasPrefix(text, "任命成员 "):
		target := strings.TrimSpace(strings.TrimPrefix(text, "任命成员 "))
		run = func() { handleAppointSectRole(bot, msg, target, sectRoleMember) }
	case strings.HasPrefix(text, "踢出宗门 "):
		target := strings.TrimSpace(strings.TrimPrefix(text, "踢出宗门 "))
		run = func() { handleKickSectMember(bot, msg, target) }
	case strings.HasPrefix(text, "转让宗主 "):
		target := strings.TrimSpace(strings.TrimPrefix(text, "转让宗主 "))
		run = func() { handleTransferSectOwner(bot, msg, target) }
	case strings.HasPrefix(text, "确认升级科技 "):
		key := strings.TrimSpace(strings.TrimPrefix(text, "确认升级科技 "))
		run = func() { handleUpgradeSectTechnology(bot, msg, key, true) }
	case strings.HasPrefix(text, "升级科技 "):
		key := strings.TrimSpace(strings.TrimPrefix(text, "升级科技 "))
		run = func() { handleUpgradeSectTechnology(bot, msg, key, false) }
	case strings.HasPrefix(text, "宗门喇叭 "):
		content := strings.TrimSpace(strings.TrimPrefix(text, "宗门喇叭 "))
		run = func() { handleSectHornStart(bot, msg, sectHornScopeSect, content) }
	case strings.HasPrefix(text, "世界喇叭 "):
		content := strings.TrimSpace(strings.TrimPrefix(text, "世界喇叭 "))
		run = func() { handleSectHornStart(bot, msg, sectHornScopeWorld, content) }
	// 注意：「确认宗门喇叭 / 确认世界喇叭」不在此路由，
	// 必须落到 state_machine 的 WAITING_CONFIRM_SECT_HORN 会话分支，
	// 由 handleSectHornSession 读取会话中暂存的正文，否则会被当成空正文。
	case strings.HasPrefix(text, "确认宗门改名 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "确认宗门改名 "))
		run = func() { handleConfirmRenameSect(bot, msg, name) }
	case strings.HasPrefix(text, "确认修改宗门名称 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "确认修改宗门名称 "))
		run = func() { handleConfirmRenameSect(bot, msg, name) }
	case strings.HasPrefix(text, "宗门改名 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "宗门改名 "))
		run = func() { handleRenameSect(bot, msg, name) }
	case strings.HasPrefix(text, "修改宗门名称 "):
		name := strings.TrimSpace(strings.TrimPrefix(text, "修改宗门名称 "))
		run = func() { handleRenameSect(bot, msg, name) }
	case text == "宗门科技":
		run = func() { handleSectTechnology(bot, msg) }
	case text == "升级宗门":
		run = func() { handleUpgradeSect(bot, msg) }
	case text == "退出宗门":
		run = func() { handleExitSect(bot, msg) }
	case text == "我的宗门":
		run = func() { handleMySect(bot, msg) }
	case text == "宗门排行":
		run = func() { handleSectRank(bot, msg) }
	case text == "宗门成员":
		run = func() { handleSectMembers(bot, msg) }
	case text == "宗门贡献榜":
		run = func() { handleSectContributionRank(bot, msg, false) }
	case text == "宗门周榜":
		run = func() { handleSectContributionRank(bot, msg, true) }
	case text == "宗门任务":
		run = func() { handleSectTasks(bot, msg) }
	case text == "我的宗门任务":
		run = func() { handleMySectTasks(bot, msg) }
	case text == "领取宗门任务奖励":
		run = func() { handleClaimSectTaskReward(bot, msg) }
	case text == "结算宗门周目标":
		run = func() { handleSettleSectWeeklyTaskReward(bot, msg) }
	case text == "宗门商店":
		run = func() { handleSectShop(bot, msg) }
	case text == "洞府":
		run = func() { handleSectCave(bot, msg) }

	}

	if run == nil {
		return false
	}

	telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	run()
	return true
}

func validateSectName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 12 {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	if textfmt.ContainsDisallowedControl(name, false) {
		return "", false
	}
	if strings.ContainsAny(name, " \r\n\t`*_[]") {
		return "", false
	}
	return name, true
}

func getSectMaxMembers(level int) int {
	if level <= 1 {
		return sectInitialMaxUsers
	}
	return sectInitialMaxUsers + (level-1)*10
}

func getSectUpgradeRequirement(level int) (fundsCost int, prestigeNeed int) {
	if level < 1 {
		level = 1
	}

	fundsCost = level * level * 100
	prestigeNeed = level * 50
	return fundsCost, prestigeNeed
}

func getSectRoleText(role string) string {
	switch role {
	case SectRoleOwner:
		return "宗主"
	case SectRoleElder:
		return "长老"
	default:
		return "成员"
	}
}

func sectMaxMembersDisplayText(sect model.Sect, context string, userID int64) string {
	maxMembers, err := getSectMaxMembersWithTechTxChecked(storage.DB, sect)
	if err != nil {
		log.Printf("sect max members display read failed: context=%s sect=%d user=%d err=%s", textfmt.FormatPlainValue(context), sect.ID, userID, textfmt.FormatPlainError(err))
		return "读取失败"
	}
	return strconv.Itoa(maxMembers)
}

func canManageSectMember(operatorRole string, targetRole string) bool {
	if operatorRole == SectRoleOwner {
		return targetRole != SectRoleOwner
	}

	if operatorRole == SectRoleElder {
		return targetRole == sectRoleMember
	}

	return false
}

func canUpgradeSectAsset(role string) bool {
	return role == SectRoleOwner || role == SectRoleElder
}

func parseSectTargetUserID(raw string) (int64, error) {
	targetID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || targetID <= 0 {
		return 0, fmt.Errorf("invalid Telegram ID")
	}
	return targetID, nil
}

func getTelegramNameForSect(user *tgbotapi.User) string {
	if user == nil {
		return "unknown"
	}
	if strings.TrimSpace(user.UserName) != "" {
		return user.UserName
	}
	if strings.TrimSpace(user.FirstName) != "" {
		return user.FirstName
	}
	return fmt.Sprintf("%d", user.ID)
}

func SectErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrNotInSect):
		return "NOT_IN_SECT"
	case errors.Is(err, storage.ErrTargetNotInSect):
		return "TARGET_NOT_IN_SECT"
	case errors.Is(err, storage.ErrAlreadyInSect):
		return "ALREADY_IN_SECT"
	case errors.Is(err, storage.ErrSectNameExists):
		return "SECT_NAME_EXISTS"
	case errors.Is(err, storage.ErrSectNotFound):
		return "SECT_NOT_FOUND"
	case errors.Is(err, storage.ErrSectFull):
		return "SECT_FULL"
	case errors.Is(err, storage.ErrSectNoPermission):
		return "NO_PERMISSION"
	case errors.Is(err, storage.ErrSectSameName):
		return "SAME_NAME"
	case errors.Is(err, storage.ErrSectFundsNotEnough):
		return "FUNDS_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectOnlyOwner):
		return "ONLY_OWNER"
	case errors.Is(err, storage.ErrSectMaxLevel):
		return "MAX_LEVEL"
	case errors.Is(err, storage.ErrSectPrestigeNotEnough):
		return "PRESTIGE_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectResourceNotEnough):
		return "RESOURCE_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectCannotAppointOwner):
		return "CANNOT_APPOINT_OWNER"
	case errors.Is(err, storage.ErrSectCaveLocked):
		return "SECT_CAVE_LOCKED"
	case errors.Is(err, storage.ErrSectCaveAlreadyUnlocked):
		return "SECT_CAVE_ALREADY_UNLOCKED"
	case errors.Is(err, storage.ErrSectPersonalPrestigeNotEnough):
		return "PERSONAL_PRESTIGE_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectContributionNotEnough):
		return "CONTRIBUTION_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectRetreatActive):
		return "SECT_RETREAT_ACTIVE"
	case errors.Is(err, storage.ErrSectRetreatNoEligibleMembers):
		return "SECT_RETREAT_NO_ELIGIBLE_MEMBERS"
	case errors.Is(err, storage.ErrUserNotFound):
		return "USER_NOT_FOUND"
	case errors.Is(err, storage.ErrTrialCannotUseRenewCode):
		return "TRIAL_CANNOT_USE_RENEW_CODE"
	case errors.Is(err, storage.ErrAbsUserIDEmpty):
		return "ABS_USER_ID_EMPTY"
	case errors.Is(err, storage.ErrSectShopRenewMonthlyLimit):
		return "SECT_SHOP_RENEW_MONTHLY_LIMIT"
	case errors.Is(err, storage.ErrSectShopRenewSectLimit):
		return "SECT_SHOP_RENEW_SECT_LIMIT"
	case errors.Is(err, storage.ErrSectShopRenewJoinedTooRecent):
		return "SECT_SHOP_RENEW_JOINED_TOO_RECENT"
	case errors.Is(err, storage.ErrSectShopRenewHistoryTooLow):
		return "SECT_SHOP_RENEW_HISTORY_TOO_LOW"
	case errors.Is(err, storage.ErrSectShopRenewExpireLimit):
		return "SECT_SHOP_RENEW_EXPIRE_LIMIT"
	case errors.Is(err, storage.ErrSectShopRenewPermanent):
		return "SECT_SHOP_RENEW_PERMANENT"
	case errors.Is(err, storage.ErrSectDailyTaskNotAllCompleted):
		return "SECT_DAILY_TASK_NOT_ALL_COMPLETED"
	case errors.Is(err, storage.ErrSectDailyTaskAlreadyClaimed):
		return "ALREADY_CLAIMED"
	case errors.Is(err, storage.ErrSectWeeklyTaskNotAchieved):
		return "SECT_WEEKLY_TASK_NOT_ACHIEVED"
	case errors.Is(err, storage.ErrSectWeeklyTaskAlreadySettled):
		return "SECT_WEEKLY_TASK_ALREADY_SETTLED"
	case err != nil:
		return storage.FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func handleCreateSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawName string) {
	chatID := msg.Chat.ID

	name, ok := validateSectName(rawName)
	if !ok {
		telegram.ReplyText(bot, chatID, sectNameInvalidText)
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"确认创建宗门 **%s** 将消耗 `%d` 积分。\n\n确认无误请发送：`确认创建宗门 %s`",
		textfmt.EscapeMarkdown(name),
		sectCreateCost,
		name,
	))
}

func handleConfirmCreateSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawName string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	name, ok := validateSectName(rawName)
	if !ok {
		telegram.ReplyText(bot, chatID, sectNameInvalidText)
		return
	}

	userName := getTelegramNameForSect(msg.From)

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var existing model.SectMember
		if err := tx.Where("user_id = ?", userID).First(&existing).Error; err == nil {
			return storage.ErrAlreadyInSect
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var user model.User
		if err := tx.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}

		sect := model.Sect{
			Name:        name,
			Level:       1,
			Prestige:    0,
			Funds:       0,
			MemberCount: 1,
			OwnerID:     userID,
			OwnerName:   userName,
		}
		if err := createSectInTx(tx, &sect); err != nil {
			return err
		}

		if err := storage.ApplyPointDeltaInTx(
			tx,
			userID,
			-sectCreateCost,
			"sect_create",
			fmt.Sprintf("创建宗门 %s，消耗 %d 积分", sectPointDescriptionName(name), sectCreateCost),
			"sect",
			fmt.Sprintf("%d", sect.ID),
		); err != nil {
			return err
		}

		member := model.SectMember{
			SectID:   int64(sect.ID),
			UserID:   userID,
			UserName: userName,
			Role:     SectRoleOwner,
			JoinedAt: time.Now(),
		}
		if err := createSectMemberInTx(tx, &member); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		switch {
		case errors.Is(err, storage.ErrAlreadyInSect):
			telegram.ReplyText(bot, chatID, "你已经加入宗门，不能重复创建。")
		case errors.Is(err, storage.ErrUserNotFound):
			telegram.ReplyText(bot, chatID, "未找到你的本地档案，请先完成注册。")
		case errors.Is(err, storage.ErrPointsNotEnough):
			telegram.ReplyText(bot, chatID, fmt.Sprintf("积分不足，创建宗门需要 `%d` 积分。", sectCreateCost))
		case errors.Is(err, storage.ErrSectNameExists):
			telegram.ReplyText(bot, chatID, "该宗门名已存在，请换一个名字。")
		default:
			log.Printf("create sect failed: user=%d name=%s err=%s", userID, textfmt.FormatPlainValue(name), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "创建宗门失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门 **%s** 创建成功。\n\n消耗积分：`%d`\n成员上限：`%d` 人",
		textfmt.EscapeMarkdown(name),
		sectCreateCost,
		sectInitialMaxUsers,
	))
}

func handleRenameSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawName string) {
	chatID := msg.Chat.ID

	name, ok := validateSectName(rawName)
	if !ok {
		telegram.ReplyText(bot, chatID, sectNameInvalidText)
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门改名将消耗 `%d` 宗门资金。\n\n新名称：`%s`\n确认请发送：`确认宗门改名 %s`",
		sectRenameCost,
		textfmt.EscapeMarkdown(name),
		name,
	))
}

func handleConfirmRenameSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawName string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	newName, ok := validateSectName(rawName)
	if !ok {
		telegram.ReplyText(bot, chatID, sectNameInvalidText)
		return
	}

	var oldName string
	var fundsAfter int

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, false); err != nil {
			return err
		}

		if !canUpgradeSectAsset(member.Role) {
			return storage.ErrSectNoPermission
		}

		var sect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", member.SectID).
			First(&sect).Error; err != nil {
			return err
		}

		if sect.Name == newName {
			return storage.ErrSectSameName
		}
		if sect.Funds < sectRenameCost {
			return storage.ErrSectFundsNotEnough
		}

		oldName = sect.Name
		fundsAfter = sect.Funds - sectRenameCost

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND funds >= ?", sect.ID, sectRenameCost).
			Updates(map[string]interface{}{
				"name":  newName,
				"funds": gorm.Expr("funds - ?", sectRenameCost),
			})
		if res.Error != nil {
			if storage.IsUniqueConstraintError(res.Error) {
				return storage.ErrSectNameExists
			}
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectFundsNotEnough
		}

		return createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       member.SectID,
			UserID:       userID,
			Delta:        -sectRenameCost,
			Reason:       fmt.Sprintf("宗门改名 %s -> %s，消耗资金 %d", sectPointDescriptionName(oldName), sectPointDescriptionName(newName), sectRenameCost),
			RefType:      "sect_rename",
			RefID:        time.Now().Format("20060102150405"),
			BalanceAfter: fundsAfter,
		})
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门。")
		case "NO_PERMISSION":
			telegram.ReplyText(bot, chatID, "只有宗主或长老可以修改宗门名称。")
		case "SAME_NAME":
			telegram.ReplyText(bot, chatID, "新名称与当前名称相同。")
		case "FUNDS_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门资金不足，改名需要 `%d` 资金。", sectRenameCost))
		case "SECT_NAME_EXISTS":
			telegram.ReplyText(bot, chatID, "该宗门名已存在，请换一个名字。")
		default:
			log.Printf("rename sect failed: user=%d new_name=%s err=%s", userID, textfmt.FormatPlainValue(newName), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗门改名失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门改名成功。\n\n旧名称：`%s`\n新名称：`%s`\n消耗资金：`%d`\n剩余资金：`%d`",
		textfmt.EscapeMarkdown(oldName),
		textfmt.EscapeMarkdown(newName),
		sectRenameCost,
		fundsAfter,
	))
}

func handleJoinSect(bot *tgbotapi.BotAPI, message *tgbotapi.Message, args string) {

	name, ok := validateSectName(args)
	if !ok {
		telegram.ReplyTextToMessage(bot, message, sectNameInvalidText)
		return
	}

	telegram.ReplyTextToMessage(bot, message, fmt.Sprintf(
		"加入宗门 **%s** 将消耗 `%d` 积分，加入后会增加等额个人贡献。\n\n确认请发送：`确认加入宗门 %s`",
		textfmt.EscapeMarkdown(name),
		sectJoinCost,
		name,
	))
}

func handleConfirmJoinSect(bot *tgbotapi.BotAPI, message *tgbotapi.Message, args string) {
	userID := message.From.ID
	userName := textfmt.GetTelegramDisplayName(message.From)

	name, ok := validateSectName(args)
	if !ok {
		telegram.ReplyTextToMessage(bot, message, sectNameInvalidText)
		return
	}

	var joinedSectName string

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var existingMember model.SectMember
		err := tx.Where("user_id = ?", userID).First(&existingMember).Error
		if err == nil {
			return storage.ErrAlreadyInSect
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var sect model.Sect
		if err := tx.Where("name = ?", name).First(&sect).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrSectNotFound
			}
			return err
		}

		maxMembers, err := getSectMaxMembersWithTechTxChecked(tx, sect)
		if err != nil {
			return err
		}
		if sect.MemberCount >= maxMembers {
			return storage.ErrSectFull
		}

		if err := storage.ApplyPointDeltaInTx(
			tx,
			userID,
			-sectJoinCost,
			"sect_join",
			fmt.Sprintf("加入宗门 %s，消耗 %d 积分", sectPointDescriptionName(sect.Name), sectJoinCost),
			"sect",
			fmt.Sprintf("%d", sect.ID),
		); err != nil {
			return err
		}

		member := model.SectMember{
			SectID:             int64(sect.ID),
			UserID:             userID,
			UserName:           userName,
			Role:               sectRoleMember,
			Contribution:       sectJoinCost,
			WeeklyContribution: sectJoinCost,
			JoinedAt:           time.Now(),
		}
		if err := createSectMemberInTx(tx, &member); err != nil {
			return err
		}

		sectUpdateRes := tx.Model(&model.Sect{}).
			Where("id = ? AND member_count < ?", sect.ID, maxMembers).
			Updates(map[string]interface{}{
				"funds":        gorm.Expr("funds + ?", sectJoinCost),
				"member_count": gorm.Expr("member_count + ?", 1),
			})
		if sectUpdateRes.Error != nil {
			return sectUpdateRes.Error
		}
		if sectUpdateRes.RowsAffected == 0 {
			return storage.ErrSectFull
		}

		fundsAfter, err := readSectFundsInTx(tx, int64(sect.ID))
		if err != nil {
			return err
		}

		if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       int64(sect.ID),
			UserID:       userID,
			Delta:        sectJoinCost,
			Reason:       "加入宗门",
			RefType:      "sect_join",
			RefID:        fmt.Sprintf("%d", time.Now().UnixNano()),
			BalanceAfter: fundsAfter,
		}); err != nil {
			return err
		}

		joinedSectName = sect.Name
		return nil
	})

	if err != nil {
		switch {
		case errors.Is(err, storage.ErrAlreadyInSect):
			telegram.ReplyTextToMessage(bot, message, "你已经加入宗门，不能重复加入。")
		case errors.Is(err, storage.ErrSectNotFound):
			telegram.ReplyTextToMessage(bot, message, "未找到该宗门。")
		case errors.Is(err, storage.ErrSectFull):
			telegram.ReplyTextToMessage(bot, message, "该宗门成员已满。")
		case errors.Is(err, storage.ErrPointsNotEnough):
			telegram.ReplyTextToMessage(bot, message, fmt.Sprintf("积分不足，加入宗门需要 `%d` 积分。", sectJoinCost))
		default:
			log.Printf("join sect failed: user=%d sect=%s err=%s", userID, textfmt.FormatPlainValue(name), textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, message, "加入宗门失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyTextToMessage(bot, message, fmt.Sprintf("已加入宗门 **%s**。\n\n消耗积分：`%d`\n个人贡献：`+%d`", textfmt.EscapeMarkdown(joinedSectName), sectJoinCost, sectJoinCost))
}

func handleMySect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, msg.From.ID, &member, false); err != nil {
		replySectMemberReadError(bot, msg.Chat.ID, msg.From.ID, err, "你尚未加入宗门。\n可发送 `创建宗门 名称` 创建宗门，或发送 `加入宗门 名称` 加入已有宗门。", "我的宗门")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, msg.Chat.ID, "宗门档案读取失败，请稍后再试。")
		return
	}

	roleText := getSectRoleText(member.Role)
	maxMembersText := sectMaxMembersDisplayText(sect, "my_sect", msg.From.ID)
	nextFundsCost, nextPrestigeNeed := getSectUpgradeRequirement(sect.Level)
	upgradeText := "已达最高等级"
	if sect.Level < sectMaxLevel {
		upgradeText = fmt.Sprintf("下级需要资金 `%d`，声望 `%d`", nextFundsCost, nextPrestigeNeed)
	}
	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf(
		"**我的宗门**\n\n"+
			"宗门：**%s**\n"+
			"职位：`%s`\n"+
			"等级：`%d`\n"+
			"声望：`%d`\n"+
			"资金：`%d`\n"+
			"成员：`%d/%s`\n\n"+
			"升级：%s\n\n"+
			"个人贡献：`%d`\n"+
			"本周贡献：`%d`\n"+
			"个人声望：`%d`\n"+
			"洞府：`%s`",
		textfmt.EscapeMarkdown(sect.Name),
		roleText,
		sect.Level,
		sect.Prestige,
		sect.Funds,
		sect.MemberCount,
		maxMembersText,
		upgradeText,
		member.Contribution,
		member.WeeklyContribution,
		member.PersonalPrestige,
		func() string {
			if member.CaveUnlockedAt != nil {
				return "已解锁"
			}
			if sect.Level >= sectCaveUnlockLevel {
				return "可解锁"
			}
			return "未开放"
		}(),
	))
}

func handleSectRank(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	var sects []model.Sect
	if err := storage.DB.Order("level DESC, prestige DESC, funds DESC, member_count DESC").
		Limit(10).
		Find(&sects).Error; err != nil {
		telegram.ReplyText(bot, msg.Chat.ID, "宗门排行榜读取失败，请稍后再试。")
		return
	}

	if len(sects) == 0 {
		telegram.ReplyText(bot, msg.Chat.ID, "暂无宗门上榜。")
		return
	}

	medals := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	var b strings.Builder
	b.WriteString("**宗门排行榜 Top 10**\n\n")

	for i, sect := range sects {
		maxMembersText := sectMaxMembersDisplayText(sect, "sect_rank", msg.From.ID)
		b.WriteString(fmt.Sprintf(
			"%s **%s**\n等级 `%d` 声望 `%d` 资金 `%d` 成员 `%d/%s`\n宗主：`%s`\n\n",
			medals[i],
			textfmt.EscapeMarkdown(sect.Name),
			sect.Level,
			sect.Prestige,
			sect.Funds,
			sect.MemberCount,
			maxMembersText,
			textfmt.EscapeMarkdown(sect.OwnerName),
		))
	}

	telegram.ReplyText(bot, msg.Chat.ID, b.String())
}

func handleSectMembers(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	page := 1
	var myMember model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, msg.From.ID, &myMember, false); err != nil {
		replySectMemberReadError(bot, msg.Chat.ID, msg.From.ID, err, "你尚未加入宗门。", "宗门成员")
		return
	}

	sect, members, totalMembers, page, err := loadSectMemberListPage(myMember.SectID, page)
	if err != nil {
		telegram.ReplyText(bot, msg.Chat.ID, "宗门成员列表读取失败，请稍后再试。")
		return
	}

	sendSectMemberListPage(bot, msg.Chat.ID, sect, members, totalMembers, page)
}

func HandleSectMemberPageCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || cb.From == nil || cb.Message == nil || cb.Message.Chat == nil {
		return false
	}
	if !strings.HasPrefix(cb.Data, sectMemberPageCallbackPrefix) {
		return false
	}

	pageText := strings.TrimPrefix(cb.Data, sectMemberPageCallbackPrefix)
	page, err := strconv.Atoi(pageText)
	if err != nil || page < 1 {
		telegram.AnswerCallback(bot, cb.ID, "页码无效")
		return true
	}

	var myMember model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, cb.From.ID, &myMember, false); err != nil {
		if errors.Is(err, storage.ErrNotInSect) {
			telegram.AnswerCallback(bot, cb.ID, "你尚未加入宗门")
		} else {
			log.Printf("⚠️ 宗门成员分页读取成员失败: user=%d err=%s", cb.From.ID, textfmt.FormatPlainError(err))
			telegram.AnswerCallback(bot, cb.ID, "成员档案读取失败")
		}
		return true
	}

	sect, members, totalMembers, actualPage, err := loadSectMemberListPage(myMember.SectID, page)
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, "成员列表读取失败")
		return true
	}

	markup := sectMemberListPageMarkup(actualPage, sectMemberListTotalPages(totalMembers))
	edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID, formatSectMemberListPage(sect, members, totalMembers, actualPage))
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &markup
	if _, err := bot.Send(edit); err != nil {
		log.Printf("⚠️ 宗门成员列表分页消息编辑失败: chat=%d message=%d user=%d page=%d err=%s", cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID, actualPage, textfmt.FormatTelegramSendError(err))
		telegram.AnswerCallback(bot, cb.ID, "分页刷新失败")
		return true
	}

	telegram.AnswerCallback(bot, cb.ID, fmt.Sprintf("第 %d 页", actualPage))
	return true
}

func loadSectMemberListPage(sectID int64, page int) (model.Sect, []model.SectMember, int, int, error) {
	var sect model.Sect
	if err := storage.DB.Where("id = ?", sectID).First(&sect).Error; err != nil {
		return sect, nil, 0, 1, err
	}

	if page < 1 {
		page = 1
	}
	var totalMembers int64
	if err := storage.DB.Model(&model.SectMember{}).Where("sect_id = ?", sectID).Count(&totalMembers).Error; err != nil {
		return sect, nil, 0, page, err
	}
	totalPages := sectMemberListTotalPages(int(totalMembers))
	if page > totalPages {
		page = totalPages
	}

	var members []model.SectMember
	if err := storage.DB.Where("sect_id = ?", sectID).
		Order("role DESC, contribution DESC, joined_at ASC").
		Limit(sectMemberListPageSize).
		Offset((page - 1) * sectMemberListPageSize).
		Find(&members).Error; err != nil {
		return sect, nil, int(totalMembers), page, err
	}
	return sect, members, int(totalMembers), page, nil
}

func sectMemberListTotalPages(totalMembers int) int {
	if totalMembers <= 0 {
		return 1
	}
	pages := (totalMembers + sectMemberListPageSize - 1) / sectMemberListPageSize
	if pages < 1 {
		return 1
	}
	return pages
}

func sendSectMemberListPage(bot *tgbotapi.BotAPI, chatID int64, sect model.Sect, members []model.SectMember, totalMembers int, page int) {
	maxMembersText := sectMaxMembersDisplayText(sect, "sect_member_list", 0)
	msg := tgbotapi.NewMessage(chatID, formatSectMemberListPageWithMax(sect, members, totalMembers, page, maxMembersText))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = sectMemberListPageMarkup(page, sectMemberListTotalPages(totalMembers))
	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 宗门成员列表发送失败: chat=%d sect=%d page=%d err=%s", chatID, sect.ID, page, textfmt.FormatTelegramSendError(err))
	}
}

func formatSectMemberListPage(sect model.Sect, members []model.SectMember, totalMembers int, page int) string {
	return formatSectMemberListPageWithMax(sect, members, totalMembers, page, strconv.Itoa(getSectMaxMembersWithTech(sect)))
}

func formatSectMemberListPageWithMax(sect model.Sect, members []model.SectMember, totalMembers int, page int, maxMembersText string) string {
	totalPages := sectMemberListTotalPages(totalMembers)
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	startRank := (page-1)*sectMemberListPageSize + 1

	realmNames := loadSectMemberRealmNames(members)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%s 宗门成员**\n成员：`%d/%s`\n页码：`%d/%d`\n\n", textfmt.EscapeMarkdown(sect.Name), totalMembers, maxMembersText, page, totalPages))
	for i, m := range members {
		b.WriteString(formatSectMemberListLine(startRank+i, m, realmNames[m.UserID]))
	}
	if len(members) == 0 {
		b.WriteString("暂无成员。")
	}
	return strings.TrimRight(b.String(), "\n")
}

// loadSectMemberRealmNames 一次性批量读取本页成员的修为境界，避免逐人查询造成 N+1。
// 返回 user_id -> 境界名（如【大斗师中期】）；查不到修为档案的成员不在 map 中。
func loadSectMemberRealmNames(members []model.SectMember) map[int64]string {
	realmNames := make(map[int64]string, len(members))
	if len(members) == 0 || storage.DB == nil {
		return realmNames
	}

	userIDs := make([]int64, 0, len(members))
	for _, m := range members {
		if m.UserID != 0 {
			userIDs = append(userIDs, m.UserID)
		}
	}
	if len(userIDs) == 0 {
		return realmNames
	}

	var cultivations []model.Cultivation
	if err := storage.DB.Where("user_id IN ?", userIDs).Find(&cultivations).Error; err != nil {
		log.Printf("⚠️ 宗门成员列表批量读取修为失败: count=%d err=%s", len(userIDs), textfmt.FormatPlainError(err))
		return realmNames
	}
	for i := range cultivations {
		cul := cultivations[i]
		realmNames[cul.UserID] = GetRealmName(&cul)
	}
	return realmNames
}

func sectMemberListPageMarkup(page int, totalPages int) tgbotapi.InlineKeyboardMarkup {
	if totalPages <= 1 {
		return tgbotapi.InlineKeyboardMarkup{}
	}
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	if page > 1 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("\u4e0a\u4e00\u9875", fmt.Sprintf("%s%d", sectMemberPageCallbackPrefix, page-1)))
	}
	if page < totalPages {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("\u4e0b\u4e00\u9875", fmt.Sprintf("%s%d", sectMemberPageCallbackPrefix, page+1)))
	}
	return tgbotapi.NewInlineKeyboardMarkup(row)
}

func formatSectMemberListLine(rank int, member model.SectMember, realmName string) string {
	if strings.TrimSpace(realmName) == "" {
		realmName = "【尚未踏入修炼之途】"
	}
	return fmt.Sprintf(
		"%d. `%s` [%s] %s 贡献 `%d`\n",
		rank,
		textfmt.EscapeMarkdown(member.UserName),
		getSectRoleText(member.Role),
		realmName,
		member.Contribution,
	)
}

func handleUpgradeSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var newLevel int
	var fundsCost int
	var prestigeNeed int
	var sectName string
	var sectID uint

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, false); err != nil {
			return err
		}

		if !canUpgradeSectAsset(member.Role) {
			return storage.ErrSectOnlyOwner
		}

		var sect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", member.SectID).
			First(&sect).Error; err != nil {
			return err
		}

		if sect.Level >= sectMaxLevel {
			return storage.ErrSectMaxLevel
		}

		fundsCost, prestigeNeed = getSectUpgradeRequirement(sect.Level)
		if sect.Funds < fundsCost {
			return storage.ErrSectFundsNotEnough
		}
		if sect.Prestige < prestigeNeed {
			return storage.ErrSectPrestigeNotEnough
		}

		newLevel = sect.Level + 1
		sectName = sect.Name
		sectID = sect.ID

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND funds >= ? AND prestige >= ?", sect.ID, fundsCost, prestigeNeed).
			Updates(map[string]interface{}{
				"level":    newLevel,
				"funds":    gorm.Expr("funds - ?", fundsCost),
				"prestige": gorm.Expr("prestige - ?", prestigeNeed),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectResourceNotEnough
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "道友尚未加入宗门，无法升级宗门。")
		case "ONLY_OWNER":
			telegram.ReplyText(bot, chatID, "只有宗主或长老可以升级宗门。")
		case "MAX_LEVEL":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门已达最高等级 `%d`。", sectMaxLevel))
		case "FUNDS_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门资金不足，升级需要 `%d` 灵石。", fundsCost))
		case "PRESTIGE_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门声望不足，升级需要 `%d` 声望。", prestigeNeed))
		case "RESOURCE_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, "宗门资源已变化，请稍后重试。")
		default:
			log.Printf("宗门升级失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗门升级失败，请稍后再试。")
		}
		return
	}

	var upgradedSect model.Sect
	maxMembersText := "读取失败"
	if err := storage.DB.Where("id = ?", sectID).First(&upgradedSect).Error; err != nil {
		log.Printf("宗门升级后成员上限读取失败: sect=%d user=%d err=%s", sectID, userID, textfmt.FormatPlainError(err))
	} else {
		maxMembers, err := getSectMaxMembersWithTechTxChecked(storage.DB, upgradedSect)
		if err != nil {
			log.Printf("宗门升级后成员上限科技读取失败: sect=%d user=%d err=%s", sectID, userID, textfmt.FormatPlainError(err))
		} else {
			maxMembersText = strconv.Itoa(maxMembers)
		}
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门 **%s** 升级成功。\n\n当前等级：`%d`\n消耗资金：`%d`\n成员上限提升至：`%s` 人",
		textfmt.EscapeMarkdown(sectName),
		newLevel,
		fundsCost,
		maxMembersText,
	))
}

func handleAppointSectRole(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawTargetID string, targetRole string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	targetID, err := parseSectTargetUserID(rawTargetID)
	if err != nil {
		telegram.ReplyText(bot, chatID, "目标用户 ID 格式不正确。\n示例：任命长老 123456789")
		return
	}

	if targetID == userID {
		telegram.ReplyText(bot, chatID, "不能任命自己，请指定本宗门其他成员。")
		return
	}

	var targetName string

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var operator model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &operator, false); err != nil {
			return err
		}

		if operator.Role != SectRoleOwner {
			return storage.ErrSectOnlyOwner
		}

		var target model.SectMember
		if err := loadTargetSectMemberByUserInTx(tx, targetID, operator.SectID, &target); err != nil {
			return err
		}

		if target.Role == SectRoleOwner {
			return storage.ErrSectCannotAppointOwner
		}

		targetName = target.UserName

		if target.Role == targetRole {
			return nil
		}

		res := tx.Model(&model.SectMember{}).
			Where("id = ? AND role = ?", target.ID, target.Role).
			Update("role", targetRole)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("SECT_APPOINT_ROLE_UPDATE_MISSED")
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门。")
		case "ONLY_OWNER":
			telegram.ReplyText(bot, chatID, "只有宗主可以任命职位。")
		case "TARGET_NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "目标用户不在本宗门。")
		case "CANNOT_APPOINT_OWNER":
			telegram.ReplyText(bot, chatID, "不能通过任命命令修改宗主职位。")
		default:
			log.Printf("⚠️ 宗门职位任命失败: operator=%d target=%d role=%s err=%s", userID, targetID, textfmt.FormatPlainValue(targetRole), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗门职位任命失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"已将 `%s` 任命为 `%s`。",
		textfmt.EscapeMarkdown(targetName),
		getSectRoleText(targetRole),
	))
}

func handleExitSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var sectName string

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, false); err != nil {
			return err
		}

		if member.Role == SectRoleOwner {
			return storage.ErrSectOnlyOwner
		}

		var sect model.Sect
		if err := tx.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
			return err
		}
		sectName = sect.Name

		deleteRes := tx.Unscoped().
			Where("id = ? AND user_id = ? AND role <> ?", member.ID, userID, SectRoleOwner).
			Delete(&model.SectMember{})
		if deleteRes.Error != nil {
			return deleteRes.Error
		}
		if deleteRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_MEMBER_DELETE_MISSED")
		}

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND member_count > 0", member.SectID).
			UpdateColumn("member_count", gorm.Expr("member_count - 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("SECT_MEMBER_COUNT_CHANGED")
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门。")
		case "ONLY_OWNER":
			telegram.ReplyText(bot, chatID, "宗主不能直接退出宗门，请先转让宗主或解散相关事务后再操作。")
		default:
			log.Printf("exit sect failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "退出宗门失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf("你已退出宗门 `%s`。", textfmt.EscapeMarkdown(sectName)))
}

func handleKickSectMember(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawTargetID string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	targetID, err := parseSectTargetUserID(rawTargetID)
	if err != nil {
		telegram.ReplyText(bot, chatID, "目标用户 ID 格式错误。\n示例：`踢出宗门 123456789`")
		return
	}

	if targetID == userID {
		telegram.ReplyText(bot, chatID, "不能将自己踢出宗门。")
		return
	}

	var targetName string

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var operator model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &operator, false); err != nil {
			return err
		}

		var target model.SectMember
		if err := loadTargetSectMemberByUserInTx(tx, targetID, operator.SectID, &target); err != nil {
			return err
		}

		if !canManageSectMember(operator.Role, target.Role) {
			return storage.ErrSectNoPermission
		}

		targetName = target.UserName

		deleteRes := tx.Unscoped().
			Where("id = ? AND user_id = ? AND sect_id = ? AND role = ?", target.ID, targetID, operator.SectID, target.Role).
			Where("EXISTS (SELECT 1 FROM sect_members op WHERE op.id = ? AND op.user_id = ? AND op.sect_id = ? AND op.role = ? AND op.deleted_at IS NULL)", operator.ID, userID, operator.SectID, operator.Role).
			Delete(&model.SectMember{})
		if deleteRes.Error != nil {
			return deleteRes.Error
		}
		if deleteRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_MEMBER_DELETE_MISSED")
		}

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND member_count > 0", operator.SectID).
			UpdateColumn("member_count", gorm.Expr("member_count - 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("SECT_MEMBER_COUNT_CHANGED")
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门，无法踢出成员。")
		case "TARGET_NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "目标成员不在你的宗门中。")
		case "NO_PERMISSION":
			telegram.ReplyText(bot, chatID, "只有宗主或长老可以踢出普通成员。")
		default:
			log.Printf("⚠️ 宗门踢出成员失败: operator=%d target=%d err=%s", userID, targetID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "踢出宗门成员失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf("已将 `%s` 踢出宗门。", textfmt.EscapeMarkdown(targetName)))
}

func handleTransferSectOwner(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawTargetID string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	targetID, err := parseSectTargetUserID(rawTargetID)
	if err != nil {
		telegram.ReplyText(bot, chatID, "目标用户 ID 格式错误。\n示例：`转让宗主 123456789`")
		return
	}

	if targetID == userID {
		telegram.ReplyText(bot, chatID, "不能将宗主之位转让给自己。")
		return
	}

	var sectName string
	var targetName string

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var owner model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &owner, false); err != nil {
			return err
		}

		if owner.Role != SectRoleOwner {
			return storage.ErrSectOnlyOwner
		}

		var target model.SectMember
		if err := loadTargetSectMemberByUserInTx(tx, targetID, owner.SectID, &target); err != nil {
			return err
		}

		var sect model.Sect
		if err := tx.Where("id = ?", owner.SectID).First(&sect).Error; err != nil {
			return err
		}

		sectName = sect.Name
		targetName = target.UserName

		ownerRes := tx.Model(&model.SectMember{}).
			Where("id = ? AND role = ?", owner.ID, SectRoleOwner).
			Update("role", sectRoleMember)
		if ownerRes.Error != nil {
			return ownerRes.Error
		}
		if ownerRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_TRANSFER_OWNER_ROLE_UPDATE_MISSED")
		}

		targetRes := tx.Model(&model.SectMember{}).
			Where("id = ? AND role = ?", target.ID, target.Role).
			Update("role", SectRoleOwner)
		if targetRes.Error != nil {
			return targetRes.Error
		}
		if targetRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_TRANSFER_TARGET_ROLE_UPDATE_MISSED")
		}

		sectRes := tx.Model(&model.Sect{}).
			Where("id = ?", sect.ID).
			Updates(map[string]interface{}{
				"owner_id":   target.UserID,
				"owner_name": target.UserName,
			})
		if sectRes.Error != nil {
			return sectRes.Error
		}
		if sectRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_TRANSFER_SECT_OWNER_UPDATE_MISSED")
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门，无法转让宗主。")
		case "ONLY_OWNER":
			telegram.ReplyText(bot, chatID, "只有宗主可以转让宗主之位。")
		case "TARGET_NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "目标成员不在你的宗门中。")
		default:
			log.Printf("⚠️ 宗门宗主转让失败: owner=%d target=%d err=%s", userID, targetID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗主转让失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门 **%s** 已转让给 `%s`。",
		textfmt.EscapeMarkdown(sectName),
		textfmt.EscapeMarkdown(targetName),
	))
}

func handleSectContributionRank(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, weekly bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var myMember model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &myMember, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门，无法查看宗门贡献排行。", "宗门贡献排行")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", myMember.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}

	title := "总贡献排行"
	valueName := "总贡献"
	if weekly {
		title = "本周贡献排行"
		valueName = "本周贡献"
	}

	members, err := querySectContributionRankRows(storage.DB, myMember.SectID, weekly, 20)
	if err != nil {
		telegram.ReplyText(bot, chatID, "宗门贡献排行读取失败，请稍后再试。")
		return
	}

	if len(members) == 0 {
		telegram.ReplyText(bot, chatID, "暂无宗门贡献排行数据。")
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%s %s**\n\n", textfmt.EscapeMarkdown(sect.Name), title))

	for i, member := range members {
		value := member.TotalContribution
		if weekly {
			value = member.WeeklyContribution
		}

		b.WriteString(fmt.Sprintf(
			"%d. `%s` [%s] %s `%d`\n",
			i+1,
			textfmt.EscapeMarkdown(member.UserName),
			getSectRoleText(member.Role),
			valueName,
			value,
		))
	}

	telegram.ReplyText(bot, chatID, b.String())
}

func querySectContributionRankRows(db *gorm.DB, sectID int64, weekly bool, limit int) ([]sectContributionRankRow, error) {
	if db == nil || sectID == 0 {
		return nil, fmt.Errorf("INVALID_SECT_RANK_QUERY")
	}
	if limit <= 0 {
		limit = 20
	}

	if weekly {
		var rows []sectContributionRankRow
		err := db.Table("sect_members").
			Select("user_id, user_name, role, joined_at, contribution, weekly_contribution, weekly_contribution AS total_contribution").
			Where("sect_id = ? AND deleted_at IS NULL", sectID).
			Order("weekly_contribution DESC, joined_at ASC").
			Limit(limit).
			Scan(&rows).Error
		return rows, err
	}

	var rows []sectContributionRankRow
	err := db.Table("sect_members AS sm").
		Select(`
			sm.user_id,
			sm.user_name,
			sm.role,
			sm.joined_at,
			sm.contribution,
			sm.weekly_contribution,
			`+sectTotalContributionSelectExpr()+` AS total_contribution
		`).
		Joins(`
			LEFT JOIN (
				SELECT sect_id, user_id, COALESCE(SUM(cost_contribution), 0) AS spent_contribution
				FROM sect_shop_purchases
				WHERE deleted_at IS NULL
				GROUP BY sect_id, user_id
			) AS ssp ON ssp.sect_id = sm.sect_id AND ssp.user_id = sm.user_id
		`).
		Where("sm.sect_id = ? AND sm.deleted_at IS NULL", sectID).
		Order("total_contribution DESC, sm.joined_at ASC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}

func sectTotalContributionSelectExpr() string {
	return "sm.contribution + COALESCE(ssp.spent_contribution, 0)"
}

func HandleDonateSect(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawAmount string) {
	userID := msg.From.ID

	amount, err := strconv.Atoi(strings.TrimSpace(rawAmount))
	if err != nil || amount <= 0 || amount > 100000 {
		telegram.ReplyTextToMessage(bot, msg, "捐献数量必须是 1-100000 的整数。\n示例：`捐献宗门 100`")
		return
	}

	var sectName string

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, false); err != nil {
			return err
		}

		var sect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", member.SectID).
			First(&sect).Error; err != nil {
			return err
		}
		sectName = sect.Name

		if err := storage.ApplyPointDeltaInTx(
			tx,
			userID,
			-amount,
			"sect_donate",
			fmt.Sprintf("捐献宗门 %s %d 积分", sectPointDescriptionName(sect.Name), amount),
			"sect",
			fmt.Sprintf("%d", member.SectID),
		); err != nil {
			return err
		}

		fundRes := tx.Model(&model.Sect{}).
			Where("id = ?", member.SectID).
			UpdateColumn("funds", gorm.Expr("funds + ?", amount))
		if fundRes.Error != nil {
			return fundRes.Error
		}
		if fundRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_DONATE_FUNDS_UPDATE_MISSED")
		}

		contributionGain := amount

		memberRes := tx.Model(&model.SectMember{}).
			Where("id = ?", member.ID).
			Updates(map[string]interface{}{
				"contribution":        gorm.Expr("contribution + ?", contributionGain),
				"weekly_contribution": gorm.Expr("weekly_contribution + ?", contributionGain),
			})
		if memberRes.Error != nil {
			return memberRes.Error
		}
		if memberRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_DONATE_MEMBER_CONTRIBUTION_UPDATE_MISSED")
		}

		fundsAfter, err := readSectFundsInTx(tx, member.SectID)
		if err != nil {
			return err
		}

		if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       member.SectID,
			UserID:       userID,
			Delta:        contributionGain,
			Reason:       "宗门捐献",
			RefType:      "sect_donate",
			RefID:        time.Now().Format("20060102150405"),
			BalanceAfter: fundsAfter,
		}); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotInSect):
			telegram.ReplyTextToMessage(bot, msg, "你尚未加入宗门，无法捐献。")
		case errors.Is(err, storage.ErrPointsNotEnough):
			telegram.ReplyTextToMessage(bot, msg, "积分不足，无法完成宗门捐献。")
		default:
			log.Printf("⚠️ 宗门捐献失败: user=%d amount=%d err=%s", userID, amount, textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, msg, "宗门捐献失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("已向宗门 **%s** 捐献 `%d` 积分。", textfmt.EscapeMarkdown(sectName), amount))
}

func parseSectShopRewardAmount(rawAmount string) (int, error) {
	amount, err := strconv.Atoi(strings.TrimSpace(rawAmount))
	if err != nil || amount <= 0 || amount > sectShopMaxExchangeAmount {
		return 0, fmt.Errorf("兑换数量必须在 1-%d 之间", sectShopMaxExchangeAmount)
	}
	return amount, nil
}

func handleSectShop(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门，无法查看宗门商店。", "宗门商店")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}

	now := time.Now()
	monthKey := sectShopMonthKey(now)
	sectRenewUsed := countSectShopRenewClaims(member.SectID, 0, monthKey)
	personalRenewUsed := countSectShopRenewClaims(member.SectID, userID, monthKey)
	sectRenewLimit := sectShopRenewMonthlyLimit(sect.Level)
	currentExpireText, renewAfterText := sectShopRenewPreviewText(userID, now)

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"**%s 宗门商店**\n\n"+
			"个人贡献：`%d`\n\n"+
			"贡献换声望：`%d` 贡献 = `1` 宗门声望 + `1` 个人声望，单次最多 `%d`。\n\n"+
			"宗门七日续期：消耗 `%d` 贡献，为当前 ABS 账号续期 `%d` 天。\n"+
			"本宗本月名额：`%d/%d`\n"+
			"个人本月次数：`%d/%d`\n"+
			"当前到期：`%s`\n"+
			"预计续期后：`%s`\n"+
			"限制：加入满 `%d` 天，历史贡献不少于 `%d`，续期后不超过当前时间 + `%d` 天。",
		textfmt.EscapeMarkdown(sect.Name),
		member.Contribution,
		sectContributionToPrestigeCost,
		sectShopMaxExchangeAmount,
		sectShopRenewContributionCost,
		sectShopRenewDays,
		sectRenewUsed,
		sectRenewLimit,
		personalRenewUsed,
		sectShopRenewPersonalMonthMax,
		textfmt.EscapeMarkdown(currentExpireText),
		textfmt.EscapeMarkdown(renewAfterText),
		sectShopRenewMinJoinedDays,
		sectShopRenewMinTotalContrib,
		sectShopRenewMaxExpireDays,
	))
}

func sectShopMonthKey(t time.Time) string {
	return storage.SignInMonthKey(t)
}

func sectShopRenewMonthlyLimit(level int) int {
	limits := []int{2, 3, 5, 7, 10, 13, 16, 20, 24, 30}
	if level < 1 {
		level = 1
	}
	if level > len(limits) {
		level = len(limits)
	}
	return limits[level-1]
}

func sectShopRenewBaseTime(expireAt *time.Time, now time.Time) time.Time {
	if expireAt == nil || expireAt.Before(now) {
		return now
	}
	return *expireAt
}

func sectShopRenewNextExpireAt(expireAt *time.Time, now time.Time) time.Time {
	return sectShopRenewBaseTime(expireAt, now).AddDate(0, 0, sectShopRenewDays)
}

func sectShopRenewAllowedByExpireLimit(expireAt *time.Time, now time.Time) bool {
	return !sectShopRenewNextExpireAt(expireAt, now).After(now.AddDate(0, 0, sectShopRenewMaxExpireDays))
}

func sectShopRenewRemainingDays(expireAt *time.Time, now time.Time) int {
	if expireAt == nil || !expireAt.After(now) {
		return 0
	}
	return int(math.Ceil(expireAt.Sub(now).Hours() / 24))
}

func sectShopRenewJoinedLongEnough(joinedAt time.Time, now time.Time) bool {
	return !joinedAt.IsZero() && !joinedAt.AddDate(0, 0, sectShopRenewMinJoinedDays).After(now)
}

func sectShopRenewPreviewText(userID int64, now time.Time) (string, string) {
	var u model.User
	if err := storage.DB.Select("expire_at", "is_whitelist").Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		return "读取失败", "-"
	}
	if u.IsWhitelist || u.ExpireAt == nil {
		return "无需续期", "无需续期"
	}
	return u.ExpireAt.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02"),
		sectShopRenewNextExpireAt(u.ExpireAt, now).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

func countSectShopRenewClaims(sectID int64, userID int64, monthKey string) int64 {
	var count int64
	query := storage.DB.Model(&model.SectShopRenewClaim{}).
		Where("sect_id = ? AND month_key = ?", sectID, monthKey)
	if userID != 0 {
		query = query.Where("user_id = ?", userID)
	}
	if err := query.Count(&count).Error; err != nil {
		log.Printf("宗门七日续期名额统计失败: sect=%d user=%d month=%s err=%s", sectID, userID, textfmt.FormatPlainValue(monthKey), textfmt.FormatPlainError(err))
		return 0
	}
	return count
}

func countSectShopRenewClaimsTx(tx *gorm.DB, sectID int64, userID int64, monthKey string) (int64, error) {
	var count int64
	query := tx.Model(&model.SectShopRenewClaim{}).
		Where("sect_id = ? AND month_key = ?", sectID, monthKey)
	if userID != 0 {
		query = query.Where("user_id = ?", userID)
	}
	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func reserveSectShopRenewClaimTx(tx *gorm.DB, sectID int64, userID int64, monthKey string, limit int) (model.SectShopRenewClaim, error) {
	if limit <= 0 {
		return model.SectShopRenewClaim{}, storage.ErrSectShopRenewSectLimit
	}
	for slot := 1; slot <= limit; slot++ {
		claim := model.SectShopRenewClaim{
			SectID:   sectID,
			UserID:   userID,
			MonthKey: monthKey,
			SlotNo:   slot,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
		if res.Error != nil {
			return model.SectShopRenewClaim{}, res.Error
		}
		if res.RowsAffected > 0 {
			return claim, nil
		}
		var own model.SectShopRenewClaim
		if err := tx.Where("user_id = ? AND month_key = ?", userID, monthKey).First(&own).Error; err == nil {
			return model.SectShopRenewClaim{}, storage.ErrSectShopRenewMonthlyLimit
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SectShopRenewClaim{}, err
		}
	}
	return model.SectShopRenewClaim{}, storage.ErrSectShopRenewSectLimit
}

func sectShopMemberTotalContributionTx(tx *gorm.DB, member model.SectMember) (int, error) {
	var spent int64
	if err := tx.Model(&model.SectShopPurchase{}).
		Where("sect_id = ? AND user_id = ?", member.SectID, member.UserID).
		Select("COALESCE(SUM(cost_contribution), 0)").
		Scan(&spent).Error; err != nil {
		return 0, err
	}
	return member.Contribution + int(spent), nil
}

func handleDisabledSectContributionForPoints(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	telegram.ReplyText(bot, msg.Chat.ID, "贡献换积分已关闭。当前可使用 `贡献换声望 数量`，每 3 点贡献兑换 1 点宗门声望。")
}

// exchangeSectContributionForPrestige 贡献→声望兑换核心事务：
// 消耗 rewardPrestige*sectContributionToPrestigeCost 贡献，宗门声望与个人声望各 +rewardPrestige
// （含 SectContributionLog 流水与 SectShopPurchase 记录）。
// 错误原样返回（NOT_IN_SECT / CONTRIBUTION_NOT_ENOUGH 等），调用方用 sectErrorCode 映射文案。
// 贡献换声望：在同一事务内扣贡献并累加宗门与个人声望。
func exchangeSectContributionForPrestige(userID int64, rewardPrestige int) (sectName string, contributionAfter int, personalPrestigeAfter int, err error) {
	costContribution := rewardPrestige * sectContributionToPrestigeCost

	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, true); err != nil {
			return err
		}

		var sect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", member.SectID).
			First(&sect).Error; err != nil {
			return err
		}
		sectName = sect.Name

		res := tx.Model(&model.SectMember{}).
			Where("id = ? AND contribution >= ?", member.ID, costContribution).
			UpdateColumn("contribution", gorm.Expr("contribution - ?", costContribution))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectContributionNotEnough
		}

		sectPrestigeRes := tx.Model(&model.Sect{}).
			Where("id = ?", member.SectID).
			UpdateColumn("prestige", gorm.Expr("prestige + ?", rewardPrestige))
		if sectPrestigeRes.Error != nil {
			return sectPrestigeRes.Error
		}
		if sectPrestigeRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_SHOP_PRESTIGE_SECT_UPDATE_MISSED")
		}

		memberPrestigeRes := tx.Model(&model.SectMember{}).
			Where("id = ?", member.ID).
			UpdateColumn("personal_prestige", gorm.Expr("personal_prestige + ?", rewardPrestige))
		if memberPrestigeRes.Error != nil {
			return memberPrestigeRes.Error
		}
		if memberPrestigeRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_SHOP_PRESTIGE_MEMBER_UPDATE_MISSED")
		}

		prestigeAfter, rerr := readSectPrestigeInTx(tx, member.SectID)
		if rerr != nil {
			return rerr
		}

		if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       member.SectID,
			UserID:       userID,
			Delta:        rewardPrestige,
			Reason:       fmt.Sprintf("贡献兑换宗门声望，消耗 %d 贡献，声望 +%d", costContribution, rewardPrestige),
			RefType:      "sect_shop_prestige",
			RefID:        fmt.Sprintf("%s:%d", SectDayKey(time.Now()), time.Now().UnixNano()),
			BalanceAfter: prestigeAfter,
		}); err != nil {
			return err
		}

		if err := createSectShopPurchaseInTx(tx, &model.SectShopPurchase{
			SectID:           member.SectID,
			UserID:           userID,
			ExchangeType:     "prestige",
			CostContribution: costContribution,
			RewardAmount:     rewardPrestige,
			DayKey:           SectDayKey(time.Now()),
		}); err != nil {
			return err
		}

		contributionAfter, rerr = readSectMemberContributionInTx(tx, member.ID)
		if rerr != nil {
			return rerr
		}
		if err := tx.Select("personal_prestige").Where("id = ?", member.ID).First(&member).Error; err != nil {
			return err
		}
		personalPrestigeAfter = member.PersonalPrestige
		return nil
	})
	return
}

func handleExchangeSectContributionForPrestige(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawAmount string) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	rewardPrestige, err := parseSectShopRewardAmount(rawAmount)
	if err != nil {
		telegram.ReplyText(bot, chatID, fmt.Sprintf("兑换数量格式错误。\n示例：`贡献换声望 10`\n%s", textfmt.FormatMarkdownError(err)))
		return
	}

	costContribution := rewardPrestige * sectContributionToPrestigeCost
	sectName, contributionAfter, personalPrestigeAfter, err := exchangeSectContributionForPrestige(userID, rewardPrestige)

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门，无法兑换宗门声望。")
		case "CONTRIBUTION_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("个人贡献不足，本次兑换需要 `%d` 贡献。", costContribution))
		default:
			log.Printf("⚠️ 宗门贡献兑换声望失败: user=%d reward=%d err=%s", userID, rewardPrestige, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "贡献兑换宗门声望失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"已在宗门 **%s** 兑换声望。\n消耗贡献：`%d`\n宗门声望增加：`%d`\n个人声望增加：`%d`\n剩余贡献：`%d`\n当前个人声望：`%d`",
		textfmt.EscapeMarkdown(sectName),
		costContribution,
		rewardPrestige,
		rewardPrestige,
		contributionAfter,
		personalPrestigeAfter,
	))
}

func handleExchangeSectRenew(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, confirmed bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	if msg.Chat != nil && !msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, chatID, "宗门七日续期涉及账号资产，请私聊 Bot 操作。")
		return
	}

	now := time.Now()
	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门，无法使用宗门七日续期。", "宗门七日续期")
		return
	}
	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}
	var u model.User
	if err := storage.DB.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		telegram.ReplyText(bot, chatID, "本地档案读取失败，无法续期，请稍后再试。")
		return
	}

	monthKey := sectShopMonthKey(now)
	sectUsed := countSectShopRenewClaims(member.SectID, 0, monthKey)
	personalUsed := countSectShopRenewClaims(member.SectID, userID, monthKey)
	sectLimit := sectShopRenewMonthlyLimit(sect.Level)

	if !confirmed {
		currentExpireText, renewAfterText := sectShopRenewPreviewText(userID, now)
		telegram.ReplyText(bot, chatID, fmt.Sprintf(
			"**宗门七日续期确认**\n\n"+
				"消耗贡献：`%d`\n"+
				"续期天数：`%d`\n"+
				"当前到期：`%s`\n"+
				"预计续期后：`%s`\n"+
				"本宗本月名额：`%d/%d`\n"+
				"个人本月次数：`%d/%d`\n"+
				"限制：加入满 `%d` 天，历史贡献不少于 `%d`，续期后不超过当前时间 + `%d` 天。\n\n"+
				"确认无误请发送：`确认宗门七日续期`",
			sectShopRenewContributionCost,
			sectShopRenewDays,
			textfmt.EscapeMarkdown(currentExpireText),
			textfmt.EscapeMarkdown(renewAfterText),
			sectUsed,
			sectLimit,
			personalUsed,
			sectShopRenewPersonalMonthMax,
			sectShopRenewMinJoinedDays,
			sectShopRenewMinTotalContrib,
			sectShopRenewMaxExpireDays,
		))
		return
	}

	var sectName string
	var contributionAfter int
	var newExpireAt time.Time
	var days int
	var absUserID string
	var needReactivate bool
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var lockedMember model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &lockedMember, true); err != nil {
			return err
		}

		var lockedSect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", lockedMember.SectID).
			First(&lockedSect).Error; err != nil {
			return err
		}
		sectName = lockedSect.Name

		var lockedUser model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("telegram_id = ?", userID).
			First(&lockedUser).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrUserNotFound
			}
			return err
		}
		if storage.IsTrialAccount(lockedUser) {
			return storage.ErrTrialCannotUseRenewCode
		}
		if lockedUser.IsWhitelist || lockedUser.ExpireAt == nil {
			return storage.ErrSectShopRenewPermanent
		}
		if strings.TrimSpace(lockedUser.AbsUserID) == "" {
			return storage.ErrAbsUserIDEmpty
		}
		if !sectShopRenewJoinedLongEnough(lockedMember.JoinedAt, now) {
			return storage.ErrSectShopRenewJoinedTooRecent
		}
		monthKey := sectShopMonthKey(now)
		sectUsed, err := countSectShopRenewClaimsTx(tx, lockedMember.SectID, 0, monthKey)
		if err != nil {
			return err
		}
		sectLimit := sectShopRenewMonthlyLimit(lockedSect.Level)
		if sectUsed >= int64(sectLimit) {
			return storage.ErrSectShopRenewSectLimit
		}
		personalUsed, err := countSectShopRenewClaimsTx(tx, lockedMember.SectID, userID, monthKey)
		if err != nil {
			return err
		}
		if personalUsed >= sectShopRenewPersonalMonthMax {
			return storage.ErrSectShopRenewMonthlyLimit
		}
		claim, err := reserveSectShopRenewClaimTx(tx, lockedMember.SectID, userID, monthKey, sectLimit)
		if err != nil {
			return err
		}
		totalContribution, err := sectShopMemberTotalContributionTx(tx, lockedMember)
		if err != nil {
			return err
		}
		if totalContribution < sectShopRenewMinTotalContrib {
			return storage.ErrSectShopRenewHistoryTooLow
		}
		if !sectShopRenewAllowedByExpireLimit(lockedUser.ExpireAt, now) {
			return storage.ErrSectShopRenewExpireLimit
		}

		res := tx.Model(&model.SectMember{}).
			Where("id = ? AND contribution >= ?", lockedMember.ID, sectShopRenewContributionCost).
			UpdateColumn("contribution", gorm.Expr("contribution - ?", sectShopRenewContributionCost))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectContributionNotEnough
		}

		newExpireAt = sectShopRenewNextExpireAt(lockedUser.ExpireAt, now)
		userRes := tx.Model(&model.User{}).
			Where("id = ? AND abs_user_id = ? AND is_whitelist = ? AND expire_at IS NOT NULL", lockedUser.ID, lockedUser.AbsUserID, false).
			Update("expire_at", newExpireAt)
		if userRes.Error != nil {
			return userRes.Error
		}
		if userRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_SHOP_RENEW_USER_STATE_CHANGED")
		}
		purchase := model.SectShopPurchase{
			SectID:           lockedMember.SectID,
			UserID:           userID,
			ExchangeType:     sectShopRenewExchangeType,
			CostContribution: sectShopRenewContributionCost,
			RewardAmount:     sectShopRenewDays,
			DayKey:           SectDayKey(now),
		}
		if err := createSectShopPurchaseInTx(tx, &purchase); err != nil {
			return err
		}
		claimRes := tx.Model(&model.SectShopRenewClaim{}).
			Where("id = ? AND purchase_id = ?", claim.ID, 0).
			UpdateColumn("purchase_id", purchase.ID)
		if claimRes.Error != nil {
			return claimRes.Error
		}
		if claimRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_SHOP_RENEW_CLAIM_STATE_CHANGED")
		}

		contributionAfter, err = readSectMemberContributionInTx(tx, lockedMember.ID)
		if err != nil {
			return err
		}
		if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       lockedMember.SectID,
			UserID:       userID,
			Delta:        -sectShopRenewContributionCost,
			Reason:       fmt.Sprintf("宗门七日续期，续期 %d 天", sectShopRenewDays),
			RefType:      "sect_shop_renew",
			RefID:        fmt.Sprintf("%s:%d", SectDayKey(now), now.UnixNano()),
			BalanceAfter: contributionAfter,
		}); err != nil {
			return err
		}
		if err := storage.WriteAuditLogInTx(
			tx,
			userID,
			"SECT_SHOP_RENEW",
			fmt.Sprintf("%d", userID),
			-sectShopRenewContributionCost,
			fmt.Sprintf("sect shop direct renew: sect_id=%d days=%d cost_contribution=%d expire_at=%s need_reactivate=%t", lockedMember.SectID, sectShopRenewDays, sectShopRenewContributionCost, newExpireAt.Format(time.RFC3339), lockedUser.IsSuspended && lockedUser.AbsUserID != ""),
		); err != nil {
			return err
		}

		days = sectShopRenewDays
		absUserID = lockedUser.AbsUserID
		needReactivate = lockedUser.IsSuspended && lockedUser.AbsUserID != ""
		return nil
	})

	if err != nil {
		handleSectRenewError(bot, chatID, userID, err, u.ExpireAt, now)
		return
	}

	if needReactivate {
		if err := abs.DefaultClient.SetUserActiveStatus(absUserID, true); err != nil {
			log.Printf("宗门七日续期 ABS 解封失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
			auditErr := storage.WriteAuditLogInTx(storage.DB, userID, "SECT_SHOP_RENEW_REACTIVATE_FAILED", fmt.Sprintf("%d", userID), 0,
				fmt.Sprintf("sect shop renew extended account but ABS reactivation failed: tg=%d abs_user_id=%s expire_at=%s days=%d error=%s", userID, textfmt.FormatPlainValue(absUserID), newExpireAt.Format(time.RFC3339), days, textfmt.FormatPlainError(err)))
			if auditErr != nil {
				log.Printf("宗门七日续期 ABS 解封失败审计写入失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(auditErr))
				telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 宗门七日续期已到账，但 ABS 恢复失败，且失败审计写入失败。\n用户：%d\nABS：%s\n到期：%s\n天数：%d\nABS错误：%s\n审计错误：%s", userID, textfmt.FormatPlainValue(absUserID), newExpireAt.Format(time.RFC3339), days, textfmt.FormatPlainError(err), textfmt.FormatPlainError(auditErr)))
			}
			telegram.ReplyText(bot, chatID, fmt.Sprintf(
				"宗门七日续期已到账，新的到期时间：`%s`。\n但 ABS 账号恢复失败，请联系管理员核查；本次贡献不应重复扣除。",
				newExpireAt.Format("2006-01-02"),
			))
			return
		}
		if err := storage.ApplyRenewReactivateLocalStatusWithAudit(userID, absUserID, newExpireAt, days); err != nil {
			log.Printf("宗门七日续期 ABS 已解封但本地状态同步失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
			auditErr := storage.WriteAuditLogInTx(storage.DB, userID, "SECT_SHOP_RENEW_REACTIVATE_LOCAL_FAILED", fmt.Sprintf("%d", userID), 0,
				fmt.Sprintf("sect shop renew reactivated ABS but local state/audit failed: tg=%d abs_user_id=%s expire_at=%s days=%d error=%s", userID, textfmt.FormatPlainValue(absUserID), newExpireAt.Format(time.RFC3339), days, textfmt.FormatPlainError(err)))
			if auditErr != nil {
				log.Printf("宗门七日续期 ABS 已解封，但本地失败审计写入失败: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(auditErr))
				telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 宗门七日续期已到账且 ABS 已恢复，但本地权限状态或成功审计失败，且本地失败审计写入失败。\n用户：%d\nABS：%s\n到期：%s\n天数：%d\n本地错误：%s\n审计错误：%s\n请立即人工核查。", userID, textfmt.FormatPlainValue(absUserID), newExpireAt.Format(time.RFC3339), days, textfmt.FormatPlainError(err), textfmt.FormatPlainError(auditErr)))
			}
			telegram.ReplyText(bot, chatID, fmt.Sprintf(
				"宗门七日续期已到账且 ABS 账号已恢复，新的到期时间：`%s`。\n但本地封禁状态同步或审计写入失败，请联系管理员核查。",
				newExpireAt.Format("2006-01-02"),
			))
			return
		}
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门 **%s** 七日续期成功。\n消耗贡献：`%d`\n续期天数：`%d`\n新的到期时间：`%s`\n剩余贡献：`%d`\n\n已直接写入当前绑定 ABS 账号。",
		textfmt.EscapeMarkdown(sectName),
		sectShopRenewContributionCost,
		sectShopRenewDays,
		newExpireAt.Format("2006-01-02"),
		contributionAfter,
	))
}

func handleSectRenewError(bot *tgbotapi.BotAPI, chatID int64, userID int64, err error, expireAt *time.Time, now time.Time) {
	switch SectErrorCode(err) {
	case "NOT_IN_SECT":
		telegram.ReplyText(bot, chatID, "你尚未加入宗门。")
	case "USER_NOT_FOUND":
		telegram.ReplyText(bot, chatID, "未找到你的本地档案，无法续期。")
	case "TRIAL_CANNOT_USE_RENEW_CODE":
		telegram.ReplyText(bot, chatID, "试用账号不能使用宗门七日续期。")
	case "ABS_USER_ID_EMPTY":
		telegram.ReplyText(bot, chatID, "当前档案缺少 ABS 用户 ID，无法续期。")
	case "SECT_SHOP_RENEW_PERMANENT":
		telegram.ReplyText(bot, chatID, "永久或白名单账号无需使用宗门续期。")
	case "SECT_SHOP_RENEW_JOINED_TOO_RECENT":
		telegram.ReplyText(bot, chatID, fmt.Sprintf("加入当前宗门满 `%d` 天后才能使用七日续期。", sectShopRenewMinJoinedDays))
	case "SECT_SHOP_RENEW_HISTORY_TOO_LOW":
		telegram.ReplyText(bot, chatID, fmt.Sprintf("历史累计贡献至少需要 `%d`。", sectShopRenewMinTotalContrib))
	case "SECT_SHOP_RENEW_SECT_LIMIT":
		telegram.ReplyText(bot, chatID, "本宗本月七日续期名额已用完。")
	case "SECT_SHOP_RENEW_MONTHLY_LIMIT":
		telegram.ReplyText(bot, chatID, "你本月已使用过宗门七日续期。")
	case "SECT_SHOP_RENEW_EXPIRE_LIMIT":
		remaining := sectShopRenewRemainingDays(expireAt, now)
		telegram.ReplyText(bot, chatID, fmt.Sprintf("当前剩余有效期约 `%d` 天，续期后不能超过当前时间 + `%d` 天。", remaining, sectShopRenewMaxExpireDays))
	case "CONTRIBUTION_NOT_ENOUGH":
		telegram.ReplyText(bot, chatID, fmt.Sprintf("个人贡献不足，需要 `%d` 贡献。", sectShopRenewContributionCost))
	default:
		log.Printf("sect shop renew failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, chatID, "宗门七日续期失败，请稍后再试。")
	}
}

func sectCaveSectRetreatCost(level int) int {
	if level < 1 {
		level = 1
	}
	return sectCaveSectRetreatBaseCost + level*sectCaveSectRetreatLevelCost
}

func sectCaveRetreatEndText(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")
}

func getActiveSectCaveRetreatTx(tx *gorm.DB, userID int64, now time.Time) (model.SectCaveRetreat, bool, error) {
	if tx == nil {
		tx = storage.DB
	}
	var retreat model.SectCaveRetreat
	err := tx.Where("user_id = ? AND status = ? AND start_at <= ? AND end_at > ?", userID, sectRetreatStatusActive, now, now).
		Order("end_at DESC").
		First(&retreat).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SectCaveRetreat{}, false, nil
		}
		return model.SectCaveRetreat{}, false, err
	}
	return retreat, true, nil
}

func closeExpiredSectCaveRetreatsForUser(userID int64, now time.Time) error {
	if storage.DB == nil || userID == 0 {
		return nil
	}
	res := storage.DB.Model(&model.SectCaveRetreat{}).
		Where("user_id = ? AND status = ? AND end_at <= ?", userID, sectRetreatStatusActive, now).
		UpdateColumn("status", "expired")
	return res.Error
}

func handleSectCave(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门。", "宗门洞府")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}

	now := time.Now()
	if err := closeExpiredSectCaveRetreatsForUser(userID, now); err != nil {
		log.Printf("sect cave expired retreat cleanup failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}

	unlockText := "未解锁"
	if member.CaveUnlockedAt != nil {
		unlockText = "已解锁"
	}
	retreatText := "无"
	if retreat, ok, err := getActiveSectCaveRetreatTx(storage.DB, userID, now); err != nil {
		log.Printf("sect cave active retreat read failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		retreatText = "读取失败"
	} else if ok {
		modeText := "个人闭关"
		if retreat.Mode == sectRetreatModeSect {
			modeText = "宗门闭关"
		}
		retreatText = fmt.Sprintf("%s，到 `%s` 结束", modeText, sectCaveRetreatEndText(retreat.EndAt))
	}

	sectRetreatCost := sectCaveSectRetreatCost(sect.Level)
	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"**%s 洞府**\n\n"+
			"宗门等级：`%d`\n"+
			"洞府状态：`%s`\n"+
			"个人贡献：`%d`\n"+
			"个人声望：`%d`\n"+
			"宗门声望：`%d`\n"+
			"当前闭关：`%s`\n\n"+
			"解锁洞府：宗门 4 级，消耗个人贡献 `%d`。\n"+
			"个人闭关：消耗个人声望 `%d`，持续 `%d` 小时。\n"+
			"宗门闭关：消耗宗门声望 `%d`，持续 `%d` 小时。",
		textfmt.EscapeMarkdown(sect.Name),
		sect.Level,
		unlockText,
		member.Contribution,
		member.PersonalPrestige,
		sect.Prestige,
		textfmt.EscapeMarkdown(retreatText),
		sectCaveUnlockContributionCost,
		sectCavePersonalRetreatCost,
		sectCavePersonalRetreatHours,
		sectRetreatCost,
		sectCaveSectRetreatHours,
	))
}

func handleUnlockSectCave(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, confirmed bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门。", "解锁洞府")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}
	if sect.Level < sectCaveUnlockLevel {
		telegram.ReplyText(bot, chatID, fmt.Sprintf("洞府需要宗门达到 `%d` 级后开放。", sectCaveUnlockLevel))
		return
	}
	if member.CaveUnlockedAt != nil {
		telegram.ReplyText(bot, chatID, "你的洞府已经解锁。")
		return
	}
	if !confirmed {
		telegram.ReplyText(bot, chatID, fmt.Sprintf(
			"解锁洞府将消耗 `%d` 个人贡献。\n当前贡献：`%d`\n确认无误请发送：`确认解锁洞府`",
			sectCaveUnlockContributionCost,
			member.Contribution,
		))
		return
	}

	var contributionAfter int
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var lockedMember model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &lockedMember, true); err != nil {
			return err
		}
		var lockedSect model.Sect
		if err := tx.Where("id = ?", lockedMember.SectID).First(&lockedSect).Error; err != nil {
			return err
		}
		if lockedSect.Level < sectCaveUnlockLevel {
			return storage.ErrSectCaveLocked
		}
		if lockedMember.CaveUnlockedAt != nil {
			return storage.ErrSectCaveAlreadyUnlocked
		}
		now := time.Now()
		res := tx.Model(&model.SectMember{}).
			Where("id = ? AND contribution >= ? AND cave_unlocked_at IS NULL", lockedMember.ID, sectCaveUnlockContributionCost).
			Updates(map[string]interface{}{
				"contribution":     gorm.Expr("contribution - ?", sectCaveUnlockContributionCost),
				"cave_unlocked_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectContributionNotEnough
		}
		var readErr error
		contributionAfter, readErr = readSectMemberContributionInTx(tx, lockedMember.ID)
		return readErr
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "你尚未加入宗门。")
		case "SECT_CAVE_LOCKED":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("洞府需要宗门达到 `%d` 级后开放。", sectCaveUnlockLevel))
		case "SECT_CAVE_ALREADY_UNLOCKED":
			telegram.ReplyText(bot, chatID, "你的洞府已经解锁。")
		case "CONTRIBUTION_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("个人贡献不足，解锁洞府需要 `%d` 贡献。", sectCaveUnlockContributionCost))
		default:
			log.Printf("sect cave unlock failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "解锁洞府失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf("洞府解锁成功。\n消耗贡献：`%d`\n剩余贡献：`%d`", sectCaveUnlockContributionCost, contributionAfter))
}

func getUserTodayRawListeningSeconds(userID int64, absUserID string, now time.Time) float64 {
	if strings.TrimSpace(absUserID) != "" {
		if _, ok := RefreshDailyListeningStatsFromABS(userID, absUserID); ok {
			if stat, cacheOK := getTodayDailyListeningStat(userID, now); cacheOK {
				return stat.CappedSeconds
			}
		}
	}
	if stat, ok := getTodayDailyListeningStat(userID, now); ok {
		return stat.CappedSeconds
	}
	return 0
}

func handleStartPersonalSectCaveRetreat(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, confirmed bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门。", "个人闭关")
		return
	}
	if member.CaveUnlockedAt == nil {
		telegram.ReplyText(bot, chatID, "请先发送 `解锁洞府` 开启自己的洞府。")
		return
	}
	now := time.Now()
	if err := closeExpiredSectCaveRetreatsForUser(userID, now); err != nil {
		log.Printf("sect cave expired retreat cleanup failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
	if retreat, ok, err := getActiveSectCaveRetreatTx(storage.DB, userID, now); err != nil {
		log.Printf("sect cave personal active retreat read failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, chatID, "闭关状态读取失败，请稍后重试。")
		return
	} else if ok {
		telegram.ReplyText(bot, chatID, fmt.Sprintf("你当前已有闭关，到 `%s` 结束。", sectCaveRetreatEndText(retreat.EndAt)))
		return
	}
	if !confirmed {
		telegram.ReplyText(bot, chatID, fmt.Sprintf(
			"个人闭关将消耗 `%d` 个人声望，持续 `%d` 小时。\n当前个人声望：`%d`\n确认无误请发送：`确认闭关`",
			sectCavePersonalRetreatCost,
			sectCavePersonalRetreatHours,
			member.PersonalPrestige,
		))
		return
	}

	var u model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		telegram.ReplyText(bot, chatID, "用户档案读取失败，请稍后再试。")
		return
	}
	baseRawSeconds := getUserTodayRawListeningSeconds(userID, u.AbsUserID, now)

	var endAt time.Time
	var personalPrestigeAfter int
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var lockedMember model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &lockedMember, true); err != nil {
			return err
		}
		if lockedMember.CaveUnlockedAt == nil {
			return storage.ErrSectCaveLocked
		}
		if _, ok, err := getActiveSectCaveRetreatTx(tx, userID, now); err != nil {
			return err
		} else if ok {
			return storage.ErrSectRetreatActive
		}
		res := tx.Model(&model.SectMember{}).
			Where("id = ? AND personal_prestige >= ?", lockedMember.ID, sectCavePersonalRetreatCost).
			UpdateColumn("personal_prestige", gorm.Expr("personal_prestige - ?", sectCavePersonalRetreatCost))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectPersonalPrestigeNotEnough
		}

		endAt = now.Add(time.Duration(sectCavePersonalRetreatHours) * time.Hour)
		if err := createSectCaveRetreatInTx(tx, &model.SectCaveRetreat{
			SectID:               lockedMember.SectID,
			UserID:               userID,
			Mode:                 sectRetreatModePersonal,
			StartAt:              now,
			EndAt:                endAt,
			BaseRawSeconds:       baseRawSeconds,
			PersonalPrestigeCost: sectCavePersonalRetreatCost,
			StartedByID:          userID,
			StartedByName:        textfmt.GetTelegramDisplayName(msg.From),
			Status:               sectRetreatStatusActive,
		}); err != nil {
			return err
		}
		var after model.SectMember
		if err := tx.Select("personal_prestige").Where("id = ?", lockedMember.ID).First(&after).Error; err != nil {
			return err
		}
		personalPrestigeAfter = after.PersonalPrestige
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "PERSONAL_PRESTIGE_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("个人声望不足，闭关需要 `%d` 声望。", sectCavePersonalRetreatCost))
		case "SECT_RETREAT_ACTIVE":
			telegram.ReplyText(bot, chatID, "你当前已有闭关，结束后才能再次闭关。")
		case "SECT_CAVE_LOCKED":
			telegram.ReplyText(bot, chatID, "请先解锁洞府后再闭关。")
		default:
			log.Printf("sect cave personal retreat start failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "个人闭关失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf("个人闭关已开启。\n持续时长：`%d` 小时\n结束时间：`%s`\n消耗个人声望：`%d`\n剩余个人声望：`%d`", sectCavePersonalRetreatHours, sectCaveRetreatEndText(endAt), sectCavePersonalRetreatCost, personalPrestigeAfter))
}

func handleStartSectCaveRetreat(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, confirmed bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门。", "宗门闭关")
		return
	}
	if member.Role != SectRoleOwner && member.Role != SectRoleElder {
		telegram.ReplyText(bot, chatID, "只有宗主或长老可以开启宗门闭关。")
		return
	}
	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}
	if sect.Level < sectCaveUnlockLevel {
		telegram.ReplyText(bot, chatID, fmt.Sprintf("洞府需要宗门达到 `%d` 级后开放。", sectCaveUnlockLevel))
		return
	}
	cost := sectCaveSectRetreatCost(sect.Level)
	if !confirmed {
		telegram.ReplyText(bot, chatID, fmt.Sprintf(
			"宗门闭关将消耗 `%d` 宗门声望，持续 `%d` 小时。\n当前宗门声望：`%d`\n确认无误请发送：`确认宗门闭关`",
			cost,
			sectCaveSectRetreatHours,
			sect.Prestige,
		))
		return
	}

	now := time.Now()
	type retreatTarget struct {
		Member model.SectMember
		User   model.User
		Base   float64
	}
	targets := make([]retreatTarget, 0)
	var members []model.SectMember
	if err := storage.DB.Where("sect_id = ? AND cave_unlocked_at IS NOT NULL", member.SectID).Find(&members).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门成员读取失败，请稍后再试。")
		return
	}
	for _, m := range members {
		if err := closeExpiredSectCaveRetreatsForUser(m.UserID, now); err != nil {
			log.Printf("sect cave expired retreat cleanup failed: user=%d err=%s", m.UserID, textfmt.FormatPlainError(err))
			continue
		}
		if _, active, err := getActiveSectCaveRetreatTx(storage.DB, m.UserID, now); err != nil {
			log.Printf("sect cave member active retreat read failed: user=%d err=%s", m.UserID, textfmt.FormatPlainError(err))
			continue
		} else if active {
			continue
		}
		var u model.User
		if err := storage.DB.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", m.UserID).First(&u).Error; err != nil {
			continue
		}
		targets = append(targets, retreatTarget{
			Member: m,
			User:   u,
			Base:   getUserTodayRawListeningSeconds(m.UserID, u.AbsUserID, now),
		})
	}
	if len(targets) == 0 {
		telegram.ReplyText(bot, chatID, "当前没有可进入宗门闭关的成员。")
		return
	}

	var endAt time.Time
	var sectPrestigeAfter int
	startedCount := 0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var lockedOperator model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &lockedOperator, true); err != nil {
			return err
		}
		if lockedOperator.Role != SectRoleOwner && lockedOperator.Role != SectRoleElder {
			return storage.ErrSectNoPermission
		}
		var lockedSect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", lockedOperator.SectID).
			First(&lockedSect).Error; err != nil {
			return err
		}
		if lockedSect.Level < sectCaveUnlockLevel {
			return storage.ErrSectCaveLocked
		}
		if lockedSect.Prestige < cost {
			return storage.ErrSectPrestigeNotEnough
		}

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND prestige >= ?", lockedSect.ID, cost).
			UpdateColumn("prestige", gorm.Expr("prestige - ?", cost))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectPrestigeNotEnough
		}

		endAt = now.Add(time.Duration(sectCaveSectRetreatHours) * time.Hour)
		for _, target := range targets {
			if _, active, err := getActiveSectCaveRetreatTx(tx, target.Member.UserID, now); err != nil {
				return err
			} else if active {
				continue
			}
			if err := createSectCaveRetreatInTx(tx, &model.SectCaveRetreat{
				SectID:           lockedOperator.SectID,
				UserID:           target.Member.UserID,
				Mode:             sectRetreatModeSect,
				StartAt:          now,
				EndAt:            endAt,
				BaseRawSeconds:   target.Base,
				SectPrestigeCost: cost,
				StartedByID:      userID,
				StartedByName:    textfmt.GetTelegramDisplayName(msg.From),
				Status:           sectRetreatStatusActive,
			}); err != nil {
				return err
			}
			startedCount++
		}
		if startedCount == 0 {
			return storage.ErrSectRetreatNoEligibleMembers
		}

		var readErr error
		sectPrestigeAfter, readErr = readSectPrestigeInTx(tx, lockedOperator.SectID)
		if readErr != nil {
			return readErr
		}
		if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       lockedOperator.SectID,
			UserID:       userID,
			Delta:        -cost,
			Reason:       fmt.Sprintf("宗门闭关消耗，覆盖 %d 名成员", startedCount),
			RefType:      "sect_cave_retreat",
			RefID:        fmt.Sprintf("%s:%d", SectDayKey(now), now.UnixNano()),
			BalanceAfter: sectPrestigeAfter,
		}); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NO_PERMISSION":
			telegram.ReplyText(bot, chatID, "只有宗主或长老可以开启宗门闭关。")
		case "SECT_CAVE_LOCKED":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("洞府需要宗门达到 `%d` 级后开放。", sectCaveUnlockLevel))
		case "PRESTIGE_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门声望不足，开启宗门闭关需要 `%d` 声望。", cost))
		case "SECT_RETREAT_NO_ELIGIBLE_MEMBERS":
			telegram.ReplyText(bot, chatID, "当前没有可进入宗门闭关的成员。")
		default:
			log.Printf("sect cave sect retreat start failed: sect=%d user=%d err=%s", member.SectID, userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗门闭关开启失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf("宗门闭关已开启。\n覆盖成员：`%d`\n持续时长：`%d` 小时\n结束时间：`%s`\n消耗宗门声望：`%d`\n剩余宗门声望：`%d`", startedCount, sectCaveSectRetreatHours, sectCaveRetreatEndText(endAt), cost, sectPrestigeAfter))
}

func AwardSectContribution(userID int64, delta int, reason string, refType string, refID string) error {
	if storage.DB == nil {
		return fmt.Errorf("DB_EMPTY")
	}

	if err := storage.DB.Transaction(func(tx *gorm.DB) error {
		return awardSectContributionTx(tx, userID, delta, reason, refType, refID)
	}); err != nil {
		log.Printf("宗门贡献奖励失败: user=%d delta=%d reason=%s err=%s", userID, delta, textfmt.FormatPlainValue(reason), textfmt.FormatPlainError(err))
		return err
	}

	return nil
}

func awardSectContributionTx(tx *gorm.DB, userID int64, delta int, reason string, refType string, refID string) error {
	if tx == nil || userID == 0 || delta <= 0 {
		return nil
	}

	var member model.SectMember
	if err := tx.Where("user_id = ?", userID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	var sect model.Sect
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", member.SectID).
		First(&sect).Error; err != nil {
		return err
	}

	sectUpdate := tx.Model(&model.Sect{}).
		Where("id = ?", member.SectID).
		Updates(map[string]interface{}{
			"prestige": gorm.Expr("prestige + ?", delta),
		})
	if sectUpdate.Error != nil {
		return sectUpdate.Error
	}
	if sectUpdate.RowsAffected == 0 {
		return fmt.Errorf("SECT_CONTRIBUTION_SECT_PRESTIGE_UPDATE_MISSED")
	}

	memberUpdate := tx.Model(&model.SectMember{}).
		Where("id = ?", member.ID).
		Updates(map[string]interface{}{
			"contribution":        gorm.Expr("contribution + ?", delta),
			"weekly_contribution": gorm.Expr("weekly_contribution + ?", delta),
		})
	if memberUpdate.Error != nil {
		return memberUpdate.Error
	}
	if memberUpdate.RowsAffected == 0 {
		return fmt.Errorf("SECT_MEMBER_CONTRIBUTION_UPDATE_MISSED")
	}

	var updatedMember model.SectMember
	if err := tx.Select("id", "contribution").
		Where("id = ?", member.ID).
		First(&updatedMember).Error; err != nil {
		return err
	}

	return createSectContributionLogInTx(tx, &model.SectContributionLog{
		SectID:       member.SectID,
		UserID:       userID,
		Delta:        delta,
		Reason:       sectContributionLogReason(reason),
		RefType:      refType,
		RefID:        refID,
		BalanceAfter: updatedMember.Contribution,
	})
}

func awardSectListeningContribution(userID int64, oldHours float64, newHours float64) {
	oldWholeHours := int(math.Floor(oldHours))
	newWholeHours := int(math.Floor(newHours))

	delta := newWholeHours - oldWholeHours
	if delta <= 0 {
		return
	}

	AwardSectContribution(
		userID,
		delta,
		fmt.Sprintf("听书增长奖励 %d 贡献", delta),
		"listening_hours",
		SectDayKey(time.Now()),
	)
}

type sectDailyTaskStatus struct {
	Key          string
	Name         string
	Completed    bool
	ProgressText string
}

const (
	sectTaskSignIn = "sign_in"
	sectTaskListen = "listen_1h"
	sectTaskDonate = "donate_1"

	sectDailyTaskDonateTarget = 1

	sectDailyTaskRewardContribution = 3
	sectDailyTaskRewardPrestige     = 3

	SectWeeklySignTarget       = 100
	SectWeeklyListenHourTarget = 200
	SectWeeklyTaskTarget       = 60

	sectWeeklyExcessPercentCap   = 200
	sectWeeklyExcessFundsRate    = 0.8
	sectWeeklyExcessPrestigeRate = 0.3

	sectMaxRawListeningSecondsPerDay = 24 * 3600
)

type sectWeeklyTaskStats struct {
	WeekKey        string
	WeekStart      time.Time
	SignCount      int64
	ListenHours    float64
	TaskClaimCount int64
}

type sectWeeklyTaskReward struct {
	AchievedCount      int
	ExcessPercentTotal int
	BaseFunds          int
	BasePrestige       int
	ExcessFunds        int
	ExcessPrestige     int
	Funds              int
	Prestige           int
}

type SectWeeklyTaskSettlementResult struct {
	SectID   int64
	SectName string
	Stats    sectWeeklyTaskStats
	Reward   sectWeeklyTaskReward
}

func SectDayKey(t time.Time) string {
	loc := time.FixedZone("CST", 8*3600)
	return t.In(loc).Format("2006-01-02")
}

func calculateSectEffectiveHoursFromSeconds(seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	if seconds > sectMaxRawListeningSecondsPerDay {
		seconds = sectMaxRawListeningSecondsPerDay
	}

	dailyHours := seconds / 3600.0

	if dailyHours <= 4.0 {
		return dailyHours
	}

	if dailyHours <= 8.0 {
		return 4.0 + (dailyHours-4.0)*0.30
	}

	return 5.2 + (dailyHours-8.0)*0.05
}

func calculateSectEffectiveHoursFromSecondsWithRetreat(seconds float64, retreatSeconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	if seconds > sectMaxRawListeningSecondsPerDay {
		seconds = sectMaxRawListeningSecondsPerDay
	}
	if retreatSeconds <= 0 {
		return calculateSectEffectiveHoursFromSeconds(seconds)
	}
	if retreatSeconds > seconds {
		retreatSeconds = seconds
	}

	hours := seconds / 3600.0
	retreatStartHours := (seconds - retreatSeconds) / 3600.0
	total := 0.0
	fullHours := int(math.Ceil(hours))
	for i := 0; i < fullHours; i++ {
		segmentStart := float64(i)
		segmentEnd := math.Min(float64(i+1), hours)
		if segmentEnd <= segmentStart {
			continue
		}
		segmentHours := segmentEnd - segmentStart
		normalRate := sectEffectiveHourRate(segmentStart, false)
		retreatRate := sectEffectiveHourRate(segmentStart, true)
		retreatHours := 0.0
		retreatStart := math.Max(segmentStart, retreatStartHours)
		if retreatStart < segmentEnd {
			retreatHours = segmentEnd - retreatStart
		}
		total += retreatHours*retreatRate + (segmentHours-retreatHours)*normalRate
	}

	return total
}

func sectEffectiveHourRate(hourStart float64, retreat bool) float64 {
	if hourStart < 4.0 {
		return 1.0
	}
	if hourStart < 8.0 {
		if retreat {
			return 0.60
		}
		return 0.30
	}
	if retreat {
		return 0.15
	}
	return 0.05
}

func calculateEffectiveCultivationHoursFromABSDays(days map[string]float64) float64 {
	effectiveTotalHours := 0.0
	for _, dailySeconds := range days {
		effectiveTotalHours += calculateSectEffectiveHoursFromSeconds(dailySeconds)
	}
	return effectiveTotalHours
}

func calculateSectRetreatBonusHours(rawSeconds float64, baseRawSeconds float64) float64 {
	rawSeconds = cappedListeningSeconds(rawSeconds)
	baseRawSeconds = cappedListeningSeconds(baseRawSeconds)
	if rawSeconds <= baseRawSeconds {
		return 0
	}
	retreatSeconds := rawSeconds - baseRawSeconds
	withRetreat := calculateSectEffectiveHoursFromSecondsWithRetreat(rawSeconds, retreatSeconds)
	normal := calculateSectEffectiveHoursFromSeconds(rawSeconds)
	bonus := withRetreat - normal
	if bonus < 0 {
		return 0
	}
	return bonus
}

func calculateSectRetreatBonusHoursForRecord(rawSeconds float64, baseRawSeconds float64, durationSeconds float64) float64 {
	if durationSeconds <= 0 {
		return 0
	}
	baseRawSeconds = cappedListeningSeconds(baseRawSeconds)
	maxRetreatRawSeconds := cappedListeningSeconds(baseRawSeconds + durationSeconds)
	rawForRetreat := math.Min(cappedListeningSeconds(rawSeconds), maxRetreatRawSeconds)
	return calculateSectRetreatBonusHours(rawForRetreat, baseRawSeconds)
}

func ActiveSectCaveRetreatBonusHours(userID int64, now time.Time) float64 {
	if storage.DB == nil || userID == 0 {
		return 0
	}
	if err := closeExpiredSectCaveRetreatsForUser(userID, now); err != nil {
		log.Printf("宗门洞府闭关过期关闭失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
	stat, ok := getTodayDailyListeningStat(userID, now)
	if !ok {
		return 0
	}
	startOfDay, endOfDay := sectDayRange(now)
	var retreats []model.SectCaveRetreat
	if err := storage.DB.Where("user_id = ? AND start_at >= ? AND start_at < ?", userID, startOfDay, endOfDay).
		Order("start_at ASC").
		Find(&retreats).Error; err != nil {
		log.Printf("宗门洞府闭关记录读取失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0
	}

	total := 0.0
	for _, retreat := range retreats {
		total += calculateSectRetreatBonusHoursForRecord(stat.CappedSeconds, retreat.BaseRawSeconds, retreat.EndAt.Sub(retreat.StartAt).Seconds())
	}
	return total
}

func cappedListeningSeconds(seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	if seconds > sectMaxRawListeningSecondsPerDay {
		return sectMaxRawListeningSecondsPerDay
	}
	return seconds
}

func recordDailyListeningStatsFromABSDays(userID int64, absUserID string, days map[string]float64, fetchedAt time.Time) error {
	adjustedDays, correctedDays := prepareDailyListeningDaysForRecord(userID, absUserID, days)
	return recordPreparedDailyListeningStats(userID, absUserID, adjustedDays, correctedDays, fetchedAt)
}

func prepareDailyListeningDaysForRecord(userID int64, absUserID string, days map[string]float64) (map[string]float64, map[string]bool) {
	if days == nil || abs.DefaultClient == nil || strings.TrimSpace(absUserID) == "" {
		return days, nil
	}
	sessions, err := fetchABSCrossDayListeningSessions(absUserID)
	if err != nil {
		log.Printf("每日听书 ABS 跨日会话读取失败，保留官方 days: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
		return days, nil
	}
	persistedSessions, err := persistAndLoadABSCrossDaySessions(userID, absUserID, days, sessions, time.Now())
	if err != nil {
		log.Printf("ABS cross-day session persistence failed; keeping official days: user=%d abs=%s err=%s", userID, textfmt.FormatPlainValue(absUserID), textfmt.FormatPlainError(err))
		return days, nil
	}
	return rebalanceABSDaysForCrossDaySessions(days, persistedSessions)
}

func recordPreparedDailyListeningStats(userID int64, absUserID string, days map[string]float64, correctedDays map[string]bool, fetchedAt time.Time) error {
	if storage.DB == nil || userID == 0 {
		return nil
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}

	records := buildOfficialDailyListeningRecords(userID, absUserID, days, fetchedAt)
	for index := range records {
		if correctedDays[records[index].DayKey] {
			records[index].Source = dailyListeningCrossDaySource
			records[index].FetchStatus = dailyListeningCrossDayStatus
			records[index].RefreshReason = "abs_cross_day_rebalance"
		}
	}
	if len(records) == 0 {
		return nil
	}

	res := storage.DB.Clauses(dailyListeningStatOnConflict(time.Now())).Create(&records)
	if res.Error != nil {
		err := res.Error
		log.Printf("每日听书统计写入失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return err
	}
	if res.RowsAffected == 0 {
		err := fmt.Errorf("DAILY_LISTENING_STATS_UPSERT_MISSED")
		log.Printf("每日听书统计写入未命中: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return err
	}
	return nil
}

func buildOfficialDailyListeningRecords(userID int64, absUserID string, days map[string]float64, fetchedAt time.Time) []model.DailyListeningStat {
	if userID == 0 {
		return nil
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}

	// Daily cultivation must use the exact Beijing calendar-day value reported by
	// listening-stats.days. The user listening-sessions endpoint is a historical
	// session list, not a trustworthy live-play feed; using it as a live fallback
	// can multiply one refresh interval by every historical session returned.
	rawOfficialByDay := make(map[string]float64, len(days)+1)
	daySet := make(map[string]bool, len(days)+1)
	for dayKey, rawSeconds := range days {
		dayKey = strings.TrimSpace(dayKey)
		if dayKey == "" {
			continue
		}
		rawOfficialByDay[dayKey] = abs.PositiveListeningSeconds(rawSeconds)
		daySet[dayKey] = true
	}

	// A successful payload with a present days object is authoritative for today
	// even when today's key is absent. Persisting zero here repairs any previously
	// inflated provisional/live value instead of leaving stale 24-hour data cached.
	if days != nil {
		todayKey := SectDayKey(fetchedAt)
		if _, ok := rawOfficialByDay[todayKey]; !ok {
			rawOfficialByDay[todayKey] = 0
		}
		daySet[todayKey] = true
	}

	records := make([]model.DailyListeningStat, 0, len(daySet))
	for dayKey := range daySet {
		rawSeconds := rawOfficialByDay[dayKey]
		cappedSeconds := cappedListeningSeconds(rawSeconds)
		records = append(records, model.DailyListeningStat{
			UserID:                userID,
			AbsUserID:             strings.TrimSpace(absUserID),
			DayKey:                dayKey,
			RawSeconds:            rawSeconds,
			CappedSeconds:         cappedSeconds,
			EffectiveHours:        calculateSectEffectiveHoursFromSeconds(rawSeconds),
			LastFetchedAt:         fetchedAt,
			OfficialRawSeconds:    rawSeconds,
			LiveRawSeconds:        0,
			LastOfficialFetchedAt: fetchedAt,
			Source:                "abs_days",
			FetchStatus:           "ok",
			FetchError:            "",
			RefreshReason:         "abs_refresh",
		})
	}
	return records
}

func dailyListeningStatOnConflict(now time.Time) clause.OnConflict {
	// daily_listening_stats uses a SQLite partial unique index, so the upsert
	// target must include the same deleted_at predicate.
	return clause.OnConflict{
		Columns: []clause.Column{
			{Name: "user_id"},
			{Name: "day_key"},
		},
		TargetWhere: clause.Where{Exprs: []clause.Expression{
			clause.Eq{Column: clause.Column{Name: "deleted_at"}, Value: nil},
		}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"raw_seconds":              gorm.Expr("excluded.raw_seconds"),
			"capped_seconds":           gorm.Expr("excluded.capped_seconds"),
			"effective_hours":          gorm.Expr("excluded.effective_hours"),
			"abs_user_id":              gorm.Expr("excluded.abs_user_id"),
			"last_fetched_at":          gorm.Expr("excluded.last_fetched_at"),
			"official_raw_seconds":     gorm.Expr("excluded.official_raw_seconds"),
			"live_raw_seconds":         gorm.Expr("excluded.live_raw_seconds"),
			"last_official_fetched_at": gorm.Expr("excluded.last_official_fetched_at"),
			"last_live_fetched_at":     gorm.Expr("excluded.last_live_fetched_at"),
			"source":                   gorm.Expr("excluded.source"),
			"fetch_status":             gorm.Expr("excluded.fetch_status"),
			"fetch_error":              gorm.Expr("excluded.fetch_error"),
			"refresh_reason":           gorm.Expr("excluded.refresh_reason"),
			"updated_at":               now,
			"deleted_at":               nil,
		}),
	}
}

func getTodayDailyListeningStat(userID int64, now time.Time) (model.DailyListeningStat, bool) {
	stat, ok, err := GetTodayDailyListeningStatChecked(userID, now)
	if err != nil {
		return model.DailyListeningStat{}, false
	}
	return stat, ok
}

func GetTodayDailyListeningStatChecked(userID int64, now time.Time) (model.DailyListeningStat, bool, error) {
	if storage.DB == nil {
		return model.DailyListeningStat{}, false, fmt.Errorf("DB_NOT_READY")
	}

	var stat model.DailyListeningStat
	if err := storage.DB.
		Where("user_id = ? AND day_key = ?", userID, SectDayKey(now)).
		First(&stat).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.DailyListeningStat{}, false, nil
		}
		return model.DailyListeningStat{}, false, err
	}

	return stat, true, nil
}

func GetTodaySectListeningHoursFromCache(userID int64, now time.Time) (float64, bool) {
	stat, ok := getTodayDailyListeningStat(userID, now)
	if !ok {
		return 0, false
	}
	return stat.EffectiveHours + ActiveSectCaveRetreatBonusHours(userID, now), true
}

// sumDailyListeningEffectiveHours 汇总累计净修为（跨日总和）。
// 该值桥接累计斗气时长同步与秘境基线，按落库的净修为直接汇总。
func sumDailyListeningEffectiveHours(userID int64) (float64, bool) {
	if storage.DB == nil || userID == 0 {
		return 0, false
	}

	var total float64
	if err := storage.DB.Model(&model.DailyListeningStat{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(effective_hours), 0)").
		Scan(&total).Error; err != nil {
		log.Printf("每日听书有效时长汇总失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0, false
	}
	return total, true
}

func sumDailyListeningRawSeconds(userID int64) (float64, bool) {
	if storage.DB == nil || userID == 0 {
		return 0, false
	}

	var total float64
	if err := storage.DB.Model(&model.DailyListeningStat{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(raw_seconds), 0)").
		Scan(&total).Error; err != nil {
		log.Printf("每日听书原始秒数汇总失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0, false
	}
	return total, true
}

// dailyListeningABSFetchMinInterval 限制同一用户连续两次真实 ABS 拉取的间隔。
// 宗门任务、领奖等消息路径会按需刷新今日净修为；若 5 分钟内已有当日官方数据，
// 直接复用缓存，避免每次点击都同步打 ABS（最多 1 次统计 + 5 页会话历史）。
const dailyListeningABSFetchMinInterval = 5 * time.Minute

// sectTaskViewMemberRefreshLimit 限制宗门任务整体页单次刷新的成员上限，
// 避免宗主/长老每次打开页面都同步拉取全宗门成员的 ABS 数据。
const sectTaskViewMemberRefreshLimit = 20

// cachedDailyListeningDaysWithinTTL 在当日官方 ABS 数据足够新鲜时返回缓存，
// 只含今日一条（当前所有调用方都只需要今日值），未命中返回 nil。
func cachedDailyListeningDaysWithinTTL(userID int64, now time.Time) (map[string]float64, bool) {
	if storage.DB == nil || userID == 0 {
		return nil, false
	}
	var stat model.DailyListeningStat
	if err := storage.DB.Where("user_id = ? AND day_key = ?", userID, SectDayKey(now)).First(&stat).Error; err != nil {
		return nil, false
	}
	if stat.LastOfficialFetchedAt.IsZero() || now.Sub(stat.LastOfficialFetchedAt) >= dailyListeningABSFetchMinInterval {
		return nil, false
	}
	return map[string]float64{SectDayKey(now): stat.OfficialRawSeconds}, true
}

func RefreshDailyListeningStatsFromABS(userID int64, absUserID string) (map[string]float64, bool) {
	if storage.DB == nil || abs.DefaultClient == nil || userID == 0 || strings.TrimSpace(absUserID) == "" {
		return nil, false
	}

	if days, ok := cachedDailyListeningDaysWithinTTL(userID, time.Now()); ok {
		return days, true
	}

	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(absUserID), nil)
	if err != nil || code != 200 {
		log.Printf("每日听书 ABS 统计读取失败: user=%d abs=%s code=%d err=%s", userID, textfmt.FormatPlainValue(absUserID), code, textfmt.FormatPlainError(err))
		return nil, false
	}

	var stats struct {
		Days map[string]float64 `json:"days"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		log.Printf("每日听书 ABS 统计解析失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return nil, false
	}

	fetchedAt := time.Now()
	adjustedDays, correctedDays := prepareDailyListeningDaysForRecord(userID, absUserID, stats.Days)
	if err := recordPreparedDailyListeningStats(userID, absUserID, adjustedDays, correctedDays, fetchedAt); err != nil {
		return nil, false
	}
	return adjustedDays, true
}

func refreshSectMembersDailyListeningStats(sectID int64, limit int) int {
	if storage.DB == nil || abs.DefaultClient == nil || sectID == 0 {
		return 0
	}
	if limit <= 0 {
		limit = 50
	}

	var users []model.User
	if err := storage.DB.Model(&model.User{}).
		Select("users.telegram_id", "users.abs_user_id").
		Joins("JOIN sect_members ON sect_members.user_id = users.telegram_id AND sect_members.deleted_at IS NULL").
		Where("sect_members.sect_id = ? AND users.abs_user_id <> ''", sectID).
		Limit(limit).
		Find(&users).Error; err != nil {
		log.Printf("宗门成员每日听书统计刷新名单读取失败: sect=%d err=%s", sectID, textfmt.FormatPlainError(err))
		return 0
	}

	success := 0
	for _, u := range users {
		if _, ok := RefreshDailyListeningStatsFromABS(u.TelegramID, u.AbsUserID); ok {
			if _, syncOK := syncCultivationFromDailyListeningStats(u.TelegramID); !syncOK {
				continue
			}
			success++
		}
	}
	return success
}

func getTodaySectListeningHoursFromABS(userID int64, now time.Time) (float64, bool) {
	if storage.DB == nil || abs.DefaultClient == nil {
		return 0, false
	}

	var u model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").
		Where("telegram_id = ?", userID).
		First(&u).Error; err != nil {
		return 0, false
	}

	if strings.TrimSpace(u.AbsUserID) == "" {
		return 0, false
	}

	statsDays, ok := RefreshDailyListeningStatsFromABS(userID, u.AbsUserID)
	if !ok {
		return 0, false
	}

	loc := time.FixedZone("CST", 8*3600)
	localNow := now.In(loc)
	localDayKey := localNow.Format("2006-01-02")
	todaySeconds, todayPresent := statsDays[localDayKey]
	matchedKey := ""
	if todaySeconds > 0 {
		matchedKey = localDayKey
	}

	if !todayPresent || todaySeconds <= 0 {
		if cachedHours, cacheOK := GetTodaySectListeningHoursFromCache(userID, now); cacheOK && cachedHours > 0 {
			log.Printf(
				"宗门今日净修为 ABS 当日数据缺失，使用本地缓存: user=%d local_day=%s today_present=%t available_days=%d cached_hours=%.3f",
				userID,
				localDayKey,
				todayPresent,
				len(statsDays),
				cachedHours,
			)
			return cachedHours, true
		}
	}

	effectiveHours := calculateSectEffectiveHoursFromSeconds(todaySeconds)

	log.Printf(
		"宗门今日净修为 ABS 当日数据命中: user=%d local_day=%s matched_day=%s today_present=%t available_days=%d seconds=%.0f effective_hours=%.3f",
		userID,
		localNow.Format("2006-01-02"),
		matchedKey,
		todayPresent,
		len(statsDays),
		todaySeconds,
		effectiveHours,
	)

	return effectiveHours, true
}

func sectStartOfDay(t time.Time) time.Time {
	loc := time.FixedZone("CST", 8*3600)
	now := t.In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

func sectDayRange(t time.Time) (time.Time, time.Time) {
	start := sectStartOfDay(t)
	return start, start.AddDate(0, 0, 1)
}

func SectWeekStart(t time.Time) time.Time {
	loc := time.FixedZone("CST", 8*3600)
	now := t.In(loc)

	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}

	todayZero := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return todayZero.AddDate(0, 0, -(weekday - 1))
}

func SectWeekKey(t time.Time) string {
	return SectWeekStart(t).Format("2006-01-02")
}

func querySectWeeklyTaskStatsTx(tx *gorm.DB, sectID int64, now time.Time) (sectWeeklyTaskStats, error) {
	weekStart := SectWeekStart(now)
	weekEnd := weekStart.AddDate(0, 0, 7)
	weekKey := weekStart.Format("2006-01-02")
	weekEndDayKey := weekEnd.Format("2006-01-02")

	stats := sectWeeklyTaskStats{
		WeekKey:   weekKey,
		WeekStart: weekStart,
	}
	if tx == nil || sectID == 0 {
		return stats, nil
	}

	if err := tx.Model(&model.SignInLog{}).
		Joins("JOIN sect_members ON sect_members.user_id = sign_in_logs.user_id AND sect_members.deleted_at IS NULL").
		Where("sect_members.sect_id = ? AND sign_in_logs.sign_date >= ? AND sign_in_logs.sign_date < ?", sectID, weekKey, weekEndDayKey).
		Count(&stats.SignCount).Error; err != nil {
		return sectWeeklyTaskStats{}, err
	}

	// 周目标净修为按落库的 effective_hours 汇总。
	if err := tx.Model(&model.DailyListeningStat{}).
		Joins("JOIN sect_members ON sect_members.user_id = daily_listening_stats.user_id AND sect_members.deleted_at IS NULL").
		Where("sect_members.sect_id = ? AND daily_listening_stats.day_key >= ? AND daily_listening_stats.day_key < ?", sectID, weekKey, weekEndDayKey).
		Select("COALESCE(SUM(daily_listening_stats.effective_hours), 0)").
		Scan(&stats.ListenHours).Error; err != nil {
		return sectWeeklyTaskStats{}, err
	}

	if err := tx.Model(&model.SectDailyTaskClaim{}).
		Where("sect_id = ? AND day_key >= ? AND day_key < ?", sectID, weekKey, weekEndDayKey).
		Count(&stats.TaskClaimCount).Error; err != nil {
		return sectWeeklyTaskStats{}, err
	}

	return stats, nil
}

func querySectWeeklyTaskStats(sectID int64, now time.Time) (sectWeeklyTaskStats, error) {
	return querySectWeeklyTaskStatsTx(storage.DB, sectID, now)
}

func calculateSectWeeklyTaskReward(signCount int64, listenHours float64, taskClaimCount int64) sectWeeklyTaskReward {
	achievedCount := 0
	if signCount >= SectWeeklySignTarget {
		achievedCount++
	}
	if listenHours+0.000001 >= SectWeeklyListenHourTarget {
		achievedCount++
	}
	if taskClaimCount >= SectWeeklyTaskTarget {
		achievedCount++
	}

	reward := sectWeeklyTaskReward{AchievedCount: achievedCount}
	switch achievedCount {
	case 1:
		reward.BaseFunds = 50
		reward.BasePrestige = 20
	case 2:
		reward.BaseFunds = 120
		reward.BasePrestige = 50
	case 3:
		reward.BaseFunds = 250
		reward.BasePrestige = 100
	default:
		return reward
	}

	reward.ExcessPercentTotal =
		sectWeeklyTaskExcessPercent(float64(signCount), float64(SectWeeklySignTarget)) +
			sectWeeklyTaskExcessPercent(listenHours, float64(SectWeeklyListenHourTarget)) +
			sectWeeklyTaskExcessPercent(float64(taskClaimCount), float64(SectWeeklyTaskTarget))
	reward.ExcessFunds = int(math.Floor(float64(reward.ExcessPercentTotal) * sectWeeklyExcessFundsRate))
	reward.ExcessPrestige = int(math.Floor(float64(reward.ExcessPercentTotal) * sectWeeklyExcessPrestigeRate))
	reward.Funds = reward.BaseFunds + reward.ExcessFunds
	reward.Prestige = reward.BasePrestige + reward.ExcessPrestige
	return reward
}

func sectWeeklyTaskExcessPercent(actual float64, target float64) int {
	if actual <= target || target <= 0 {
		return 0
	}
	percent := int(math.Floor((actual/target - 1.0) * 100.0))
	if percent < 0 {
		return 0
	}
	if percent > sectWeeklyExcessPercentCap {
		return sectWeeklyExcessPercentCap
	}
	return percent
}

func getSectDailyTaskStatuses(userID int64, sectID int64, now time.Time) ([]sectDailyTaskStatus, error) {
	listenHoursOverride := (*float64)(nil)

	if listenHoursFromABS, listenOK := getTodaySectListeningHoursFromABS(userID, now); listenOK {
		listenHoursOverride = &listenHoursFromABS
	} else if listenHoursFromCache, cacheOK := GetTodaySectListeningHoursFromCache(userID, now); cacheOK {
		listenHoursOverride = &listenHoursFromCache
	}

	return getSectDailyTaskStatusesTx(storage.DB, userID, sectID, now, listenHoursOverride)
}

func getSectDailyTaskStatusesTx(
	tx *gorm.DB,
	userID int64,
	sectID int64,
	now time.Time,
	listenHoursOverride *float64,
) ([]sectDailyTaskStatus, error) {
	if tx == nil {
		tx = storage.DB
	}

	dayKey := SectDayKey(now)
	startOfDay, endOfDay := sectDayRange(now)

	var signCount int64
	if err := tx.Model(&model.SignInLog{}).
		Where("user_id = ? AND sign_date = ?", userID, dayKey).
		Count(&signCount).Error; err != nil {
		return nil, err
	}

	var listenHours float64
	if listenHoursOverride != nil {
		listenHours = *listenHoursOverride
	}

	var donatePoints int
	if err := tx.Model(&model.SectContributionLog{}).
		Where("sect_id = ? AND user_id = ? AND ref_type = ? AND created_at >= ? AND created_at < ?", sectID, userID, "sect_donate", startOfDay, endOfDay).
		Select("COALESCE(SUM(delta), 0)").
		Scan(&donatePoints).Error; err != nil {
		return nil, err
	}

	signStatus := "未完成"
	if signCount > 0 {
		signStatus = "已完成"
	}

	listenCompleted := listenHours+0.000001 >= 1
	listenProgressHours := listenHours
	if listenCompleted && listenProgressHours < 1 {
		listenProgressHours = 1
	}

	listenStatus := fmt.Sprintf("%.2f/1.00", listenProgressHours)
	if listenCompleted {
		listenStatus = "已完成"
	}

	donateStatus := fmt.Sprintf("%d/%d", donatePoints, sectDailyTaskDonateTarget)
	if donatePoints >= sectDailyTaskDonateTarget {
		donateStatus = "已完成"
	}

	return []sectDailyTaskStatus{
		{
			Key:          sectTaskSignIn,
			Name:         "今日签到",
			Completed:    signCount > 0,
			ProgressText: signStatus,
		},
		{
			Key:          sectTaskListen,
			Name:         "今日净修为 +1 小时",
			Completed:    listenCompleted,
			ProgressText: listenStatus,
		},
		{
			Key:          sectTaskDonate,
			Name:         fmt.Sprintf("今日捐献 %d 积分", sectDailyTaskDonateTarget),
			Completed:    donatePoints >= sectDailyTaskDonateTarget,
			ProgressText: donateStatus,
		},
	}, nil
}

func countCompletedSectDailyTasks(tasks []sectDailyTaskStatus) int {
	count := 0
	for _, task := range tasks {
		if task.Completed {
			count++
		}
	}
	return count
}

func getCompletedSectDailyTaskNames(tasks []sectDailyTaskStatus) []string {
	names := make([]string, 0)
	for _, task := range tasks {
		if task.Completed {
			names = append(names, task.Name)
		}
	}
	return names
}

func getIncompleteSectDailyTaskSummaries(tasks []sectDailyTaskStatus) []string {
	items := make([]string, 0)
	for _, task := range tasks {
		if task.Completed {
			continue
		}
		progress := strings.TrimSpace(task.ProgressText)
		if progress == "" {
			items = append(items, task.Name)
			continue
		}
		items = append(items, fmt.Sprintf("%s\uff08%s\uff09", task.Name, progress))
	}
	return items
}

func sectDailyTaskIncompleteText(incomplete []string) string {
	if len(incomplete) == 0 {
		return "宗门每日任务尚未完成，请稍后再试。"
	}
	var b strings.Builder
	b.WriteString("宗门每日任务尚未完成：")
	for _, item := range incomplete {
		b.WriteString("\n- ")
		b.WriteString(item)
	}
	return b.String()
}

func sectClaimExistsForDay(dayKey string, userID int64) (bool, error) {
	if userID <= 0 || dayKey == "" {
		return false, fmt.Errorf("INVALID_SECT_DAILY_TASK_CLAIM_CHECK")
	}
	var claim model.SectDailyTaskClaim
	if err := storage.DB.Where("user_id = ? AND day_key = ?", userID, dayKey).First(&claim).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func sectWeeklySettlementExists(sectID int64, weekKey string) (bool, error) {
	if sectID <= 0 || weekKey == "" {
		return false, fmt.Errorf("INVALID_SECT_WEEKLY_SETTLEMENT_CHECK")
	}
	var settlement model.SectWeeklyTaskSettlement
	if err := storage.DB.Where("sect_id = ? AND week_key = ?", sectID, weekKey).First(&settlement).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func handleSectTasks(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门，无法查看宗门任务。", "宗门任务")
		return
	}

	if !canUpgradeSectAsset(member.Role) {
		telegram.ReplyText(bot, chatID, "❌ 宗门任务仅宗主与长老可用。\n若需查询个人任务完成情况，请发送：`我的宗门任务`")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}

	now := time.Now()
	dayKey := SectDayKey(now)

	if canViewSectListeningStats(member) {
		refreshSectMembersDailyListeningStats(member.SectID, sectTaskViewMemberRefreshLimit)
	}

	sectDailyTaskRewardContributionText := "读取失败"
	sectDailyTaskRewardPrestigeText := "读取失败"
	sectDailyTaskRewardContribution, sectDailyTaskRewardPrestige, rewardReadErr := getSectDailyTaskRewardsTxChecked(storage.DB, member.SectID)
	if rewardReadErr != nil {
		log.Printf("宗门任务页每日奖励读取失败: user=%d sect=%d err=%s", userID, member.SectID, textfmt.FormatPlainError(rewardReadErr))
	} else {
		sectDailyTaskRewardContributionText = strconv.Itoa(sectDailyTaskRewardContribution)
		sectDailyTaskRewardPrestigeText = strconv.Itoa(sectDailyTaskRewardPrestige)
	}

	todayClaimCountText := "读取失败"
	todayClaimCount, claimCountErr := countSectDailyTaskClaimsForDay(member.SectID, dayKey)
	if claimCountErr != nil {
		log.Printf("宗门每日任务领取统计读取失败: sect=%d day=%s err=%s", member.SectID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(claimCountErr))
	} else {
		todayClaimCountText = strconv.FormatInt(todayClaimCount, 10)
	}

	memberCountText := "读取失败"
	if sect.MemberCount < 0 {
		log.Printf("宗门成员数异常: sect=%d member_count=%d", member.SectID, sect.MemberCount)
	} else {
		memberCountText = strconv.Itoa(sect.MemberCount)
	}

	weeklyStats, statsErr := querySectWeeklyTaskStats(member.SectID, now)
	if statsErr != nil {
		log.Printf("宗门周目标统计状态暂不可用: sect=%d err=%s", member.SectID, textfmt.FormatPlainError(statsErr))
	}
	weeklyReward := sectWeeklyTaskReward{}
	if statsErr == nil {
		weeklyReward = calculateSectWeeklyTaskReward(weeklyStats.SignCount, weeklyStats.ListenHours, weeklyStats.TaskClaimCount)
	}
	weeklySettled := false
	var settlementErr error
	if statsErr == nil {
		weeklySettled, settlementErr = sectWeeklySettlementExists(member.SectID, weeklyStats.WeekKey)
	}
	if settlementErr != nil {
		log.Printf("宗门周目标结算状态读取失败: sect=%d week=%s err=%s", member.SectID, textfmt.FormatPlainValue(weeklyStats.WeekKey), textfmt.FormatPlainError(settlementErr))
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%s 宗门任务（整体）**\n\n", textfmt.EscapeMarkdown(sect.Name)))
	b.WriteString("本页仅展示宗门整体完成情况，不展示个人任务明细。\n")
	b.WriteString("道友个人任务请发送：`我的宗门任务`\n\n")

	b.WriteString(fmt.Sprintf(
		"今日个人任务领取：`%s/%s`\n每日奖励：个人贡献 +%s，本周贡献 +%s，宗门声望 +%s。\n\n",
		todayClaimCountText,
		memberCountText,
		sectDailyTaskRewardContributionText,
		sectDailyTaskRewardContributionText,
		sectDailyTaskRewardPrestigeText,
	))

	b.WriteString("周目标：\n")
	if statsErr != nil {
		b.WriteString("宗门周目标统计状态暂不可用，请稍后重试。\n")
	} else {
		b.WriteString(fmt.Sprintf("1. 签到：`%d/%d`%s\n", weeklyStats.SignCount, SectWeeklySignTarget, formatSectWeeklyTaskExcessText(float64(weeklyStats.SignCount), float64(SectWeeklySignTarget))))
		b.WriteString(fmt.Sprintf("2. 听书：`%.1f/%d` 小时%s\n", weeklyStats.ListenHours, SectWeeklyListenHourTarget, formatSectWeeklyTaskExcessText(weeklyStats.ListenHours, float64(SectWeeklyListenHourTarget))))
		b.WriteString(fmt.Sprintf("3. 个人任务：`%d/%d`%s\n\n", weeklyStats.TaskClaimCount, SectWeeklyTaskTarget, formatSectWeeklyTaskExcessText(float64(weeklyStats.TaskClaimCount), float64(SectWeeklyTaskTarget))))
		if weeklySettled {
			b.WriteString(fmt.Sprintf("周目标已结算：`%d/3`。\n", weeklyReward.AchievedCount))
		} else if settlementErr != nil {
			b.WriteString(fmt.Sprintf("本周结算状态暂不可用，当前达成：`%d/3`。\n", weeklyReward.AchievedCount))
		} else if weeklyReward.AchievedCount > 0 {
			b.WriteString(fmt.Sprintf("当前可结算：达成 `%d/3`，资金 +%d，声望 +%d，超额 %d%%。\n", weeklyReward.AchievedCount, weeklyReward.Funds, weeklyReward.Prestige, weeklyReward.ExcessPercentTotal))
			b.WriteString("可发送 `结算宗门周目标` 手动补结算。\n")
		} else {
			b.WriteString(fmt.Sprintf("当前达成：`%d/3`，暂不可结算。\n", weeklyReward.AchievedCount))
		}
	}
	b.WriteString("\n统计口径按北京时间计算。")

	telegram.ReplyText(bot, chatID, b.String())
}

func handleMySectTasks(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	var member model.SectMember
	if err := loadSectMemberByUserInTx(storage.DB, userID, &member, false); err != nil {
		replySectMemberReadError(bot, chatID, userID, err, "你尚未加入宗门，无法查看个人宗门任务。", "我的宗门任务")
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		telegram.ReplyText(bot, chatID, "宗门档案读取失败，请稍后再试。")
		return
	}

	now := time.Now()
	dayKey := SectDayKey(now)

	tasks, tasksErr := getSectDailyTaskStatuses(userID, member.SectID, now)
	if tasksErr != nil {
		log.Printf("查询宗门每日任务状态失败: user=%d sect=%d err=%s", userID, member.SectID, textfmt.FormatPlainError(tasksErr))
	}
	completedTaskCount := countCompletedSectDailyTasks(tasks)
	sectDailyTaskRewardContributionText := "读取失败"
	sectDailyTaskRewardPrestigeText := "读取失败"
	sectDailyTaskRewardContribution, sectDailyTaskRewardPrestige, rewardReadErr := getSectDailyTaskRewardsTxChecked(storage.DB, member.SectID)
	if rewardReadErr != nil {
		log.Printf("个人宗门任务页每日奖励读取失败: user=%d sect=%d err=%s", userID, member.SectID, textfmt.FormatPlainError(rewardReadErr))
	} else {
		sectDailyTaskRewardContributionText = strconv.Itoa(sectDailyTaskRewardContribution)
		sectDailyTaskRewardPrestigeText = strconv.Itoa(sectDailyTaskRewardPrestige)
	}
	claimedToday, claimErr := sectClaimExistsForDay(dayKey, userID)
	if claimErr != nil {
		log.Printf("宗门每日任务领取状态读取失败: user=%d day=%s err=%s", userID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(claimErr))
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**%s 我的宗门任务**\n\n", textfmt.EscapeMarkdown(sect.Name)))

	rewardStatus := "状态暂不可用"
	if tasksErr == nil {
		b.WriteString("个人每日任务：\n")
		for i, task := range tasks {
			b.WriteString(fmt.Sprintf("%d. %s：%s\n", i+1, task.Name, task.ProgressText))
		}
		rewardStatus = fmt.Sprintf("%d/%d", completedTaskCount, len(tasks))
		if completedTaskCount >= len(tasks) {
			rewardStatus = "可领取"
		}
		if claimedToday {
			rewardStatus = "已领取"
		} else if claimErr != nil {
			rewardStatus = "状态暂不可用"
		}
	} else {
		b.WriteString("个人任务状态暂不可用，请稍后重试。\n")
	}

	b.WriteString(fmt.Sprintf(
		"\n每日奖励：个人贡献 +%s，本周贡献 +%s，宗门声望 +%s。\n领取状态：%s\n\n",
		sectDailyTaskRewardContributionText,
		sectDailyTaskRewardContributionText,
		sectDailyTaskRewardPrestigeText,
		rewardStatus,
	))
	b.WriteString("完成全部个人任务后，可发送 `领取宗门任务奖励`。\n")
	b.WriteString("统计口径按北京时间计算。")

	telegram.ReplyText(bot, chatID, b.String())
}

func countSectDailyTaskClaimsForDay(sectID int64, dayKey string) (int64, error) {
	if storage.DB == nil || sectID == 0 || strings.TrimSpace(dayKey) == "" {
		return 0, nil
	}
	var count int64
	if err := storage.DB.Model(&model.SectDailyTaskClaim{}).
		Where("sect_id = ? AND day_key = ?", sectID, dayKey).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func formatSectWeeklyTaskExcessText(actual float64, target float64) string {
	excess := sectWeeklyTaskExcessPercent(actual, target)
	if excess <= 0 {
		return ""
	}
	return fmt.Sprintf("超额 +%d%%", excess)
}

func handleClaimSectTaskReward(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID

	now := time.Now()
	dayKey := SectDayKey(now)

	listenHoursOverride := (*float64)(nil)
	if listenHoursFromABS, listenOK := getTodaySectListeningHoursFromABS(userID, now); listenOK {
		listenHoursOverride = &listenHoursFromABS
	} else if listenHoursFromCache, cacheOK := GetTodaySectListeningHoursFromCache(userID, now); cacheOK {
		listenHoursOverride = &listenHoursFromCache
	}

	var sectName string
	var completedTaskCount int
	var completedTaskNames []string
	var incompleteTaskSummaries []string
	var sectDailyTaskRewardContribution int
	var sectDailyTaskRewardPrestige int

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, userID, &member, false); err != nil {
			return err
		}

		var sect model.Sect
		if err := tx.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
			return err
		}
		sectName = sect.Name

		var rewardErr error
		sectDailyTaskRewardContribution, sectDailyTaskRewardPrestige, rewardErr = getSectDailyTaskRewardsTxChecked(tx, member.SectID)
		if rewardErr != nil {
			return rewardErr
		}

		tasks, tasksErr := getSectDailyTaskStatusesTx(tx, userID, member.SectID, now, listenHoursOverride)
		if tasksErr != nil {
			return tasksErr
		}
		completedTaskNames = getCompletedSectDailyTaskNames(tasks)
		incompleteTaskSummaries = getIncompleteSectDailyTaskSummaries(tasks)
		completedTaskCount = len(completedTaskNames)

		if completedTaskCount < len(tasks) {
			return storage.ErrSectDailyTaskNotAllCompleted
		}

		claim := model.SectDailyTaskClaim{
			SectID:             member.SectID,
			UserID:             userID,
			DayKey:             dayKey,
			CompletedTaskCount: completedTaskCount,
			RewardContribution: sectDailyTaskRewardContribution,
			RewardPrestige:     sectDailyTaskRewardPrestige,
		}

		if err := createSectDailyTaskClaimInTx(tx, &claim); err != nil {
			if storage.IsUniqueConstraintError(err) {
				return storage.ErrSectDailyTaskAlreadyClaimed
			}
			return err
		}

		sectRewardRes := tx.Model(&model.Sect{}).
			Where("id = ?", member.SectID).
			Updates(map[string]interface{}{
				"funds":    gorm.Expr("funds + ?", sectDailyTaskRewardContribution),
				"prestige": gorm.Expr("prestige + ?", sectDailyTaskRewardPrestige),
			})
		if sectRewardRes.Error != nil {
			return sectRewardRes.Error
		}
		if sectRewardRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_DAILY_TASK_SECT_REWARD_UPDATE_MISSED")
		}

		memberRewardRes := tx.Model(&model.SectMember{}).
			Where("id = ?", member.ID).
			Updates(map[string]interface{}{
				"contribution":        gorm.Expr("contribution + ?", sectDailyTaskRewardContribution),
				"weekly_contribution": gorm.Expr("weekly_contribution + ?", sectDailyTaskRewardContribution),
			})
		if memberRewardRes.Error != nil {
			return memberRewardRes.Error
		}
		if memberRewardRes.RowsAffected == 0 {
			return fmt.Errorf("SECT_DAILY_TASK_MEMBER_REWARD_UPDATE_MISSED")
		}

		var updatedMember model.SectMember
		if err := tx.Select("id", "contribution").
			Where("id = ?", member.ID).
			First(&updatedMember).Error; err != nil {
			return err
		}

		return createSectContributionLogInTx(tx, &model.SectContributionLog{
			SectID:       member.SectID,
			UserID:       userID,
			Delta:        sectDailyTaskRewardContribution,
			Reason:       fmt.Sprintf("宗门每日任务奖励，完成 %d 项", completedTaskCount),
			RefType:      "sect_daily_task",
			RefID:        dayKey,
			BalanceAfter: updatedMember.Contribution,
		})
	})

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyTextToMessage(bot, msg, "道友尚未加入宗门，无法领取宗门任务奖励。")
		case "SECT_DAILY_TASK_NOT_ALL_COMPLETED":
			telegram.ReplyTextToMessage(bot, msg, sectDailyTaskIncompleteText(incompleteTaskSummaries))
		case "ALREADY_CLAIMED":
			telegram.ReplyTextToMessage(bot, msg, "今日宗门任务奖励已经领取过了。")
		default:
			log.Printf("宗门每日任务领奖失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, msg, "宗门任务奖励领取失败，请稍后再试。")
		}
		return
	}

	completedTaskText := strings.Join(completedTaskNames, ", ")
	if completedTaskText == "" {
		completedTaskText = "none"
	}

	telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf(
		"宗门 **%s** 今日任务奖励已领取。\n"+
			"完成任务：`%d` 项（%s）\n"+
			"个人贡献：`+%d`\n"+
			"本周贡献：`+%d`\n"+
			"宗门声望：`+%d`",
		textfmt.EscapeMarkdown(sectName),
		completedTaskCount,
		textfmt.EscapeMarkdown(completedTaskText),
		sectDailyTaskRewardContribution,
		sectDailyTaskRewardContribution,
		sectDailyTaskRewardPrestige,
	))
}

func handleSettleSectWeeklyTaskReward(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID
	result, err := settleSectWeeklyTaskReward(time.Now(), userID, textfmt.GetTelegramDisplayName(msg.From), true)

	if err != nil {
		switch SectErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "道友尚未加入宗门，无法结算宗门周目标。")
		case "NO_PERMISSION":
			telegram.ReplyText(bot, chatID, "只有宗主或长老可以结算宗门周目标。")
		case "SECT_WEEKLY_TASK_NOT_ACHIEVED":
			telegram.ReplyText(bot, chatID, "本周宗门目标尚未达成，暂不可结算。")
		case "SECT_WEEKLY_TASK_ALREADY_SETTLED":
			telegram.ReplyText(bot, chatID, "本周宗门目标已经结算过了。")
		default:
			log.Printf("宗门周目标结算失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "宗门周目标结算失败，请稍后再试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"宗门 **%s** 周目标结算完成。\n"+
			"达成目标：`%d/3`\n"+
			"超额合计：`%d%%`\n"+
			"签到：`%d/%d`\n"+
			"听书：`%.1f/%d` 小时\n"+
			"个人任务：`%d/%d`\n\n"+
			"宗门资金奖励：`%d`\n"+
			"宗门声望奖励：`%d`",
		textfmt.EscapeMarkdown(result.SectName),
		result.Reward.AchievedCount,
		result.Reward.ExcessPercentTotal,
		result.Stats.SignCount,
		SectWeeklySignTarget,
		result.Stats.ListenHours,
		SectWeeklyListenHourTarget,
		result.Stats.TaskClaimCount,
		SectWeeklyTaskTarget,
		result.Reward.Funds,
		result.Reward.Prestige,
	))
}

func settleSectWeeklyTaskReward(now time.Time, actorID int64, actorName string, requireOperatorPermission bool) (SectWeeklyTaskSettlementResult, error) {
	var result SectWeeklyTaskSettlementResult
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := loadSectMemberByUserInTx(tx, actorID, &member, false); err != nil {
			return err
		}
		if requireOperatorPermission && !canUpgradeSectAsset(member.Role) {
			return storage.ErrSectNoPermission
		}
		targetTime, err := sectWeeklyTaskManualSettlementTargetTx(tx, member.SectID, now)
		if err != nil {
			return err
		}
		var txResult SectWeeklyTaskSettlementResult
		if err := SettleSectWeeklyTaskRewardForSectTx(tx, member.SectID, targetTime, actorID, actorName, &txResult); err != nil {
			return err
		}
		result = txResult
		return nil
	})
	if err != nil {
		return SectWeeklyTaskSettlementResult{}, err
	}
	return result, nil
}

func sectWeeklyTaskManualSettlementTargetTx(tx *gorm.DB, sectID int64, now time.Time) (time.Time, error) {
	currentWeekStart := SectWeekStart(now)
	manualPreviousWeekCutoff := currentWeekStart.Add(time.Duration(storage.SectWeeklyTaskAutoSettleHour) * time.Hour)
	if now.Before(manualPreviousWeekCutoff) {
		return now, nil
	}

	previousWeek := currentWeekStart.AddDate(0, 0, -7)
	var settlement model.SectWeeklyTaskSettlement
	if err := tx.Where("sect_id = ? AND week_key = ?", sectID, previousWeek.Format("2006-01-02")).First(&settlement).Error; err == nil {
		return now, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, err
	}
	return previousWeek, nil
}

func SettleSectWeeklyTaskRewardForSectTx(tx *gorm.DB, sectID int64, now time.Time, actorID int64, actorName string, result *SectWeeklyTaskSettlementResult) error {
	var sect model.Sect
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", sectID).
		First(&sect).Error; err != nil {
		return err
	}

	stats, err := querySectWeeklyTaskStatsTx(tx, sectID, now)
	if err != nil {
		return err
	}
	reward := calculateSectWeeklyTaskReward(stats.SignCount, stats.ListenHours, stats.TaskClaimCount)
	if reward.AchievedCount <= 0 || reward.Funds <= 0 || reward.Prestige <= 0 {
		return storage.ErrSectWeeklyTaskNotAchieved
	}

	settlement := model.SectWeeklyTaskSettlement{
		SectID:             sectID,
		WeekKey:            stats.WeekKey,
		SignCount:          stats.SignCount,
		ListenHours:        stats.ListenHours,
		TaskClaimCount:     stats.TaskClaimCount,
		AchievedCount:      reward.AchievedCount,
		ExcessPercentTotal: reward.ExcessPercentTotal,
		RewardFunds:        reward.Funds,
		RewardPrestige:     reward.Prestige,
		SettledByID:        actorID,
		SettledByName:      actorName,
	}
	if err := createSectWeeklyTaskSettlementInTx(tx, &settlement); err != nil {
		return err
	}

	weeklyRewardRes := tx.Model(&model.Sect{}).
		Where("id = ?", sectID).
		Updates(map[string]interface{}{
			"funds":    gorm.Expr("funds + ?", reward.Funds),
			"prestige": gorm.Expr("prestige + ?", reward.Prestige),
		})
	if weeklyRewardRes.Error != nil {
		return weeklyRewardRes.Error
	}
	if weeklyRewardRes.RowsAffected == 0 {
		return fmt.Errorf("SECT_WEEKLY_TASK_REWARD_UPDATE_MISSED")
	}

	fundsAfter, err := readSectFundsInTx(tx, sectID)
	if err != nil {
		return err
	}
	prestigeAfter, err := readSectPrestigeInTx(tx, sectID)
	if err != nil {
		return err
	}

	if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
		SectID:       sectID,
		UserID:       actorID,
		Delta:        reward.Funds,
		Reason:       formatSectWeeklyTaskSettlementReason(stats, reward, "宗门资金"),
		RefType:      "sect_weekly_task_reward_funds",
		RefID:        stats.WeekKey,
		BalanceAfter: fundsAfter,
	}); err != nil {
		return err
	}

	if err := createSectContributionLogInTx(tx, &model.SectContributionLog{
		SectID:       sectID,
		UserID:       actorID,
		Delta:        reward.Prestige,
		Reason:       formatSectWeeklyTaskSettlementReason(stats, reward, "宗门声望"),
		RefType:      "sect_weekly_task_reward_prestige",
		RefID:        stats.WeekKey,
		BalanceAfter: prestigeAfter,
	}); err != nil {
		return err
	}

	if result != nil {
		*result = SectWeeklyTaskSettlementResult{
			SectID:   sectID,
			SectName: sect.Name,
			Stats:    stats,
			Reward:   reward,
		}
	}
	return nil
}

func formatSectWeeklyTaskSettlementReason(stats sectWeeklyTaskStats, reward sectWeeklyTaskReward, assetName string) string {
	return fmt.Sprintf(
		"宗门周目标结算：%s；周期=%s；签到=%d/%d；听书=%.1f/%d 小时；任务=%d/%d；达成=%d/3；基础奖励=+%d/+%d；超额=%d%%；超额奖励=+%d/+%d；总奖励=+%d/+%d",
		assetName,
		stats.WeekKey,
		stats.SignCount,
		SectWeeklySignTarget,
		stats.ListenHours,
		SectWeeklyListenHourTarget,
		stats.TaskClaimCount,
		SectWeeklyTaskTarget,
		reward.AchievedCount,
		reward.BaseFunds,
		reward.BasePrestige,
		reward.ExcessPercentTotal,
		reward.ExcessFunds,
		reward.ExcessPrestige,
		reward.Funds,
		reward.Prestige,
	)
}

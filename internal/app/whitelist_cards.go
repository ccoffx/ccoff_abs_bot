package app

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var (
	errWhitelistCardInvalid = errors.New("白名单卡密无效或已被使用。")
	errWhitelistCardAccount = errors.New("请先注册或绑定正式听书账号；体验账号请先使用邀请码转正。")
	errWhitelistCardAlready = errors.New("您已经是白名单，无需再次兑换，卡密未消耗。")
	errWhitelistCardAdmin   = errors.New("超级管理员不能兑换白名单卡，卡密未消耗。")
)

func generateWhitelistCards(actorID int64, count int, reason string) ([]string, error) {
	if count < 1 || count > 100 {
		return nil, errors.New("invalid whitelist card count")
	}
	reason, ok := storage.ValidateAdminReason(reason)
	if !ok {
		return nil, errors.New("invalid whitelist card reason")
	}
	var codes []string
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if config.AppConfig == nil || !config.AppConfig.AdminIDs[actorID] {
			var actor model.User
			if err := tx.Where("telegram_id = ?", actorID).First(&actor).Error; err != nil {
				return err
			}
			if actor.Role != "super_admin" {
				return errors.New("whitelist generation requires super admin")
			}
		}
		for i := 0; i < count; i++ {
			created := false
			for attempt := 0; attempt < 5; attempt++ {
				code := "WL-" + security.GenerateRandomCode(32)
				hash := security.HashSensitiveToken(code)
				if hash == "" {
					return storage.ErrSecurityPepperNotConfigured
				}
				record := model.WhitelistCode{CodeHash: hash, CodePreview: security.MaskSecret(code), CreatedByID: actorID}
				if err := tx.Create(&record).Error; err != nil {
					if storage.IsUniqueConstraintError(err) {
						continue
					}
					return err
				}
				codes = append(codes, code)
				created = true
				break
			}
			if !created {
				return errors.New("whitelist code collision limit")
			}
		}
		return storage.WriteAuditLogInTx(tx, actorID, "GENERATE_WHITELIST_CODES", "whitelist_codes", 0,
			fmt.Sprintf("generated whitelist codes count=%d; reason: %s", count, textfmt.FormatPlainValue(reason)))
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func validateWhitelistCard(tx *gorm.DB, userID int64, hash string) (*model.WhitelistCode, error) {
	var user model.User
	if err := tx.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errWhitelistCardAccount
		}
		return nil, err
	}
	if user.Role == "super_admin" || (config.AppConfig != nil && config.AppConfig.AdminIDs[userID]) {
		return nil, errWhitelistCardAdmin
	}
	if user.IsWhitelist {
		return nil, errWhitelistCardAlready
	}
	if strings.TrimSpace(user.AbsUserID) == "" || storage.IsTrialAccount(user) {
		return nil, errWhitelistCardAccount
	}
	var code model.WhitelistCode
	if hash == "" {
		return nil, storage.ErrSecurityPepperNotConfigured
	}
	if err := tx.Where("code_hash = ? AND is_used = ?", hash, false).First(&code).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errWhitelistCardInvalid
		}
		return nil, err
	}
	if code.CreatedByID == userID {
		return nil, errWhitelistCardAdmin
	}
	return &code, nil
}

func redeemWhitelistCard(userID int64, hash string) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		code, err := validateWhitelistCard(tx, userID, hash)
		if err != nil {
			return err
		}
		now := time.Now()
		res := tx.Model(&model.WhitelistCode{}).Where("id = ? AND is_used = ?", code.ID, false).
			Updates(map[string]interface{}{"is_used": true, "used_by_id": userID, "used_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errWhitelistCardInvalid
		}
		res = tx.Model(&model.User{}).Where("telegram_id = ? AND is_whitelist = ? AND role <> ?", userID, false, "super_admin").
			Updates(map[string]interface{}{"is_whitelist": true, "whitelist_expire": nil})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("whitelist account state changed")
		}
		return storage.WriteAuditLogInTx(tx, userID, "REDEEM_WHITELIST_CODE", fmt.Sprint(code.ID), 0, "redeemed permanent whitelist card")
	})
}

func whitelistCardErrorText(err error) string {
	for _, known := range []error{errWhitelistCardInvalid, errWhitelistCardAccount, errWhitelistCardAlready, errWhitelistCardAdmin} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "白名单卡操作失败，本次未生成或消耗卡密，请稍后重试。"
}

// Called only after the router's private-chat check; these states must precede
// generic “兑换”, “生成” and “白名单” keyword routing.
func handleWhitelistCardMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string, session *telegram.SessionState) bool {
	if msg == nil || msg.Chat == nil || msg.From == nil || !msg.Chat.IsPrivate() {
		return false
	}
	userID, chatID := msg.From.ID, msg.Chat.ID
	step := session.GetStep()
	reply := func(s string) { telegram.SendPlainText(bot, chatID, s) }
	failure := func(err error) {
		log.Printf("whitelist card failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		reply(whitelistCardErrorText(err))
	}
	if step == "IDLE" {
		switch text {
		case "兑换白名单", "使用白名单卡", "白名单卡":
			session.SetStep("WL_CARD_INPUT")
			reply("🏳️ 请发送白名单卡密。兑换后永久免除账号生命周期封禁和自动清理。需已有正式听书账号；卡密仅可使用一次。发送“" + textfmt.Copyable("取消") + "”退出。")
			return true
		case "生成白名单", "生成白名单卡":
			if !telegram.RequireSuperAdmin(bot, chatID, userID) {
				return true
			}
			session.SetStep("WL_CARD_COUNT")
			reply("请输入生成白名单卡的数量（1-100）。每张卡可兑换永久白名单。")
			return true
		}
		return false
	}
	if !strings.HasPrefix(step, "WL_CARD_") {
		return false
	}
	if step == "WL_CARD_COUNT" || step == "WL_CARD_REASON" || step == "WL_CARD_GENERATE" {
		if !telegram.RequireSuperAdmin(bot, chatID, userID) {
			telegram.ClearSession(userID)
			return true
		}
	}
	switch step {
	case "WL_CARD_COUNT":
		count, err := strconv.Atoi(text)
		if err != nil || count < 1 || count > 100 {
			reply("请输入有效数量，范围 1-100。")
			return true
		}
		session.SetTemp("wl_count", strconv.Itoa(count))
		session.SetStep("WL_CARD_REASON")
		reply("请输入生成原因，" + storage.AdminReasonRequirementText + "：")
	case "WL_CARD_REASON":
		reason, ok := storage.ValidateAdminReason(text)
		if !ok {
			reply(storage.AdminReasonInvalidText)
			return true
		}
		session.SetTemp("wl_reason", reason)
		session.SetStep("WL_CARD_GENERATE")
		reply(fmt.Sprintf("即将生成 %s 张永久白名单卡。\n原因：%s\n确认请回复："+textfmt.Copyable("确认生成白名单卡")+"\n取消请回复："+textfmt.Copyable("取消"), session.GetTemp("wl_count"), reason))
	case "WL_CARD_GENERATE":
		if text != "确认生成白名单卡" {
			reply("请回复“" + textfmt.Copyable("确认生成白名单卡") + "”，或发送“" + textfmt.Copyable("取消") + "”。")
			return true
		}
		count, _ := strconv.Atoi(session.GetTemp("wl_count"))
		codes, err := generateWhitelistCards(userID, count, session.GetTemp("wl_reason"))
		telegram.ClearSession(userID)
		if err != nil {
			failure(err)
			return true
		}
		// Keep each message well below Telegram's message length limit.
		for start := 0; start < len(codes); start += 40 {
			end := start + 40
			if end > len(codes) {
				end = len(codes)
			}
			reply("🏳️ 已生成永久白名单卡，请妥善保存并仅交给指定用户：\n\n" + strings.Join(codes[start:end], "\n"))
		}
	case "WL_CARD_INPUT", "WL_CARD_REDEEM":
		if config.AppConfig != nil && config.AppConfig.NoticeGroupID != 0 && !telegram.IsUserInGroupFresh(bot, userID, config.AppConfig.NoticeGroupID) {
			reply("请先加入指定群组，再兑换白名单卡。")
			telegram.ClearSession(userID)
			return true
		}
		if step == "WL_CARD_INPUT" {
			hash := security.HashSensitiveToken(text)
			if _, err := validateWhitelistCard(storage.DB, userID, hash); err != nil {
				failure(err)
				return true
			}
			session.SetTemp("wl_hash", hash)
			session.SetStep("WL_CARD_REDEEM")
			reply("确认使用此卡为当前账号开通永久白名单？卡密使用后失效。\n如账号已暂停，兑换后仍需联系管理员恢复。\n确认请回复：" + textfmt.Copyable("确认兑换白名单") + "\n取消请回复：" + textfmt.Copyable("取消"))
			return true
		}
		if text != "确认兑换白名单" {
			reply("请回复“" + textfmt.Copyable("确认兑换白名单") + "”，或发送“" + textfmt.Copyable("取消") + "”。")
			return true
		}
		err := redeemWhitelistCard(userID, session.GetTemp("wl_hash"))
		telegram.ClearSession(userID)
		if err != nil {
			failure(err)
			return true
		}
		reply("🏳️ 兑换成功！当前账号已加入永久白名单，免除账号生命周期封禁和自动清理。若账号已暂停，请联系管理员恢复。")
	}
	return true
}

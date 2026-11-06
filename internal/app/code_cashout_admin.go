package app

import (
	"errors"
	"log"

	"ccoff-abs-bot/internal/market"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const codeCashoutAdminConfirmStep = "WAITING_CONFIRM_CODE_CASHOUT_SWITCH"

func sendCodeCashoutUnavailable(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, err error) {
	if errors.Is(err, market.ErrCodeCashoutDisabled) {
		telegram.SendPlainText(bot, msg.Chat.ID, "⏸ 卡密回收已暂停，暂不支持将邀请码或续期卡回收为积分。卡密仍可正常用于注册或续期。")
		return
	}
	log.Printf("⚠️ 卡密回收开关读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
	telegram.SendPlainText(bot, msg.Chat.ID, "❌ 卡密回收状态暂时读取失败，请稍后重试。")
}

func handleCodeCashoutAdminMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string, session *telegram.SessionState) bool {
	if msg == nil || msg.From == nil || msg.Chat == nil || session == nil {
		return false
	}
	pending := session.GetStep() == codeCashoutAdminConfirmStep
	isCommand := text == "卡密回收状态" || text == "关闭卡密回收" || text == "开启卡密回收"
	if !pending && !isCommand {
		return false
	}
	if !storage.IsSuperAdmin(msg.From.ID) {
		telegram.SendPlainText(bot, msg.Chat.ID, "❌ 卡密回收管理仅限超级管理员。")
		telegram.ClearSession(msg.From.ID)
		return true
	}
	if !msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "❌ 卡密回收管理请在私聊中执行。")
		telegram.ClearSession(msg.From.ID)
		return true
	}

	if isCommand {
		telegram.ClearSession(msg.From.ID)
		if text == "卡密回收状态" {
			err := market.CheckCodeCashoutEnabled(storage.DB)
			switch {
			case err == nil:
				telegram.SendPlainText(bot, msg.Chat.ID, "♻️ 卡密回收状态：已开启。")
			case errors.Is(err, market.ErrCodeCashoutDisabled):
				telegram.SendPlainText(bot, msg.Chat.ID, "⏸ 卡密回收状态：已关闭。")
			default:
				sendCodeCashoutUnavailable(bot, msg, err)
			}
			return true
		}
		session = telegram.GetSession(msg.From.ID)
		session.SetTemp("code_cashout_switch_command", text)
		session.SetStep(codeCashoutAdminConfirmStep)
		telegram.UserSessions.Store(msg.From.ID, session)
		telegram.SendPlainText(bot, msg.Chat.ID, "请确认"+text+"。该设置对所有邀请码和续期卡的回收生效，保存后立即生效并在重启后保留；正常注册、续期和积分兑换卡密不受影响。\n\n回复："+textfmt.Copyable("确认"+text)+"\n取消请发送："+textfmt.Copyable("取消"))
		return true
	}

	command := session.GetTemp("code_cashout_switch_command")
	if command != "关闭卡密回收" && command != "开启卡密回收" {
		telegram.ClearSession(msg.From.ID)
		telegram.SendPlainText(bot, msg.Chat.ID, "❌ 卡密回收设置会话已失效，请重新发起。")
		return true
	}
	if text != "确认"+command {
		telegram.SendPlainText(bot, msg.Chat.ID, "请回复："+textfmt.Copyable("确认"+command)+"，或发送“取消”退出。")
		return true
	}
	value, state := 0, "关闭"
	if command == "开启卡密回收" {
		value, state = 1, "开启"
	}
	_, err := setConfigIntWithAudit(msg.From.ID, market.CodeCashoutEnabledKey, value, 1, "SET_CODE_CASHOUT_ENABLED", "卡密回收开关", storage.AutomaticAdminReason)
	telegram.ClearSession(msg.From.ID)
	if err != nil {
		log.Printf("⚠️ 卡密回收开关更新失败: actor=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, msg.Chat.ID, "❌ 卡密回收设置保存失败，本次未修改，请稍后重试。")
		return true
	}
	telegram.SendPlainText(bot, msg.Chat.ID, "✅ 卡密回收已"+state+"，立即生效并已保存。")
	return true
}

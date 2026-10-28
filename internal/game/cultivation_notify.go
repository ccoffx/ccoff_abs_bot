package game

import (
	"fmt"
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

// ==========================================
// 📿 小阶段自动突破检测与双端通知引擎
// ==========================================
func FetchReportAndCheckUpgrade(bot *tgbotapi.BotAPI, userID int64, absUserID string) string {
	if absUserID == "" {
		return ""
	}

	oldCul := GetOrCreateCultivation(userID)
	oldRealm := GetRealmName(oldCul)

	reportStr := GetPersonalReport(abs.DefaultClient, absUserID)

	newCul := GetOrCreateCultivation(userID)
	if oldCul == nil || newCul == nil {
		log.Printf("⚠️ 境界变化检查斗气档案读取失败: user=%d old_nil=%t new_nil=%t", userID, oldCul == nil, newCul == nil)
		return reportStr
	}
	newRealm := GetRealmName(newCul)

	if oldRealm != newRealm {
		var u model.User
		if err := storage.DB.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
			log.Printf("⚠️ 境界变化公告读取本地档案失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		}

		displayName := textfmt.EscapeMarkdown(u.Username)
		if displayName == "" {
			displayName = "神秘道友"
		}

		if chat, err := bot.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: userID}}); err == nil {
			if chat.UserName != "" {
				displayName = textfmt.TelegramUsernameMentionMarkdown(chat.UserName)
			}
		}

		announce := fmt.Sprintf("🎉 **【斗气精进·修为突破】** 🎉\n\n恭喜道友 %s 闭关苦修，厚积薄发！\n\n✨ 成功从 **【%s】** 突破至全新的 **【%s】** 境界！\n\n*(大道漫漫，望道友继续潜心听书，早日登临绝顶！)*", displayName, oldRealm, newRealm)

		if _, err := telegram.SendAutoDelete(bot, tgbotapi.NewMessage(userID, announce)); err != nil {
			log.Printf("发送修为突破私聊通知失败: user=%d err=%s", userID, textfmt.FormatTelegramSendError(err))
		}

		if config.AppConfig.NoticeGroupID != 0 {
			// 如果是大境界跨越，只发突破通报（奖池逻辑已在雷劫里处理）
			if oldCul.MajorRealm != newCul.MajorRealm {
				telegram.SendGroupAutoDeleteMessage(bot, config.AppConfig.NoticeGroupID, announce)
			} else {
				// 🚨 小阶段福利：调用全局安全引擎注入奖池
				smallPts := security.RandomIntRange(5, 10)

				currentPool, isBurst := storage.AddPointsToFusionPool(smallPts)

				progressText := ""
				if !isBurst {
					progressText = fmt.Sprintf("\n\n*(💧 此番精进引动天地共鸣，为天道奖池注入了 `%d` 积分，当前进度 `%d/300`)*", smallPts, currentPool)
					telegram.SendGroupAutoDeleteMessage(bot, config.AppConfig.NoticeGroupID, announce+progressText)
				} else {
					progressText = fmt.Sprintf("\n\n*(💧 此番精进成为了压轴造化，为天道奖池注入了最后 `%d` 积分！)*", smallPts)
					telegram.SendGroupAutoDeleteMessage(bot, config.AppConfig.NoticeGroupID, announce+progressText)

					telegram.NotifyFusionPoolBurst(bot, config.AppConfig.NoticeGroupID, "众道友接连突破引动天地异象")
				}
			}
		}
	}

	return reportStr
}

func HandleBreakthroughConfirmation(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, session *telegram.SessionState, text string) {
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return
	}
	if session == nil {
		session = telegram.GetSession(msg.From.ID)
	}

	if !msg.Chat.IsPrivate() {
		telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
	}

	mode := session.GetTemp("bt_mode")
	if (mode == "USE_INVENTORY" && text == "确认渡劫") || (mode == "AUTO_BUY" && text == "确认代购并渡劫") {
		// 将控制权移交给底层渡劫处决引擎
		BeginBreakthroughWithGuards(bot, msg, mode)
	} else {
		telegram.ReplyTextToMessage(bot, msg, "🛑 您已压制体内翻涌的气血，取消了本次渡劫。")
	}
	telegram.ClearSession(msg.From.ID)
}

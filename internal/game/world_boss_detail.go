package game

import (
	"errors"
	"fmt"
	"log"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

// Personal details use the saved progress for any participant, including those
// outside the public top ten. Replies identify the player in shared chats.
func handleWorldBossDetail(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	event, err := getActiveOrLatestWorldBossChecked()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		telegram.ReplyTextToMessage(bot, msg, "📜 暂无世界Boss记录。")
		return
	}
	if err != nil {
		log.Printf("世界Boss明细活动读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyTextToMessage(bot, msg, "📜 世界Boss明细暂时读取失败，请稍后重试。")
		return
	}

	var participant model.WorldBossParticipant
	// Pin the event status and personal progress to the same database snapshot.
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("boss_id = ?", event.BossID).First(&event).Error; err != nil {
			return err
		}
		return tx.Where("boss_id = ? AND user_id = ?", event.BossID, msg.From.ID).First(&participant).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if status, found, statusErr := activityEntryStatus(activityEntryBoss, event.BossID, msg.From.ID); statusErr == nil && found {
			telegram.ReplyTextToMessage(bot, msg, status)
			return
		}
		telegram.ReplyTextToMessage(bot, msg, "📜 未找到你在本期世界Boss中的参与记录。发送 `参加Boss` 可在活动开放期间参加。")
		return
	}
	if err != nil {
		log.Printf("世界Boss明细读取失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyTextToMessage(bot, msg, "📜 世界Boss明细暂时读取失败，请稍后重试。")
		return
	}

	name := participant.UserName
	if name == "" {
		name = textfmt.GetTelegramDisplayName(msg.From)
	}
	baseline := "旧记录未保存日基线"
	if participant.ListeningSnapshot.BaselineDay != "" {
		baseline = fmt.Sprintf("%s，%.2f 小时", participant.ListeningSnapshot.BaselineDay, participant.ListeningSnapshot.BaselineSeconds/3600)
	}
	progressLabel := "当前已保存"
	if event.Status == "settled" {
		progressLabel = "最终结算"
	}
	multiplier := "待计算"
	if participant.Multiplier > 0 {
		multiplier = fmt.Sprintf("%.2f", participant.Multiplier)
	}
	rewards := "奖励以最终结算为准。"
	if event.Status == "settled" {
		rewards = fmt.Sprintf("结算奖励：积分 +`%d`，贡献 +`%d`，声望 +`%d`", participant.RewardPoints, participant.RewardContribution, participant.RewardPrestige)
	}
	telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf(
		"📜 **世界Boss明细 · %s**\n\n道友：`%s`\n入场时间：`%s`（北京时间）\n入场当日有效听书基线：%s\n\n%s有效听书：`%.2f` 分钟\n%s伤害：`%.2f`\n基础伤害：`%.2f`\n伤害倍率：`%s`\n\n%s%s",
		textfmt.EscapeMarkdown(event.Name), textfmt.EscapeMarkdown(name),
		participant.CreatedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"), baseline,
		progressLabel, participant.DeltaHours*60, progressLabel, preciseWorldBossDamage(participant),
		participant.BaseDamage, multiplier, rewards, activityListeningSnapshotNotice(participant.ListeningSnapshot)+worldBossLiveRefreshNotice(event)))
}

package telegram

import (
	"fmt"
	"log"
	"strings"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func HandleRedPacketNoticeRecoveryCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || (fields[0] != "红包公告过渡" && fields[0] != "解除红包公告过渡") {
		return false
	}
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return true
	}
	if !msg.Chat.IsPrivate() {
		SendPlainText(bot, msg.Chat.ID, "红包公告过渡处置仅允许超级管理员私聊执行。")
		return true
	}
	if !RequireSuperAdmin(bot, msg.Chat.ID, msg.From.ID) {
		return true
	}
	if fields[0] == "红包公告过渡" && len(fields) == 2 {
		var packet model.RedPacket
		if err := storage.DB.First(&packet, "id = ?", fields[1]).Error; err != nil {
			SendPlainText(bot, msg.Chat.ID, "红包读取失败，请检查编号后重试。")
			return true
		}
		var records []model.AutoDeleteMsg
		err := storage.DB.Where("chat_id = ? AND id <= ? AND COALESCE(packet_id,'') = ''", packet.NoticeChatID, packet.NoticeLegacyCleanupMaxID).Order("id").Limit(20).Find(&records).Error
		if err != nil {
			SendPlainText(bot, msg.Chat.ID, "过渡记录读取失败，请稍后重试。")
			return true
		}
		text := fmt.Sprintf("红包：%s\n目标群：%d\n发送状态：%s\n迁移过渡记录上限：%d\n以下为最多20条待核查消息：", packet.ID, packet.NoticeChatID, packet.NoticeSendState, packet.NoticeLegacyCleanupMaxID)
		for _, record := range records {
			text += fmt.Sprintf("\n消息 %d（清理记录 %d）", record.MessageID, record.ID)
		}
		text += "\n确认旧红包公告不存在、阻塞项只是普通消息后，可发送：\n解除红包公告过渡 " + packet.ID + " 已核查旧公告不存在"
		SendPlainText(bot, msg.Chat.ID, text)
		return true
	}
	if fields[0] == "解除红包公告过渡" && len(fields) == 3 && fields[2] == "已核查旧公告不存在" {
		if err := storage.ReleaseLegacyRedPacketNoticeBarrier(storage.DB, fields[1], msg.From.ID); err != nil {
			log.Printf("解除红包公告过渡失败: %s", textfmt.FormatPlainError(err))
			SendPlainText(bot, msg.Chat.ID, "解除失败：红包已结束、当前状态不允许或数据库暂不可用。")
			return true
		}
		SendPlainText(bot, msg.Chat.ID, "已解除该红包的历史消息过渡限制，后续巡检按发送状态和限流时间补发；原清理记录保留。")
		return true
	}
	SendPlainText(bot, msg.Chat.ID, "查看：红包公告过渡 <红包编号>\n核查旧公告不存在后解除：解除红包公告过渡 <红包编号> 已核查旧公告不存在")
	return true
}

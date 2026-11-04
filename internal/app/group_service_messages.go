package app

import (
	"fmt"
	"log"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Service messages may be emitted by bots or anonymous administrators. Handle
// them before the person-only business and membership gates, without treating
// them as commands or changing the separate chat_member verification events.
func handleGroupServiceMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) bool {
	job, ok := groupServiceDeleteJob(bot, msg)
	if !ok {
		return false
	}
	if !telegram.EnqueueTelegramAsync(job) {
		log.Printf("群系统提示删除未入队: chat=%d message=%d", msg.Chat.ID, msg.MessageID)
	}
	return true
}

func groupServiceDeleteJob(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) (telegram.TelegramAsyncJob, bool) {
	if msg == nil || msg.Chat == nil || msg.MessageID <= 0 ||
		(msg.Chat.Type != "group" && msg.Chat.Type != "supergroup") ||
		config.AppConfig == nil || config.AppConfig.NoticeGroupID == 0 ||
		msg.Chat.ID != config.AppConfig.NoticeGroupID ||
		(len(msg.NewChatMembers) == 0 && msg.LeftChatMember == nil && msg.PinnedMessage == nil) {
		return telegram.TelegramAsyncJob{}, false
	}
	chatID, messageID := msg.Chat.ID, msg.MessageID
	return telegram.TelegramAsyncJob{
		Kind:      "group_service_delete",
		DedupeKey: fmt.Sprintf("group_service_delete:%d:%d", chatID, messageID),
		Priority:  telegram.TelegramAsyncPriorityNormal,
		Send: func() error {
			// Delete the outer service message, never PinnedMessage.MessageID.
			_, err := bot.Request(tgbotapi.NewDeleteMessage(chatID, messageID))
			if err != nil {
				return fmt.Errorf("chat=%d message=%d: %w", chatID, messageID, err)
			}
			return nil
		},
	}, true
}

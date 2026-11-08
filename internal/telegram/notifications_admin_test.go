package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
)

func TestAdminNoticeReceiptsReflectPartialQueueAdmission(t *testing.T) {
	previousConfig, previousDispatcher := config.AppConfig, telegramAsyncDispatcher
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true, 2: true}}
	telegramAsyncDispatcher = testTelegramDispatcher()
	t.Cleanup(func() {
		config.AppConfig, telegramAsyncDispatcher = previousConfig, previousDispatcher
	})
	bot := &tgbotapi.BotAPI{}
	if NotifySuperAdminPlain(bot, 3, "notice") || telegramAsyncDispatcher.length() != 0 {
		t.Fatal("unconfigured recipient was queued")
	}
	// One queue slot admits the first admin and rejects the second. A receipt
	// for the rejected admin must become available after capacity is restored.
	if !NotifySuperAdminPlain(bot, 1, "notice") || NotifySuperAdminPlain(bot, 2, "notice") {
		t.Fatal("per-recipient receipts do not reflect partial queue admission")
	}
	<-telegramAsyncDispatcher.high
	if !NotifySuperAdminPlain(bot, 2, "notice") {
		t.Fatal("rejected recipient could not be retried")
	}
	<-telegramAsyncDispatcher.high
	if NotifySuperAdminsPlain(bot, "notice") || telegramAsyncDispatcher.length() != 1 {
		t.Fatal("broadcast must report partial queue admission as unsuccessful")
	}
}

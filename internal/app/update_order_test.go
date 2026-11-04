package app

import (
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestUserUpdateOrderSurvivesCancelledTurns(t *testing.T) {
	first := reserveUserUpdate(88101)
	cancelled := reserveUserUpdate(88101)
	last := reserveUserUpdate(88101)
	other := reserveUserUpdate(88102)
	defer first.finish()
	defer cancelled.finish()
	defer last.finish()
	defer other.finish()
	if !first.isReady() || cancelled.isReady() || last.isReady() || !other.isReady() {
		t.Fatal("same-user turns overlapped or blocked an unrelated user")
	}
	cancelled.finish()
	if last.isReady() {
		t.Fatal("cancelling a queued update let a later update overtake the first")
	}
	first.finish()
	if !last.isReady() {
		t.Fatal("cancelled update blocked the remaining queue")
	}
	last.finish()
	updateOrder.Lock()
	_, retained := updateOrder.users[first.userID]
	updateOrder.Unlock()
	if retained {
		t.Fatal("completed update queue was not reclaimed")
	}
}

func TestStartFastPathDoesNotOvertakeQueuedMessage(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = previous; telegram.ClearSession(88103) })
	jobs := make(chan telegramMessageJob, 2)
	t.Cleanup(func() {
		for len(jobs) > 0 {
			(<-jobs).turn.finish()
		}
	})
	message := func(text string) *tgbotapi.Message {
		return &tgbotapi.Message{From: &tgbotapi.User{ID: 88103}, Chat: &tgbotapi.Chat{ID: 88103, Type: "private"}, Text: text}
	}
	telegram.GetSession(88103).SetStep("WAITING_INPUT")
	dispatchTelegramUpdate(nil, jobs, tgbotapi.Update{Message: message("先前输入")})
	// With no Bot client, incorrectly taking the synchronous fast path would
	// both erase this session and panic while attempting to send the menu.
	dispatchTelegramUpdate(nil, jobs, tgbotapi.Update{Message: message("/start")})
	if len(jobs) != 2 || telegram.GetSession(88103).GetStep() != "WAITING_INPUT" {
		t.Fatal("/start overtook the queued input")
	}
	first, second := <-jobs, <-jobs
	defer first.turn.finish()
	defer second.turn.finish()
	if !first.turn.isReady() || second.turn.isReady() {
		t.Fatal("queued messages were released out of arrival order")
	}
	first.turn.finish()
	if !second.turn.isReady() {
		t.Fatal("finishing the first input did not release /start")
	}
}

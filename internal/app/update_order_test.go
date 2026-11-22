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
	if len(jobs) != 1 || telegram.GetSession(88103).GetStep() != "WAITING_INPUT" {
		t.Fatal("/start overtook the queued input")
	}
	first := <-jobs
	if !first.turn.isReady() {
		t.Fatal("first turn was not ready")
	}
	first.turn.finish()
	if len(jobs) != 1 {
		t.Fatal("finishing the first input did not dispatch /start")
	}
	second := <-jobs
	defer second.turn.finish()
	if !second.turn.isReady() {
		t.Fatal("queued /start was not released in order")
	}

}

func TestWaitingUserMessagesDoNotOccupyWorkers(t *testing.T) {
	slow := reserveUserUpdate(88201)
	jobs := make(chan telegramMessageJob, 3)
	enqueue := func(id int64) *userUpdateTurn {
		turn := reserveUserMessage(id, 3)
		if turn == nil {
			t.Fatal("message unexpectedly rejected")
		}
		turn.whenReady(func() { jobs <- telegramMessageJob{msg: &tgbotapi.Message{From: &tgbotapi.User{ID: id}}, turn: turn} })
		return turn
	}
	first, second, other := enqueue(88201), enqueue(88201), enqueue(88202)
	defer first.finish()
	defer second.finish()
	defer other.finish()
	defer slow.finish()
	if len(jobs) != 1 {
		t.Fatal("waiting same-user turns occupied the ready queue")
	}
	job := <-jobs
	if job.turn != other {
		t.Fatal("unrelated user's turn was starved")
	}
	job.turn.finish()
	slow.finish()
	if len(jobs) != 1 {
		t.Fatal("head was not scheduled after its predecessor finished")
	}
	job = <-jobs
	if job.turn != first {
		t.Fatal("arrival order changed")
	}
	job.turn.finish()
	job = <-jobs
	if job.turn != second {
		t.Fatal("second message was not released in order")
	}
	job.turn.finish()
	updateOrder.Lock()
	pending := updateOrder.messages
	updateOrder.Unlock()
	if pending != 0 {
		t.Fatal("message budget leaked", pending)
	}
}

package activity

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestLotteryPrivateConfirmationDoesNotSpendUntilConfirmed(t *testing.T) {
	db := setupKeywordDB(t)
	bot, cap := newClaimBot(t)
	a := newKeywordLottery("频道抽奖测试")
	a.EntryCost = 5
	if err := createLotteryActivityInTx(db, &a); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{TelegramID: 2, Username: "entrant", Points: 20}).Error; err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}
	start := fmt.Sprintf("/start lottery_%d", a.ID)
	if !IsLotteryStart(start) || IsLotteryStart("/start ref_someone") || IsLotteryStart("普通 lottery_1") {
		t.Fatal("deep link parsing collision")
	}
	HandleLotteryStart(bot, msg, start)
	var user model.User
	db.First(&user, "telegram_id = ?", 2)
	if user.Points != 20 {
		t.Fatal("opening preview spent points")
	}
	if len(cap.snapshot()) == 0 || !strings.Contains(cap.snapshot()[0].Text, "确认后才扣分") {
		t.Fatal("missing cost confirmation")
	}
	cb := &tgbotapi.CallbackQuery{ID: "confirm", From: msg.From, Message: &tgbotapi.Message{Chat: msg.Chat}, Data: fmt.Sprintf("lottery_enter:%d:2", a.ID)}
	HandleLotteryEntryCallback(bot, cb)
	HandleLotteryEntryCallback(bot, cb)
	db.First(&user, "telegram_id = ?", 2)
	var count int64
	db.Model(&model.LotteryParticipant{}).Where("activity_id = ?", a.ID).Count(&count)
	if user.Points != 15 || count != 1 {
		t.Fatal("confirmation was not idempotent")
	}
	cb.From = &tgbotapi.User{ID: 3}
	HandleLotteryEntryCallback(bot, cb)
	if !strings.Contains(cap.snapshot()[len(cap.snapshot())-1].Text, "本人") {
		t.Fatal("forwarded button ownership not checked")
	}
}
func TestChannelLotterySnapshotsAndDurableLifecycle(t *testing.T) {
	db := setupKeywordDB(t)
	if err := db.AutoMigrate(&model.AnnouncementDelivery{}); err != nil {
		t.Fatal(err)
	}
	config.AppConfig.AnnouncementChannelID = -100200
	config.AppConfig.NoticeGroupID = -100300
	bot, _ := newClaimBot(t)
	session := telegram.GetSession(1)
	defer telegram.ClearSession(1)
	for k, v := range map[string]string{"lottery_title": "频道测试", "lottery_join_keyword": "秋日星河", "lottery_mode": lotteryModeCount, "lottery_limit": "5", "lottery_entry_cost": "0", "lottery_prizes": "积分 100 1"} {
		session.SetTemp(k, v)
	}
	id, err := createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, session)
	if err != nil {
		t.Fatal(err)
	}
	var a model.LotteryActivity
	db.First(&a, id)
	if !a.AnnounceIsChannel || a.AnnounceChatID != -100200 {
		t.Fatal("channel destination not frozen")
	}
	var intro model.AnnouncementDelivery
	if err = db.Where("key = ?", fmt.Sprintf("lottery:%d:intro", id)).First(&intro).Error; err != nil || intro.ReminderGroupID != -100300 {
		t.Fatalf("intro not durable: %+v %v", intro, err)
	}
	config.AppConfig.AnnouncementChannelID = -100999
	if err = drawLotteryActivity(bot, id, "manual", 1); err != nil {
		t.Fatal(err)
	}
	var result model.AnnouncementDelivery
	if err = db.Where("key = ?", fmt.Sprintf("lottery:%d:result", id)).First(&result).Error; err != nil || result.TargetChatID != -100200 {
		t.Fatal("draw followed new config instead of saved destination")
	}
	// Existing group lotteries must never migrate just because a channel is added.
	legacy := newKeywordLottery("旧群活动")
	legacy.AnnounceChatID = -100400
	if err = db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err = queueLotteryIntro(db, legacy); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&model.AnnouncementDelivery{}).Where("key = ?", fmt.Sprintf("lottery:%d:intro", legacy.ID)).Count(&n)
	if n != 0 {
		t.Fatal("historical group event migrated")
	}
}
func TestPrivateLotteryConfirmationRechecksActivityAndBalance(t *testing.T) {
	db := setupKeywordDB(t)
	bot, _ := newClaimBot(t)
	a := newKeywordLottery("状态复查")
	a.EntryCost = 10
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{TelegramID: 2, Username: "poor", Points: 5})
	cb := &tgbotapi.CallbackQuery{ID: "confirm", From: &tgbotapi.User{ID: 2}, Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}, Data: fmt.Sprintf("lottery_enter:%d:2", a.ID)}
	HandleLotteryEntryCallback(bot, cb)
	var n int64
	db.Model(&model.LotteryParticipant{}).Count(&n)
	if n != 0 {
		t.Fatal("insufficient balance admitted")
	}
	db.Model(&model.User{}).Where("telegram_id = ?", 2).Update("points", 20)
	past := time.Now().Add(-time.Minute)
	db.Model(&a).Updates(map[string]any{"mode": lotteryModeTime, "draw_at": past})
	HandleLotteryEntryCallback(bot, cb)
	db.Model(&model.LotteryParticipant{}).Count(&n)
	if n != 0 {
		t.Fatal("expired lottery admitted")
	}
	db.Model(&a).Updates(map[string]any{"status": lotteryStatusClosed, "draw_at": nil})
	HandleLotteryEntryCallback(bot, cb)
	db.Model(&model.LotteryParticipant{}).Count(&n)
	if n != 0 {
		t.Fatal("cancelled lottery admitted")
	}
}

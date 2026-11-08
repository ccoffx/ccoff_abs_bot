package app

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/titles"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func TestTitleRoleChangeRetriesDatabaseFailureBeforeConsumingUpdate(t *testing.T) {
	setupTreasureTestDB(t)
	config.AppConfig.NoticeGroupID = -100
	previousVerification := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previousVerification })
	if err := storage.DB.AutoMigrate(&model.TitleGroup{}, &model.TitleMember{}, &model.TitleMemberSyncRequest{}, &model.TitleEquipment{}); err != nil {
		t.Fatal(err)
	}
	equipment := model.TitleEquipment{ChatID: -100, UserID: 1, TitleKey: "escape", DesiredTag: "临阵脱身", AppliedTag: "临阵脱身", Version: 7, SyncedVersion: 7}
	if err := storage.DB.Create(&equipment).Error; err != nil {
		t.Fatal(err)
	}
	attempts := 0
	const callback = "test:title_role_change_transient_failure"
	if err := storage.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "title_member_sync_requests" {
			attempts++
			if attempts == 1 {
				tx.AddError(errors.New("transient database write failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.DB.Callback().Create().Remove(callback) })
	user := &tgbotapi.User{ID: 1}
	dispatchTelegramUpdate(nil, nil, tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{
		Chat:          tgbotapi.Chat{ID: -100, Type: "supergroup"},
		OldChatMember: tgbotapi.ChatMember{User: user, Status: "member"},
		NewChatMember: tgbotapi.ChatMember{User: user, Status: "administrator", CanBeEdited: true},
	}})
	titles.ProcessMemberUpdates(time.Now())
	if err := storage.DB.First(&equipment, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || equipment.Version != 8 || equipment.Version <= equipment.SyncedVersion || equipment.TitleKey != "escape" || equipment.DesiredTag != "临阵脱身" {
		t.Fatalf("role-change event was lost after a transient failure: attempts=%d equipment=%+v", attempts, equipment)
	}
}

func TestTitleEquipmentFailureDoesNotBlockOtherTelegramUpdates(t *testing.T) {
	setupTreasureTestDB(t)
	config.AppConfig.NoticeGroupID = -100
	previousVerification := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previousVerification })
	if err := storage.DB.AutoMigrate(&model.TitleGroup{}, &model.TitleMember{}, &model.TitleMemberSyncRequest{}, &model.TitleEquipment{}); err != nil {
		t.Fatal(err)
	}
	e := model.TitleEquipment{ChatID: -100, UserID: 1, TitleKey: "escape", DesiredTag: "临阵脱身", Version: 7, SyncedVersion: 7}
	if err := storage.DB.Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	failing.Store(true)
	const callback = "test:broken_title_equipment"
	if err := storage.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "title_groups" && failing.Load() {
			tx.AddError(errors.New("title table unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.DB.Callback().Create().Remove(callback) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		user := &tgbotapi.User{ID: 1}
		dispatchTelegramUpdate(nil, nil, tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{
			Chat: tgbotapi.Chat{ID: -100, Type: "supergroup"}, OldChatMember: tgbotapi.ChatMember{User: user, Status: "member"}, NewChatMember: tgbotapi.ChatMember{User: user, Status: "administrator", CanBeEdited: true},
		}})
	}()
	t.Cleanup(func() { failing.Store(false); <-done })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a title-table failure blocked the Telegram receiver")
	}
	now := time.Now()
	titles.ProcessMemberUpdates(now)
	var queued int64
	if err := storage.DB.Model(&model.TitleMemberSyncRequest{}).Count(&queued).Error; err != nil || queued != 1 {
		t.Fatalf("failed resync lost its durable event: queued=%d err=%v", queued, err)
	}
	jobs := make(chan telegramMessageJob, 1)
	dispatchTelegramUpdate(nil, jobs, tgbotapi.Update{Message: &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "我的档案"}})
	select {
	case job := <-jobs:
		job.turn.finish()
	default:
		t.Fatal("another user's message was not dispatched")
	}
	failing.Store(false)
	titles.ProcessMemberUpdates(now.Add(time.Minute))
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil || e.Version != 8 || e.TitleKey != "escape" {
		t.Fatalf("background resync did not recover: %+v err=%v", e, err)
	}
}

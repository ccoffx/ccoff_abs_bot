package titles

import (
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func memberRoleEvent(userID int64) *tgbotapi.ChatMemberUpdated {
	user := &tgbotapi.User{ID: userID}
	return &tgbotapi.ChatMemberUpdated{
		Chat: tgbotapi.Chat{ID: -100, Type: "supergroup"}, OldChatMember: tgbotapi.ChatMember{User: user, Status: "member"}, NewChatMember: tgbotapi.ChatMember{User: user, Status: "administrator", CanBeEdited: true},
	}
}

func TestMemberRoleUpdateDoesNotDependOnRealmConfiguration(t *testing.T) {
	for _, equipped := range []bool{false, true} {
		name := "new member"
		if equipped {
			name = "existing equipment"
		}
		t.Run(name, func(t *testing.T) {
			setupTitleTestDB(t)
			var before model.TitleEquipment
			if equipped {
				if err := TrackMember(-100, "交流群", 1); err != nil {
					t.Fatal(err)
				}
				before = titleTestEquipment(t, -100, 1)
			}
			if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", 99).Error; err != nil {
				t.Fatal(err)
			}
			user := &tgbotapi.User{ID: 1}
			if err := HandleMemberUpdate(&tgbotapi.ChatMemberUpdated{
				Chat:          tgbotapi.Chat{ID: -100, Type: "supergroup"},
				OldChatMember: tgbotapi.ChatMember{User: user, Status: "member"},
				NewChatMember: tgbotapi.ChatMember{User: user, Status: "administrator", CanBeEdited: true},
			}); err != nil {
				t.Fatalf("realm error blocked the member event: %v", err)
			}
			ProcessMemberUpdates(time.Now())
			if equipped {
				after := titleTestEquipment(t, -100, 1)
				if after.Version != before.Version+1 || after.TitleKey != before.TitleKey || after.DesiredTag != before.DesiredTag {
					t.Fatalf("role change did not requeue current selection: %+v", after)
				}
			} else {
				var member model.TitleMember
				if err := storage.DB.First(&member, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
					t.Fatal("new membership was not recorded", err)
				}
				// When the independent realm calculation becomes healthy again,
				// its normal refresh must find this member and create the default.
				if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("major_realm", 2).Error; err != nil {
					t.Fatal(err)
				}
				if err := RefreshDefaults(); err != nil {
					t.Fatal(err)
				}
				if got := titleTestEquipment(t, -100, 1); got.DesiredTag != "三星斗师" {
					t.Fatalf("default title did not recover: %+v", got)
				}
			}
		})
	}
}

func TestMemberUpdateQueueSurvivesRestartAndKeepsLatestSelection(t *testing.T) {
	setupTitleTestDB(t)
	for _, id := range []int64{1, 2} {
		if err := TrackMember(-100, "交流群", id); err != nil {
			t.Fatal(err)
		}
		if err := HandleMemberUpdate(memberRoleEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := HandleMemberUpdate(memberRoleEvent(1)); err != nil {
		t.Fatal(err)
	}
	var request model.TitleMemberSyncRequest
	if err := storage.DB.First(&request, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil || request.Version != 2 {
		t.Fatalf("events were not coalesced: %+v err=%v", request, err)
	}
	if err := storage.DB.Exec(`CREATE TRIGGER fail_one_role_resync BEFORE UPDATE ON title_equipments WHEN NEW.user_id = 1 BEGIN SELECT RAISE(ABORT, 'equipment unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ProcessMemberUpdates(now)
	if err := storage.DB.First(&request, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil || !request.NextAttemptAt.After(now) {
		t.Fatalf("failed event lost retry state: %+v err=%v", request, err)
	}
	if got := titleTestEquipment(t, -100, 2); got.Version != 2 {
		t.Fatal("one failed event blocked another member", got)
	}
	var seq int
	var name, file string
	if err := storage.DB.Raw("PRAGMA database_list").Row().Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	conn, _ := storage.DB.DB()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := gorm.Open(sqlite.Open(file), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	storage.DB = reopened
	reopenedConn, _ := reopened.DB()
	reopenedConn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = reopenedConn.Close() })
	if err := storage.DB.Exec("DROP TRIGGER fail_one_role_resync").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.TribulationTitleHolder{ChatID: -100, UserID: 1, TitleKey: "escape", Count: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	ProcessMemberUpdates(now.Add(time.Minute))
	if got := titleTestEquipment(t, -100, 1); got.TitleKey != "escape" || got.DesiredTag != "临阵脱身" || got.Version != 3 {
		t.Fatal("restarted worker overwrote the latest choice", got)
	}
	var remaining int64
	if err := storage.DB.Model(&model.TitleMemberSyncRequest{}).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("success did not consume queued events: remaining=%d err=%v", remaining, err)
	}
}

func TestMemberUpdateAcknowledgementPreservesNewerEvent(t *testing.T) {
	setupTitleTestDB(t)
	if err := TrackMember(-100, "交流群", 1); err != nil {
		t.Fatal(err)
	}
	if err := HandleMemberUpdate(memberRoleEvent(1)); err != nil {
		t.Fatal(err)
	}
	const callback = "test:newer_role_event"
	injected := false
	if err := storage.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "title_groups" && !injected {
			injected = true
			// A newer event is durable after this batch selected version 1.
			tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Model(&model.TitleMemberSyncRequest{}).Where("chat_id = ? AND user_id = ?", -100, 1).Update("version", gorm.Expr("version + 1")).Error)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.DB.Callback().Create().Remove(callback) })
	ProcessMemberUpdates(time.Now())
	var request model.TitleMemberSyncRequest
	if err := storage.DB.First(&request).Error; err != nil || request.Version != 2 {
		t.Fatalf("old acknowledgement consumed a newer event: %+v err=%v", request, err)
	}
	ProcessMemberUpdates(time.Now())
	var remaining int64
	if err := storage.DB.Model(&model.TitleMemberSyncRequest{}).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("newer event did not complete: %d %v", remaining, err)
	}
}

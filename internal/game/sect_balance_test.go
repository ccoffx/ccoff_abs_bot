package game

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestSectCapacityIncludesTechnology(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	if err := storage.DB.AutoMigrate(&model.SectTechnology{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ level, tech, want int }{
		{1, 0, 15}, {3, 0, 17}, {4, 0, 18}, {7, 0, 21}, {10, 0, 24},
		{1, 5, 25}, {10, 2, 28}, {7, 5, 31}, {10, 5, 34}, {100, 5, 34},
	} {
		if err := storage.DB.Where("sect_id = ?", 1).Delete(&model.SectTechnology{}).Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.DB.Create(&model.SectTechnology{SectID: 1, TechKey: sectTechMemberLimit, Level: tc.tech}).Error; err != nil {
			t.Fatal(err)
		}
		sect := model.Sect{Level: tc.level}
		sect.ID = 1
		got, err := getSectMaxMembersWithTechTxChecked(storage.DB, sect)
		if err != nil || got != tc.want {
			t.Fatalf("level=%d tech=%d: got %d, want %d, err=%v", tc.level, tc.tech, got, tc.want, err)
		}
	}
}

func TestOverCapacitySectRejectsJoinWithoutChargingOrRemovingMembers(t *testing.T) {
	bot := setupSectRegressionTest(t)
	if err := storage.DB.AutoMigrate(&model.SectTechnology{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.Sect{}).Where("id = 1").Update("member_count", 20).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 2, Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	handleConfirmJoinSect(bot, &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, From: &tgbotapi.User{ID: 2}}, "audit")
	var user model.User
	var sect model.Sect
	if err := storage.DB.First(&user, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&sect, 1).Error; err != nil {
		t.Fatal(err)
	}
	var joins, members int64
	if err := storage.DB.Model(&model.SectMember{}).Where("user_id = ?", 2).Count(&joins).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.SectMember{}).Count(&members).Error; err != nil {
		t.Fatal(err)
	}
	if user.Points != 100 || sect.MemberCount != 20 || joins != 0 || members != 1 {
		t.Fatalf("rejected join mutated state: points=%d count=%d joins=%d members=%d", user.Points, sect.MemberCount, joins, members)
	}
}

func TestSectMemberTechnologyUpgradeBeyondThirtyAllowsJoin(t *testing.T) {
	bot := setupSectRegressionTest(t)
	if err := storage.DB.AutoMigrate(&model.SectTechnology{}, &model.SectTechnologyLog{}, &model.PointTransaction{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.Sect{}).Where("id = 1").Updates(map[string]any{"level": 10, "funds": 10000, "prestige": 10000, "member_count": 30}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.SectMember{}).Where("user_id = 1").Update("role", SectRoleOwner).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SectTechnology{SectID: 1, TechKey: sectTechMemberLimit, Level: 3}).Error; err != nil {
		t.Fatal(err)
	}
	handleUpgradeSectTechnology(bot, &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, From: &tgbotapi.User{ID: 1}}, "聚贤纳士", true)
	var sect model.Sect
	var tech model.SectTechnology
	if err := storage.DB.First(&sect, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&tech, "sect_id = ?", 1).Error; err != nil {
		t.Fatal(err)
	}
	if sect.Funds != 9040 || sect.Prestige != 9520 || tech.Level != 4 {
		t.Fatalf("upgrade beyond thirty failed: sect=%+v tech=%+v", sect, tech)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 2, Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	handleConfirmJoinSect(bot, &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, From: &tgbotapi.User{ID: 2}}, "audit")
	var user model.User
	var member model.SectMember
	if err := storage.DB.First(&sect, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&user, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&member, "user_id = ?", 2).Error; err != nil {
		t.Fatal(err)
	}
	if sect.MemberCount != 31 || user.Points != 90 || member.SectID != 1 {
		t.Fatalf("join beyond thirty failed: count=%d points=%d member=%+v", sect.MemberCount, user.Points, member)
	}
}

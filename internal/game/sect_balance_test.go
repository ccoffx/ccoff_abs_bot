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
		{0, 0, 24}, {1, 0, 24}, {2, 0, 27}, {3, 0, 28}, {4, 0, 29},
		{5, 0, 30}, {6, 0, 31}, {7, 0, 32}, {8, 0, 33}, {9, 0, 34}, {10, 0, 35},
		{1, 5, 34}, {10, 2, 39}, {7, 5, 42}, {10, 5, 45}, {100, 5, 45},
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

func TestSectJoinCapacityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		level, count int
		join         bool
	}{
		{"level_one_full", 1, 24, false},
		{"level_one_over_capacity", 1, 25, false},
		{"level_two_last_slot", 2, 26, true},
		{"level_two_full", 2, 27, false},
		{"level_two_over_capacity", 2, 28, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot := setupSectRegressionTest(t)
			if err := storage.DB.AutoMigrate(&model.SectTechnology{}, &model.PointTransaction{}); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.Sect{}).Where("id = 1").Updates(map[string]any{"level": tc.level, "member_count": tc.count}).Error; err != nil {
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
			var joins, members, charges int64
			if err := storage.DB.Model(&model.SectMember{}).Where("user_id = ?", 2).Count(&joins).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.SectMember{}).Count(&members).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.PointTransaction{}).Where("user_id = ?", 2).Count(&charges).Error; err != nil {
				t.Fatal(err)
			}
			wantPoints, wantCount := 100, tc.count
			var wantJoins int64
			if tc.join {
				wantPoints -= sectJoinCost
				wantCount++
				wantJoins = 1
			}
			if user.Points != wantPoints || sect.MemberCount != wantCount || joins != wantJoins || members != 1+wantJoins || charges != wantJoins {
				t.Fatalf("join boundary: points=%d count=%d joins=%d members=%d charges=%d", user.Points, sect.MemberCount, joins, members, charges)
			}
		})
	}
}

func TestSectMemberTechnologyUpgradeBeyondThirtyAllowsJoin(t *testing.T) {
	bot := setupSectRegressionTest(t)
	if err := storage.DB.AutoMigrate(&model.SectTechnology{}, &model.SectTechnologyLog{}, &model.PointTransaction{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.Sect{}).Where("id = 1").Updates(map[string]any{"level": 10, "funds": 10000, "prestige": 10000, "member_count": 41}).Error; err != nil {
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
	if sect.MemberCount != 42 || user.Points != 90 || member.SectID != 1 {
		t.Fatalf("join beyond thirty failed: count=%d points=%d member=%+v", sect.MemberCount, user.Points, member)
	}
}

package app

import (
	"strings"
	"testing"

	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestAdminParametersProceedToConfirmationWithoutExplanation(t *testing.T) {
	bot := channelOpsFixture(t)
	config.AppConfig.NoticeGroupID = 0
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	defer telegram.ClearSession(1)
	for _, tc := range []struct {
		step, input, want string
		temp              map[string]string
	}{
		{"WAITING_GEN_INVITE_COUNT", "2", "WAITING_CONFIRM_GEN_INVITE", nil},
		{"WAITING_GEN_RENEW_COUNT", "2", "WAITING_CONFIRM_GEN_RENEW", map[string]string{"days": "30"}},
		{"WAITING_SET_INVITE_PRICE", "500", "WAITING_CONFIRM_SET_INVITE_PRICE", nil},
		{"WAITING_SET_RENEW_PRICE", "500", "WAITING_CONFIRM_SET_RENEW_PRICE", nil},
		{"WAITING_PROMOTE_ID", "2", "WAITING_CONFIRM_PROMOTE", nil},
		{"WAITING_WHITELIST_ID", "2", "WAITING_CONFIRM_WHITELIST", nil},
		{"WAITING_SET_SERVER_LINES", "https://example.invalid", "WAITING_CONFIRM_SET_SERVER_LINES", nil},
		{"WAITING_GEN_INVITE_COUNT", "101", "WAITING_GEN_INVITE_COUNT", nil},
		{"WAITING_BOOK_LINK", "https://www.ximalaya.com/album/123456", "WAITING_BOOK_CONFIRM", nil},
	} {
		t.Run(tc.step+tc.input, func(t *testing.T) {
			telegram.ClearSession(1)
			session := telegram.GetSession(1)
			session.SetStep(tc.step)
			for k, v := range tc.temp {
				session.SetTemp(k, v)
			}
			msg.Text = tc.input
			handleInteractiveMessage(bot, msg)
			if got := telegram.GetSession(1).GetStep(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	var count int64
	if err := storage.DB.Model(&model.AuditLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("preview must not perform admin mutations")
	}
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", 2).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Role == "admin" || user.IsWhitelist {
		t.Fatal("confirmation was bypassed")
	}
}

func TestServiceWizardsGenerateExplanationsAndKeepConfirmation(t *testing.T) {
	bot := channelOpsFixture(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	defer telegram.ClearSession(1)
	for _, tc := range []struct {
		step, input, want string
		temp              map[string]string
	}{
		{"WAITING_OPEN_REG_VALUE", "5", "WAITING_OPEN_REG_CONFIRM", map[string]string{"open_reg_mode": openRegistrationModeQuota}},
		{"WAITING_COMPENSATION_POINTS", "100", "WAITING_COMPENSATION_CONFIRM", map[string]string{"comp_mode": "1"}},
		{"WAITING_COMPENSATION_DAYS", "3", "WAITING_COMPENSATION_CONFIRM", map[string]string{"comp_mode": "2", "comp_points": "0"}},
	} {
		telegram.ClearSession(1)
		session := telegram.GetSession(1)
		session.SetStep(tc.step)
		for k, v := range tc.temp {
			session.SetTemp(k, v)
		}
		if !HandleAdminServiceOperations(bot, msg, tc.input, session, "super_admin") {
			t.Fatal("unhandled")
		}
		if got := session.GetStep(); got != tc.want {
			t.Fatalf("%s: got %s", tc.step, got)
		}
	}
	var count int64
	storage.DB.Model(&model.CompensationCampaign{}).Count(&count)
	if count != 0 {
		t.Fatal("preview created compensation")
	}
}

func TestLotteryAndWhitelistCreationSkipExplanation(t *testing.T) {
	bot := channelOpsFixture(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	telegram.ClearSession(1)
	defer telegram.ClearSession(1)
	session := telegram.GetSession(1)
	if !activity.HandleLotteryCommand(bot, msg, "创建积分抽奖", session, "super_admin") {
		t.Fatal("unhandled")
	}
	if session.GetStep() != "WAITING_LOTTERY_MODE" || !strings.HasPrefix(session.GetTemp("lottery_title"), "积分抽奖 ") {
		t.Fatal("lottery still asks for title")
	}
	session.SetStep("WL_CARD_COUNT")
	if !handleWhitelistCardMessage(bot, msg, "2", session) || session.GetStep() != "WL_CARD_GENERATE" {
		t.Fatal("whitelist did not reach confirmation")
	}
	if session.GetTemp("wl_reason") != storage.AutomaticAdminReason {
		t.Fatal("missing automatic audit context")
	}
}

func TestConfigurationWritesSkipExplanationAndRequireConfirmation(t *testing.T) {
	bot := channelOpsFixture(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
	telegram.ClearSession(1)
	defer telegram.ClearSession(1)
	if !HandleCultivationAdminWriteCommand(bot, msg, "重载斗气配置") || telegram.GetSession(1).GetStep() != "WAITING_CONFIRM_CULTIVATION_ADMIN_WRITE" {
		t.Fatal("cultivation confirmation missing")
	}
	telegram.ClearSession(1)
	if !game.HandleSectSecretRealmAdminCommand(bot, msg, "设置秘境倍率 normal 1 100 100") {
		t.Fatal("realm command unhandled")
	}
	if telegram.GetSession(1).GetStep() != "WAITING_CONFIRM_SECT_REALM_CONFIG" {
		t.Fatal("realm confirmation missing")
	}
}

func TestInviteGenerationWithoutExplanationStillRequiresConfirmationAndAudits(t *testing.T) {
	bot := channelOpsFixture(t)
	config.AppConfig.NoticeGroupID = 0
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	if err := storage.DB.AutoMigrate(&model.InviteCode{}); err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "2"}
	telegram.ClearSession(1)
	defer telegram.ClearSession(1)
	telegram.GetSession(1).SetStep("WAITING_GEN_INVITE_COUNT")
	handleInteractiveMessage(bot, msg)
	var count int64
	if err := storage.DB.Model(&model.InviteCode{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("preview created codes")
	}
	msg.Text = "确认生成邀请码"
	handleInteractiveMessage(bot, msg)
	if err := storage.DB.Model(&model.InviteCode{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("want 2 codes, got %d", count)
	}
	var audit model.AuditLog
	if err := storage.DB.Where("action = ?", "GENERATE_INVITE_CODES").First(&audit).Error; err != nil {
		t.Fatal(err)
	}
}

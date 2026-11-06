package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/market"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestCodeCashoutAdminCommandsConfirmCancelAuditAndPermissions(t *testing.T) {
	bot, requests := groupReplyFixture(t)
	config.AppConfig.NoticeGroupID = 0
	send := func(id int64, chatType, text string) {
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: id}, Chat: &tgbotapi.Chat{ID: id, Type: chatType}, Text: text})
	}
	assertEnabled := func(enabled bool) {
		t.Helper()
		err := market.CheckCodeCashoutEnabled(storage.DB)
		if enabled && err != nil || !enabled && !errors.Is(err, market.ErrCodeCashoutDisabled) {
			t.Fatalf("enabled=%t: %v", enabled, err)
		}
	}
	for _, chatType := range []string{"private", "group"} {
		send(2, chatType, "关闭卡密回收")
		send(2, chatType, "确认关闭卡密回收")
		assertEnabled(true)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Update("role", "admin").Error; err != nil {
		t.Fatal(err)
	}
	send(2, "private", "关闭卡密回收")
	send(1, "group", "关闭卡密回收")
	assertEnabled(true)
	if telegram.GetSession(1).GetStep() == codeCashoutAdminConfirmStep || telegram.GetSession(2).GetStep() == codeCashoutAdminConfirmStep {
		t.Fatal("unauthorized request opened confirmation")
	}
	send(1, "private", "关闭卡密回收")
	assertEnabled(true)
	send(1, "private", "取消")
	send(1, "private", "确认关闭卡密回收")
	assertEnabled(true)
	send(1, "private", "关闭卡密回收")
	config.AppConfig.AdminIDs = nil
	send(1, "private", "确认关闭卡密回收")
	assertEnabled(true)
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	send(1, "private", "关闭卡密回收")
	send(1, "private", "确认开启卡密回收")
	assertEnabled(true)
	send(1, "private", "确认关闭卡密回收")
	assertEnabled(false)
	send(1, "private", "卡密回收状态")
	last := (*requests)[len(*requests)-1]
	if !strings.Contains(last.fields["text"], "已关闭") {
		t.Fatalf("status omitted closure: %+v", last)
	}
	send(1, "private", "开启卡密回收")
	assertEnabled(false)
	send(1, "private", "确认开启卡密回收")
	send(1, "private", "确认开启卡密回收")
	assertEnabled(true)
	var audits []model.AuditLog
	if err := storage.DB.Where("action = ?", "SET_CODE_CASHOUT_ENABLED").Order("id").Find(&audits).Error; err != nil || len(audits) != 2 {
		t.Fatalf("want one audit per confirmed change: %+v %v", audits, err)
	}
	for _, audit := range audits {
		if audit.ActorID != 1 || audit.Target != market.CodeCashoutEnabledKey {
			t.Fatalf("incorrect audit actor or target: %+v", audit)
		}
	}
	for _, menu := range []string{"config", "cards"} {
		_, markup := renderSuperAdminMenu(menu)
		data, err := json.Marshal(markup)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"code_cashout_status", "code_cashout_disable", "code_cashout_enable"} {
			if _, superOnly, ok := adminMenuCommandText(key); !ok || !superOnly || !strings.Contains(string(data), "admin:cmd:"+key) {
				t.Fatalf("missing privileged %s entry in %s", key, menu)
			}
		}
	}
}

func TestCodeCashoutAdminAuditFailureRollsBackSwitch(t *testing.T) {
	bot, requests := groupReplyFixture(t)
	config.AppConfig.NoticeGroupID = 0
	if err := storage.DB.Exec(`CREATE TRIGGER fail_cashout_switch_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	handleInteractiveMessage(bot, groupReplyAdminMessage("关闭卡密回收"))
	handleInteractiveMessage(bot, groupReplyAdminMessage("确认关闭卡密回收"))
	if err := market.CheckCodeCashoutEnabled(storage.DB); err != nil {
		t.Fatal("audit failure changed switch", err)
	}
	if !strings.Contains((*requests)[len(*requests)-1].fields["text"], "保存失败") {
		t.Fatal("failed save was reported as successful")
	}
}

func TestCodeCashoutRoutesBlockEntryInputAndPendingConfirmation(t *testing.T) {
	bot, requests := groupReplyFixture(t)
	config.AppConfig.NoticeGroupID = 0
	config.AppConfig.SecurityPepper = strings.Repeat("s", 64)
	if err := storage.DB.AutoMigrate(&model.InviteCode{}, &model.RenewCode{}); err != nil {
		t.Fatal(err)
	}
	raw := "benefitsecret123456"
	if err := storage.CreateInviteCodeRecord(storage.DB, raw); err != nil {
		t.Fatal(err)
	}
	send := func(text string) {
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: text})
	}
	for _, stage := range []string{"entry", "input", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			telegram.ClearSession(2)
			if err := storage.SetSystemConfigStringChecked(market.CodeCashoutEnabledKey, "1"); err != nil {
				t.Fatal(err)
			}
			next := "卡密回收"
			if stage != "entry" {
				send("卡密回收")
				next = raw
			}
			if stage == "confirmation" {
				send(raw)
				if telegram.GetSession(2).GetStep() != "WAITING_CODE_CASHOUT_CONFIRM" {
					t.Fatal("could not get quote before closure")
				}
				next = "确认回收卡密"
			}
			if err := storage.SetSystemConfigStringChecked(market.CodeCashoutEnabledKey, "0"); err != nil {
				t.Fatal(err)
			}
			send(next)
			last := (*requests)[len(*requests)-1].fields["text"]
			if !strings.Contains(last, "卡密回收已暂停") || telegram.GetSession(2).GetStep() != "IDLE" {
				t.Fatalf("closure not enforced at %s: %s", stage, last)
			}
			var code model.InviteCode
			if err := storage.DB.First(&code).Error; err != nil || code.IsUsed || code.CashedOutAt != nil {
				t.Fatalf("closed route consumed code: %+v %v", code, err)
			}
			var user model.User
			if err := storage.DB.Where("telegram_id = ?", 2).First(&user).Error; err != nil || user.Points != 100 {
				t.Fatalf("closed route awarded points: %+v %v", user, err)
			}
		})
	}
}

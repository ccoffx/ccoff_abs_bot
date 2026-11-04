package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestSignInRollsBackWhenSectContributionFails(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	if err := storage.DB.AutoMigrate(
		&model.SignInStreak{}, &model.SignInLog{}, &model.SignInRewardClaim{},
		&model.PointTransaction{}, &model.Sect{}, &model.SectMember{},
		&model.SectContributionLog{}, &model.Cultivation{},
	); err != nil {
		t.Fatal(err)
	}
	user := model.User{TelegramID: 101, Username: "sign_user", Points: 10}
	sect := model.Sect{Name: "测试宗门", OwnerID: 101, MemberCount: 1}
	if err := storage.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&sect).Error; err != nil {
		t.Fatal(err)
	}
	member := model.SectMember{SectID: int64(sect.ID), UserID: 101, Role: "owner"}
	if err := storage.DB.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE TRIGGER fail_signin_contribution BEFORE INSERT ON sect_contribution_logs BEGIN SELECT RAISE(ABORT, 'simulated contribution failure'); END`).Error; err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		result := any(map[string]any{"message_id": 1, "chat": map[string]any{"id": 101, "type": "private"}})
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			result = map[string]any{"id": 1, "is_bot": true, "first_name": "test"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	HandleUserSignIn(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 101, FirstName: "Sign"}, Chat: &tgbotapi.Chat{ID: 101, Type: "private"}})

	var signCount, pointCount int64
	if err := storage.DB.Model(&model.SignInLog{}).Count(&signCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.PointTransaction{}).Count(&pointCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&user, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&sect, sect.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&member, member.ID).Error; err != nil {
		t.Fatal(err)
	}
	if signCount != 0 || pointCount != 0 || user.Points != 10 || sect.Prestige != 0 || member.Contribution != 0 {
		t.Fatalf("failed contribution committed sign-in: logs=%d points=%d balance=%d prestige=%d contribution=%d", signCount, pointCount, user.Points, sect.Prestige, member.Contribution)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_signin_contribution").Error; err != nil {
		t.Fatal(err)
	}
	HandleUserSignIn(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 101, FirstName: "Sign"}, Chat: &tgbotapi.Chat{ID: 101, Type: "private"}})
	if err := storage.DB.Model(&model.SignInLog{}).Count(&signCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&sect, sect.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&member, member.ID).Error; err != nil {
		t.Fatal(err)
	}
	if signCount != 1 || sect.Prestige != 1 || member.Contribution != 1 {
		t.Fatalf("retry did not settle sign-in and contribution once: logs=%d prestige=%d contribution=%d", signCount, sect.Prestige, member.Contribution)
	}
}

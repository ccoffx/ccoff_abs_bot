package account

import (
	"strings"
	"testing"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func TestWealthLeaderboardWalletNames(t *testing.T) {
	setupReferralAutoClaimTestDB(t)
	oldConfig := config.AppConfig
	config.AppConfig = nil
	t.Cleanup(func() { config.AppConfig = oldConfig })
	if err := storage.DB.AutoMigrate(&model.RankTitleHolder{}); err != nil {
		t.Fatal(err)
	}
	users := []model.User{
		{TelegramID: 1, Username: "ghost_tg_1", DisplayName: "小_明*", Points: 300},
		{TelegramID: 2, Username: "normal_account", Points: 200},
		{TelegramID: 3, Username: "ghost_tg_3", Points: 100},
		{TelegramID: 4, Username: "admin", Role: "admin", Points: 999},
	}
	if err := storage.DB.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	rows, total, page, err := loadWealthLeaderboardPage(1)
	if err != nil || total != 3 || page != 1 || len(rows) != 3 {
		t.Fatalf("load: rows=%v total=%d page=%d err=%v", rows, total, page, err)
	}
	text := formatWealthLeaderboardPage(rows, total, page)
	for _, want := range []string{textfmt.EscapeMarkdown("小_明*"), "normal\\_account", "道友3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "ghost") {
		t.Fatal("internal wallet name exposed", text)
	}
	for i, row := range rows {
		if row.TelegramID != int64(i+1) || row.Points != users[i].Points {
			t.Fatal("ranking or balance changed", rows)
		}
	}
	var saved model.User
	if err := storage.DB.First(&saved, users[0].ID).Error; err != nil || saved.Username != "ghost_tg_1" {
		t.Fatal("account identity changed", saved, err)
	}
}

package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestCultivationRankShowsOwnerPerkAndHistoricalBonus(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	db := storage.DB
	if err := db.AutoMigrate(&model.Cultivation{}, &model.Sect{}, &model.SectMember{}, &model.RankTitleHolder{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 4; id++ {
		for _, row := range []any{
			&model.User{TelegramID: id, Username: fmt.Sprintf("rank%d", id), AbsUserID: fmt.Sprintf("abs%d", id)},
			&model.Cultivation{UserID: id, MajorRealm: 2, MinorRealm: 1, TotalAudioTime: float64(100 - id), PillAudioTime: 10},
		} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, owner := range []int64{1, 4} {
		sect := model.Sect{Name: fmt.Sprintf("sect%d", owner), OwnerID: owner}
		if err := db.Create(&sect).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.SectMember{UserID: owner, SectID: int64(sect.ID), Role: "owner"}).Error; err != nil {
			t.Fatal(err)
		}
		if owner == 1 {
			if err := db.Create(&model.SectMember{UserID: 3, SectID: int64(sect.ID), Role: "member"}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	today := SectDayKey(time.Now())
	for _, row := range []model.DailyListeningStat{
		{UserID: 1, DayKey: today, SectOwnerBonusHours: .192},
		{UserID: 1, DayKey: "2020-01-01", SectOwnerBonusHours: .108},
		{UserID: 1, DayKey: "2099-01-01", SectOwnerBonusHours: 999},
		{UserID: 2, DayKey: today, SectOwnerBonusHours: .125},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	deleted := model.DailyListeningStat{UserID: 1, DayKey: "2020-01-02", SectOwnerBonusHours: 888}
	if err := db.Create(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	texts := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			texts <- r.Form.Get("text")
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	HandleCultivationRank(bot, 1)
	text := <-texts
	entries := strings.Split(text, "\n\n")
	if len(entries) < 6 {
		t.Fatal("incomplete ranking", text)
	}
	for i, expected := range []struct{ perk, bonus string }{
		{"宗主专享", "+0.300"},
		{"", "+0.125"},
		{"", ""},
		{"宗主专享", "+0.000"},
	} {
		entry := entries[i+1]
		if !strings.Contains(entry, fmt.Sprintf("rank%d", i+1)) || strings.Contains(entry, "宗主专享") != (expected.perk != "") || strings.Contains(entry, "累计宗主额外修为") != (expected.bonus != "") {
			t.Errorf("incorrect identity or bonus visibility for rank %d: %s", i+1, entry)
		}
		if expected.bonus != "" && !strings.Contains(entry, expected.bonus) {
			t.Error("incorrect accumulated bonus", entry)
		}
	}
	if !strings.Contains(text, "总修为 `109.0`") || !strings.Contains(text, "创建宗门成为宗主") || !strings.Contains(text, "身份变更次日生效") {
		t.Fatal("totals changed or perk promotion missing", text)
	}
}

package app

import (
	"ccoff-abs-bot/internal/background"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestFreshAccountAndListeningQueriesDoNotWaitForABS(t *testing.T) {
	setupTreasureTestDB(t)
	previousVerification, previousClient := joinVerification, abs.DefaultClient
	joinVerification = nil
	t.Cleanup(func() { joinVerification, abs.DefaultClient = previousVerification, previousClient })
	if err := storage.DB.AutoMigrate(&model.SignInStreak{}, &model.SignInLog{}, &model.SignInRewardClaim{}, &model.Sect{}, &model.SectMember{}, &model.SectContributionLog{}, &model.DailyListeningStat{}, &model.ListeningReportSnapshot{}, &model.RankTitleHolder{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Updates(map[string]any{"abs_user_id": "abs1", "status": "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Cultivation{UserID: 1, ListeningInitialized: true, MajorRealm: 2, MinorRealm: 9, TotalAudioTime: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: game.SectDayKey(time.Now()), RawSeconds: 3600, CappedSeconds: 3600, EffectiveHours: 1, BaseCultivationHours: 1, LastFetchedAt: time.Now(), CultivationApplied: true, AppliedEligible: true, AppliedEffectiveHours: 1, AppliedCappedSeconds: 3600, AppliedBaseHours: 1, AppliedTotalHours: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.ListeningReportSnapshot{UserID: 1, AbsUserID: "abs1", StatsFetchedAt: time.Now(), ProgressFetchedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	absServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer absServer.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: absServer.URL, Token: "test", HttpClient: absServer.Client()}
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		mu.Lock()
		texts = append(texts, r.Form.Get("text"))
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	defer waitActivityBackground(t)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"签到", "斗气榜", "我的信息", "听书报告", "刷新我的今日净修为"} {
		mu.Lock()
		before := len(texts)
		mu.Unlock()
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "Listener"}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: command})
		if command == "我的信息" || command == "听书报告" {
			mu.Lock()
			reply := strings.Join(texts[before:], "\n")
			mu.Unlock()
			if strings.Contains(reply, "待结算") {
				t.Fatalf("%s failed to use a settled fresh amount: %s", command, reply)
			}
		}
	}
	// A seven-minute snapshot is within the shared ten-minute query cache.
	if err := storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("last_fetched_at", time.Now().Add(-7*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.ListeningReportSnapshot{}).Where("user_id = ?", 1).Update("progress_fetched_at", time.Now().Add(-7*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"刷新我的今日净修为", "听书报告", "我的信息"} {
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "Listener"}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: command})
	}
	if requests.Load() != 0 {
		t.Fatalf("local commands made %d ABS requests", requests.Load())
	}
	for _, command := range []string{"刷新我的今日净修为", "听书报告", "我的信息"} {
		if err := storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("last_fetched_at", time.Now().Add(-11*time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		before := requests.Load()
		handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "Listener"}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: command})
		if command == "刷新我的今日净修为" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := background.Default.WaitIdle(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() <= before {
				t.Fatal("explicit refresh did not run in the background")
			}
		} else if requests.Load() != before {
			t.Fatalf("%s queried ABS instead of its saved data", command)
		}
	}
	// A failed refresh after a binding change must not expose the old account's row.
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := len(texts)
	mu.Unlock()
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: "刷新我的今日净修为"})
	waitActivityBackground(t)
	mu.Lock()
	reply := strings.Join(texts[before:], "\n")
	mu.Unlock()
	if !strings.Contains(reply, "今日暂无听书统计") || strings.Contains(reply, "今日净修为：") {
		t.Fatalf("old binding's daily statistics were displayed: %s", reply)
	}
	// Admin queries use the current binding for today, while history remains readable.
	for _, historical := range []bool{false, true} {
		command := "查看每日净修为 1"
		if historical {
			var old model.DailyListeningStat
			if err := storage.DB.Where("user_id = ?", 1).First(&old).Error; err != nil {
				t.Fatal(err)
			}
			old.ID = 0
			old.DayKey = game.SectDayKey(time.Now().AddDate(0, 0, -1))
			if err := storage.DB.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			command += " " + old.DayKey
		}
		mu.Lock()
		before := len(texts)
		mu.Unlock()
		game.HandleDailyListeningStatCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, command, "admin")
		mu.Lock()
		reply := strings.Join(texts[before:], "\n")
		mu.Unlock()
		if historical != strings.Contains(reply, "今日净修为：") {
			t.Fatalf("historical=%v unexpected admin reply: %s", historical, reply)
		}
	}
	mu.Lock()
	joined := strings.Join(texts, "\n")
	mu.Unlock()
	for _, want := range []string{"签到成功", "斗气风云榜", "100.0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in replies: %s", want, joined)
		}
	}
}

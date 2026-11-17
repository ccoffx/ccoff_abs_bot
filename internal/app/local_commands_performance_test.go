package app

import (
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
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"签到", "斗气榜", "我的信息", "听书报告"} {
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
	if requests.Load() != 0 {
		t.Fatalf("local commands made %d ABS requests", requests.Load())
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

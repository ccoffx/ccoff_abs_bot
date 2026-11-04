package account

import (
	"crypto/rand"
	"errors"
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

type signInFailingEntropy struct{ remaining int }

func (reader *signInFailingEntropy) Read(p []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, errors.New("entropy unavailable")
	}
	reader.remaining--
	clear(p)
	return len(p), nil
}

func TestSignInEntropyFailurePreservesStreakAndBalance(t *testing.T) {
	for successfulReads := 0; successfulReads <= 1; successfulReads++ {
		t.Run(fmt.Sprint(successfulReads), func(t *testing.T) {
			setupReferralAutoClaimTestDB(t)
			if err := storage.DB.AutoMigrate(&model.SignInStreak{}, &model.SignInLog{}, &model.SignInRewardClaim{}, &model.PointTransaction{}, &model.Cultivation{}, &model.SectMember{}); err != nil {
				t.Fatal(err)
			}
			for _, index := range storage.DatabaseIndexes {
				if strings.Contains(index, "idx_sign_in_") || strings.Contains(index, "idx_cultivations_") {
					if err := storage.DB.Exec(index).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			user := model.User{TelegramID: 101, Username: "sign_user", Points: 10}
			streak := model.SignInStreak{UserID: 101, CurrentStreakDays: 13, LongestStreakDays: 13, TotalSignDays: 13, CycleSeq: 1, LastSignDate: storage.SignInDateKey(time.Now().AddDate(0, 0, -1))}
			if err := storage.DB.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Create(&streak).Error; err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test"}}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":101,"type":"private"}}}`)
			}))
			t.Cleanup(server.Close)
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 101, FirstName: "Sign"}, Chat: &tgbotapi.Chat{ID: 101, Type: "private"}}
			func() {
				previous := rand.Reader
				rand.Reader = &signInFailingEntropy{remaining: successfulReads}
				defer func() { rand.Reader = previous }()
				HandleUserSignIn(bot, msg)
			}()
			if err := storage.DB.First(&streak, streak.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.First(&user, user.ID).Error; err != nil {
				t.Fatal(err)
			}
			if user.Points != 10 || user.LastSignAt != nil || streak.CurrentStreakDays != 13 || streak.TotalSignDays != 13 {
				t.Fatalf("failed sign-in changed balance or streak: user=%+v streak=%+v", user, streak)
			}
			for _, row := range []any{&model.SignInLog{}, &model.SignInRewardClaim{}, &model.PointTransaction{}} {
				var count int64
				if err := storage.DB.Model(row).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("failed sign-in committed %d %T records", count, row)
				}
			}
			HandleUserSignIn(bot, msg)
			if err := storage.DB.First(&streak, streak.ID).Error; err != nil || streak.CurrentStreakDays != 14 {
				t.Fatalf("sign-in did not recover: streak=%+v error=%v", streak, err)
			}
		})
	}
}

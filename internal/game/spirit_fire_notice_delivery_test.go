package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSpiritFireNoticeDelivery(t *testing.T, send http.HandlerFunc) (*tgbotapi.BotAPI, *atomic.Int64, model.SpiritFireNotice) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "notice.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	old := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = old; _ = conn.Close() })
	if err := db.AutoMigrate(&model.SpiritFireNotice{}); err != nil {
		t.Fatal(err)
	}
	notice := model.SpiritFireNotice{UserID: 1, Text: "异火奖励已到账"}
	if err := db.Create(&notice).Error; err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		sends.Add(1)
		if send != nil {
			send(w, r)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, &sends, notice
}

func readSpiritFireNotice(t *testing.T, id uint) model.SpiritFireNotice {
	t.Helper()
	var notice model.SpiritFireNotice
	if err := storage.DB.First(&notice, id).Error; err != nil {
		t.Fatal(err)
	}
	return notice
}

func TestSpiritFireNoticeDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		code         int
	}{
		{"delivered", spiritFireNoticeSent, 0},
		{"blocked", spiritFireNoticeFailed, 403},
		{"server outcome unknown", spiritFireNoticeUncertain, 502},
		{"response outcome unknown", spiritFireNoticeUncertain, -1},
		{"rate limited", spiritFireNoticePending, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, sends, notice := setupSpiritFireNoticeDelivery(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.code == -1 {
					fmt.Fprint(w, "incomplete response")
					return
				}
				if tc.code == 0 {
					fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":1,"type":"private"}}}`)
					return
				}
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"simulated failure","parameters":{"retry_after":600}}`, tc.code)
			})
			started := time.Now()
			if err := dispatchSpiritFireNotices(bot); err != nil {
				t.Fatal(err)
			}
			notice = readSpiritFireNotice(t, notice.ID)
			if notice.Status != tc.status || notice.Attempts != 1 || notice.SendingAt == nil {
				t.Fatalf("incorrect receipt: %+v", notice)
			}
			if tc.code == 429 && (notice.NextAttemptAt == nil || notice.NextAttemptAt.Before(started.Add(600*time.Second))) {
				t.Fatalf("Telegram retry_after ignored: %+v", notice)
			}
			if err := dispatchSpiritFireNotices(bot); err != nil {
				t.Fatal(err)
			}
			if sends.Load() != 1 {
				t.Fatalf("delivered, rejected, uncertain or delayed notice resent: %d", sends.Load())
			}
			if tc.code == 429 {
				if err := storage.DB.Model(&notice).Update("next_attempt_at", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
				if err := dispatchSpiritFireNotices(bot); err != nil {
					t.Fatal(err)
				}
				if sends.Load() != 2 || readSpiritFireNotice(t, notice.ID).Attempts != 2 {
					t.Fatal("confirmed rate limit did not retry when due")
				}
			}
		})
	}
}

func TestSpiritFireNoticeReceiptFailureDoesNotResend(t *testing.T) {
	bot, sends, notice := setupSpiritFireNoticeDelivery(t, nil)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_notice_receipt BEFORE UPDATE OF status ON spirit_fire_notices WHEN NEW.status = 'sent' BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := dispatchSpiritFireNotices(bot); err == nil {
		t.Fatal("receipt failure was hidden")
	}
	if readSpiritFireNotice(t, notice.ID).Status != spiritFireNoticeSending {
		t.Fatal("uncommitted receipt lost its durable claim")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_notice_receipt").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&notice).Update("sending_at", time.Now().Add(-2*spiritFireNoticeStaleAge)).Error; err != nil {
		t.Fatal(err)
	}
	if err := dispatchSpiritFireNotices(bot); err != nil {
		t.Fatal(err)
	}
	if readSpiritFireNotice(t, notice.ID).Status != spiritFireNoticeUncertain || sends.Load() != 1 {
		t.Fatal("recovery repeated a possibly delivered notice")
	}
}

func TestSpiritFireNoticeClaimFailureDoesNotSend(t *testing.T) {
	bot, sends, notice := setupSpiritFireNoticeDelivery(t, nil)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_notice_claim BEFORE UPDATE OF status ON spirit_fire_notices WHEN NEW.status = 'sending' BEGIN SELECT RAISE(ABORT, 'claim unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := dispatchSpiritFireNotices(bot); err == nil {
		t.Fatal("claim failure was hidden")
	}
	if sends.Load() != 0 || readSpiritFireNotice(t, notice.ID).Status != spiritFireNoticePending {
		t.Fatal("sent without durable ownership of the notice")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_notice_claim").Error; err != nil {
		t.Fatal(err)
	}
	if err := dispatchSpiritFireNotices(bot); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 || readSpiritFireNotice(t, notice.ID).Status != spiritFireNoticeSent {
		t.Fatal("pending notice did not recover")
	}
}

func TestConcurrentSpiritFireNoticeDispatchDoesNotDuplicate(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	bot, sends, _ := setupSpiritFireNoticeDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":1,"type":"private"}}}`)
	})
	done := make(chan error, 1)
	go func() { done <- dispatchSpiritFireNotices(bot) }()
	<-started
	err := dispatchSpiritFireNotices(bot)
	close(release)
	firstErr := <-done
	if err != nil || firstErr != nil || sends.Load() != 1 {
		t.Fatalf("concurrent dispatch: sends=%d errors=%v/%v", sends.Load(), err, firstErr)
	}
}

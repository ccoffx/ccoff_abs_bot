package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type hornAPICapture struct {
	sync.Mutex
	sends map[int64]int
	codes map[int64]int
}

func (capture *hornAPICapture) count(userID int64) int {
	capture.Lock()
	defer capture.Unlock()
	return capture.sends[userID]
}

func setupHornDeliveryTest(t *testing.T) (*tgbotapi.BotAPI, *hornAPICapture, model.SectHornBroadcast) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "horn.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	if err := db.AutoMigrate(&model.SectHornBroadcast{}, &model.SectHornDelivery{}); err != nil {
		t.Fatal(err)
	}
	broadcast := model.SectHornBroadcast{HornID: "horn-test", Scope: sectHornScopeSect, SenderID: 99, Content: "今晚准备开启宗门秘境", RecipientCount: 2, Status: sectHornStatusQueued, SubmittedAt: time.Now()}
	if err := db.Create(&broadcast).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := db.Create(&model.SectHornDelivery{HornID: broadcast.HornID, UserID: id, Status: sectHornDeliveryPending}).Error; err != nil {
			t.Fatal(err)
		}
	}
	capture := &hornAPICapture{sends: make(map[int64]int), codes: make(map[int64]int)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"test"}}`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		id, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		capture.Lock()
		capture.sends[id]++
		code := capture.codes[id]
		capture.Unlock()
		if code != 0 {
			fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"simulated failure"}`, code)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":%d,"type":"private"}}}`, id)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, capture, broadcast
}

func ageHornAttempt(t *testing.T, hornID string) {
	t.Helper()
	if err := storage.DB.Model(&model.SectHornBroadcast{}).Where("horn_id = ?", hornID).
		UpdateColumn("updated_at", time.Now().Add(-2*sectHornStaleSendingAge)).Error; err != nil {
		t.Fatal(err)
	}
}

func TestHornFinalizationFailureDoesNotRepeatDeliveredMessages(t *testing.T) {
	bot, capture, broadcast := setupHornDeliveryTest(t)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_horn_receipt BEFORE UPDATE OF status ON sect_horn_deliveries WHEN NEW.user_id = 1 AND NEW.status = 'sent' BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	deliverSectHornBroadcast(bot, broadcast.HornID)
	if err := storage.DB.First(&broadcast, broadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.Status != sectHornStatusSending || broadcast.CompletedAt != nil || capture.count(1) != 1 || capture.count(2) != 1 {
		t.Fatalf("in-flight receipt was incorrectly finalized: %+v", broadcast)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_horn_receipt").Error; err != nil {
		t.Fatal(err)
	}
	ageHornAttempt(t, broadcast.HornID)
	deliverSectHornBroadcast(bot, broadcast.HornID)
	deliverSectHornBroadcast(bot, broadcast.HornID)
	if err := storage.DB.First(&broadcast, broadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.Status != sectHornStatusCompleted || broadcast.SuccessCount != 1 || broadcast.UncertainCount != 1 || broadcast.FailedCount != 0 || capture.count(1) != 1 || capture.count(2) != 1 || capture.count(99) != 1 {
		t.Fatalf("recovery duplicated sends or lost the uncertain receipt: %+v", broadcast)
	}
}

func TestHornClaimFailureRetainsUnsentRecipients(t *testing.T) {
	bot, capture, broadcast := setupHornDeliveryTest(t)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_horn_claim BEFORE UPDATE OF status ON sect_horn_deliveries WHEN NEW.status = 'sending' BEGIN SELECT RAISE(ABORT, 'claim unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	deliverSectHornBroadcast(bot, broadcast.HornID)
	if capture.count(1) != 0 || capture.count(2) != 0 {
		t.Fatal("sent a message before its durable claim")
	}
	if _, completed, err := finalizeSectHornBroadcast(broadcast.HornID, time.Now()); err != nil || completed {
		t.Fatalf("unfinished deliveries were marked complete: %t %v", completed, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_horn_claim").Error; err != nil {
		t.Fatal(err)
	}
	ageHornAttempt(t, broadcast.HornID)
	deliverSectHornBroadcast(bot, broadcast.HornID)
	if err := storage.DB.First(&broadcast, broadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.Status != sectHornStatusCompleted || broadcast.SuccessCount != 2 || capture.count(1) != 1 || capture.count(2) != 1 {
		t.Fatalf("pending recipients did not recover: %+v", broadcast)
	}
}

func TestHornSeparatesRejectedAndUncertainSends(t *testing.T) {
	bot, capture, broadcast := setupHornDeliveryTest(t)
	capture.Lock()
	capture.codes[1], capture.codes[2] = 403, 502
	capture.Unlock()
	deliverSectHornBroadcast(bot, broadcast.HornID)
	if err := storage.DB.First(&broadcast, broadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.Status != sectHornStatusCompleted || broadcast.SuccessCount != 0 || broadcast.FailedCount != 1 || broadcast.UncertainCount != 1 {
		t.Fatalf("rejected and uncertain sends were conflated: %+v", broadcast)
	}
}

func TestHornStatisticsFailureDoesNotEndBroadcast(t *testing.T) {
	bot, _, broadcast := setupHornDeliveryTest(t)
	if err := storage.DB.Model(&broadcast).Update("status", sectHornStatusSending).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Callback().Row().Before("gorm:row").Register("test:horn_statistics_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "sect_horn_deliveries" {
			tx.AddError(fmt.Errorf("temporary statistics failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer storage.DB.Callback().Row().Remove("test:horn_statistics_failure")
	completeSectHornBroadcast(bot, broadcast.HornID)
	if err := storage.DB.First(&broadcast, broadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.Status != sectHornStatusSending || broadcast.CompletedAt != nil || !strings.Contains(broadcast.LastError, "temporary statistics failure") {
		t.Fatalf("a temporary statistics error ended the broadcast: %+v", broadcast)
	}
}

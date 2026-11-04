package books

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestStaleNeedInfoSnapshotCannotCancelRefreshedRequest(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "patrol.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	old := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = old; conn.Close() })
	if err := db.AutoMigrate(&model.BookRequest{}, &model.BookRequestLog{}, &model.User{}, &model.PointTransaction{}, &model.AuditLog{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer srv.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Create(&model.User{TelegramID: 1, Username: "reader", Points: 50}).Error; err != nil {
		t.Fatal(err)
	}
	request := model.BookRequest{UserID: 1, Status: BookRequestStatusNeedInfo, LastActionAt: now.Add(-49 * time.Hour), CostPaid: 20}
	if err := db.Create(&request).Error; err != nil {
		t.Fatal(err)
	}
	// The patrol snapshot predates a new request for information by the admin.
	if err := db.Model(&request).Update("last_action_at", now).Error; err != nil {
		t.Fatal(err)
	}
	autoCancelStaleNeedInfoBookRequest(bot, request, now)
	if err := db.First(&request, request.ID).Error; err != nil || request.Status != BookRequestStatusNeedInfo {
		t.Fatalf("fresh request cancelled by stale snapshot: %+v %v", request, err)
	}
	var logs int64
	db.Model(&model.BookRequestLog{}).Count(&logs)
	if logs != 0 {
		t.Fatal("stale cancellation wrote a refund/action log")
	}
	var user model.User
	if err := db.First(&user, "telegram_id = ?", 1).Error; err != nil || user.Points != 50 {
		t.Fatalf("fresh request was refunded: %+v %v", user, err)
	}
	// A genuinely stale request still completes the normal timeout path.
	if err := db.Model(&request).Update("last_action_at", now.Add(-49*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	autoCancelStaleNeedInfoBookRequest(bot, request, now)
	if err := db.First(&request, request.ID).Error; err != nil || request.Status != BookRequestStatusCancelled {
		t.Fatalf("genuine timeout was not cancelled: %+v %v", request, err)
	}
	if err := db.First(&user, "telegram_id = ?", 1).Error; err != nil || user.Points != 70 {
		t.Fatalf("genuine timeout was not refunded: %+v %v", user, err)
	}
}

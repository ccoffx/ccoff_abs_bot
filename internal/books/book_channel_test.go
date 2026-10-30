package books

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

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBookPreviewAndDeliveryFreezeChannel(t *testing.T) {
	oldDB, oldConfig, oldABS := storage.DB, config.AppConfig, abs.DefaultClient
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "book.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { storage.DB = oldDB; config.AppConfig = oldConfig; abs.DefaultClient = oldABS; _ = conn.Close() })
	storage.DB = db
	config.AppConfig = &config.Config{NoticeGroupID: -100300, AnnouncementChannelID: -100200}
	if err = db.AutoMigrate(&model.BookRequestAnnouncementPreviewCandidate{}, &model.BookRequestAnnouncementDelivery{}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var text string
	var target int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		mu.Lock()
		text = r.Form.Get("text")
		target, _ = strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":3,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	abs.DefaultClient = &abs.AbsClient{BaseURL: server.URL, HttpClient: server.Client()}
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	candidate := BookAnnouncementCandidate{ItemID: "book", Title: "测试书籍", LibraryName: "测试库", RecentAt: time.Now()}
	if err = sendBookAnnouncementPreview(bot, 1, 1, candidate); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	preview := text
	mu.Unlock()
	if !strings.Contains(preview, "-100200") {
		t.Fatal("preview did not identify destination")
	}
	var row model.BookRequestAnnouncementPreviewCandidate
	if err = db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	// Drop the memory cache to exercise the persisted target after restart.
	bookRequestAnnouncementPreviewItems.Delete(row.Token)
	config.AppConfig.AnnouncementChannelID = -100999
	saved, ok := resolveBookAnnouncementPreviewCandidate(1, row.Token, time.Now())
	if !ok || saved.AnnouncementTargetID != -100200 {
		t.Fatal("preview target not durable")
	}
	if _, err = claimBookRequestAnnouncementDelivery(1, saved.ItemID, saved.AnnouncementTargetID, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := publishBookRequestGroupAnnouncement(bot, model.BookRequest{Model: gorm.Model{ID: 1}}, saved); err != nil {
		t.Fatal(err.Err)
	}
	mu.Lock()
	sentTarget := target
	mu.Unlock()
	if sentTarget != -100200 {
		t.Fatal("publication moved to new config instead of previewed channel")
	}
	if err = markBookRequestAnnouncementDelivery(1, bookAnnouncementDeliveryFailed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = claimBookRequestAnnouncementDelivery(1, saved.ItemID, -100999, 1, time.Now()); err == nil {
		t.Fatal("retry rewrote frozen destination")
	}
}

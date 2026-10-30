package announcements

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type apiCapture struct {
	mu                     sync.Mutex
	sends                  map[string]int
	texts                  map[string]string
	edits                  int
	failChannel, failGroup int
	postAllowed            bool
	channelKind            string
	linkedChatID           int64
}

func announcementFixture(t *testing.T) (*tgbotapi.BotAPI, *apiCapture) {
	t.Helper()
	oldDB, oldConfig := storage.DB, config.AppConfig
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "announcements.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	if err = db.AutoMigrate(&model.AnnouncementDelivery{}, &model.AuditLog{}, &model.User{}, &model.AutoDeleteMsg{}, &model.LotteryActivity{}); err != nil {
		t.Fatal(err)
	}
	storage.DB = db
	config.AppConfig = &config.Config{AnnouncementChannelID: -100200, NoticeGroupID: -100300, AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { storage.DB = oldDB; config.AppConfig = oldConfig; _ = conn.Close() })
	cap := &apiCapture{sends: map[string]int{}, texts: map[string]string{}, postAllowed: true, channelKind: "channel"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		id := r.Form.Get("chat_id")
		cap.mu.Lock()
		defer cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if method == "getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
			return
		}
		result := any(true)
		switch method {
		case "getChat":
			result = map[string]any{"id": -100200, "type": cap.channelKind, "username": "public_channel", "linked_chat_id": cap.linkedChatID}
		case "getChatMember":
			result = map[string]any{"status": "administrator", "can_post_messages": cap.postAllowed}
		case "sendMessage":
			cap.sends[id]++
			cap.texts[id] = r.Form.Get("text")
			fail := 0
			if id == "-100200" {
				fail = cap.failChannel
				cap.failChannel = 0
			}
			if id == "-100300" {
				fail = cap.failGroup
				cap.failGroup = 0
			}
			if fail != 0 {
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"simulated send failure"}`, fail)
				return
			}
			chatID, _ := strconv.ParseInt(id, 10, 64)
			result = map[string]any{"message_id": cap.sends[id], "chat": map[string]any{"id": chatID, "type": "channel"}}
		case "editMessageText":
			cap.edits++
			result = map[string]any{"message_id": 1, "chat": map[string]any{"id": -100200, "type": "channel"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, cap
}
func queueTest(t *testing.T, key string, reminder bool) model.AnnouncementDelivery {
	t.Helper()
	g := int64(0)
	if reminder {
		g = -100300
	}
	d := model.AnnouncementDelivery{Key: key, Kind: "service", TargetChatID: -100200, ReminderGroupID: g, ActorID: 1, Text: "📣 测试公告\n公开内容"}
	if err := Queue(storage.DB, d); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Where("key = ?", key).First(&d).Error; err != nil {
		t.Fatal(err)
	}
	return d
}
func loadDelivery(t *testing.T, id uint) model.AnnouncementDelivery {
	t.Helper()
	var d model.AnnouncementDelivery
	if err := storage.DB.First(&d, id).Error; err != nil {
		t.Fatal(err)
	}
	return d
}
func TestChannelValidationAndLinks(t *testing.T) {
	bot, c := announcementFixture(t)
	if err := Validate(bot); err != nil {
		t.Fatal(err)
	}
	c.postAllowed = false
	if Validate(bot) == nil {
		t.Fatal("missing post permission accepted")
	}
	c.postAllowed = true
	c.channelKind = "supergroup"
	if Validate(bot) == nil {
		t.Fatal("group accepted as announcement channel")
	}
	if got := MessageLink(-100123456, "", 12); got != "https://t.me/c/123456/12" {
		t.Fatal(got)
	}
	if got := MessageLink(-100123456, "public", 12); got != "https://t.me/public/12" {
		t.Fatal(got)
	}
}
func TestPublishAndReminderPersistIndependently(t *testing.T) {
	bot, c := announcementFixture(t)
	d := queueTest(t, "service:test", true)
	c.failGroup = 400
	Dispatch(bot)
	r := loadDelivery(t, d.ID)
	if r.State != "sent" || r.MessageID == 0 || r.ReminderState != "failed" {
		t.Fatalf("bad states: %+v", r)
	}
	if c.sends["-100200"] != 1 || c.sends["-100300"] != 1 {
		t.Fatal("publication/reminder missing")
	}
	if err := Resolve(bot, d.ID, "频道提醒重试", 0, 1); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	r = loadDelivery(t, d.ID)
	if r.ReminderState != "sent" || c.sends["-100200"] != 1 || c.sends["-100300"] != 2 {
		t.Fatal("reminder retry duplicated main post")
	}
	if !strings.Contains(c.texts["-100300"], "https://t.me/public_channel/1") {
		t.Fatal("group reminder lacks channel message link")
	}
	if err := Queue(storage.DB, model.AnnouncementDelivery{Key: d.Key, TargetChatID: -100999, Text: "changed"}); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if c.sends["-100200"] != 1 || loadDelivery(t, d.ID).TargetChatID != -100200 {
		t.Fatal("queue replay changed destination or duplicated post")
	}
}

func TestLinkedDiscussionGroupDoesNotReceiveDuplicateReminder(t *testing.T) {
	for _, linked := range []int64{-100300, -100400} {
		t.Run(strconv.FormatInt(linked, 10), func(t *testing.T) {
			bot, c := announcementFixture(t)
			c.linkedChatID = linked
			d := queueTest(t, "linked:discussion", true)
			Dispatch(bot)
			Dispatch(bot)
			r := loadDelivery(t, d.ID)
			if r.State != "sent" || c.sends["-100200"] != 1 {
				t.Fatal("channel publication missing or duplicated")
			}
			if linked == -100300 {
				if r.ReminderState != "linked" || r.ReminderMessageID != 0 || c.sends["-100300"] != 0 {
					t.Fatalf("linked group received an extra reminder: %+v", r)
				}
			} else if r.ReminderState != "sent" || c.sends["-100300"] != 1 {
				t.Fatal("unlinked target group lost its reminder")
			}
		})
	}
}
func TestUncertainSendRequiresExplicitResolution(t *testing.T) {
	bot, c := announcementFixture(t)
	d := queueTest(t, "uncertain", true)
	c.failChannel = 502
	Dispatch(bot)
	r := loadDelivery(t, d.ID)
	if r.State != "uncertain" || c.sends["-100300"] != 0 {
		t.Fatalf("unknown send must stop: %+v", r)
	}
	Dispatch(bot)
	if c.sends["-100200"] != 1 {
		t.Fatal("unknown send retried")
	}
	if err := Resolve(bot, d.ID, "频道公告重试", 0, 1); err == nil {
		t.Fatal("uncertain send bypassed manual absence check")
	}
	if err := Resolve(bot, d.ID, "频道公告已发", 22, 1); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	r = loadDelivery(t, d.ID)
	if r.State != "sent" || r.MessageID != 22 || c.sends["-100200"] != 1 || c.sends["-100300"] != 1 {
		t.Fatal("manual confirmation should only release reminder")
	}
}
func TestRestartConvertsSendsToUncertainAndRetriesEdits(t *testing.T) {
	bot, c := announcementFixture(t)
	d := queueTest(t, "restart", false)
	if err := storage.DB.Model(&d).Update("state", "sending").Error; err != nil {
		t.Fatal(err)
	}
	if err := Recover(storage.DB); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if loadDelivery(t, d.ID).State != "uncertain" || c.sends["-100200"] != 0 {
		t.Fatal("restarted send must not repeat")
	}
	if err := Resolve(bot, d.ID, "频道公告未发", 0, 1); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if err := QueueEdit(storage.DB, "restart:edit", d.Key, "service_edit", "新状态", 1, false); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.AnnouncementDelivery{}).Where("key = ?", "restart:edit").Update("state", "sending").Error; err != nil {
		t.Fatal(err)
	}
	if err := Recover(storage.DB); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if c.sends["-100200"] != 1 || c.edits != 1 {
		t.Fatal("edit recovery must edit, not publish again")
	}
}
func TestDefiniteFailureRetryAndExhaustedRowsDoNotStarveQueue(t *testing.T) {
	bot, c := announcementFixture(t)
	d := queueTest(t, "retry", false)
	c.failChannel = 400
	Dispatch(bot)
	if loadDelivery(t, d.ID).State != "failed" {
		t.Fatal("definite API rejection must be retryable")
	}
	if err := Resolve(bot, d.ID, "频道公告重试", 0, 1); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if loadDelivery(t, d.ID).State != "sent" || c.sends["-100200"] != 2 {
		t.Fatal("confirmed failure did not retry")
	}
	for i := 0; i < 35; i++ {
		d := queueTest(t, fmt.Sprintf("exhausted:%d", i), false)
		storage.DB.Model(&d).Updates(map[string]any{"state": "failed", "attempts": 3, "retry_at": time.Time{}})
	}
	fresh := queueTest(t, "fresh", false)
	Dispatch(bot)
	if loadDelivery(t, fresh.ID).State != "sent" {
		t.Fatal("exhausted deliveries starved new announcement")
	}
}

func TestSentButAuditFinalizationFailedDoesNotRepublish(t *testing.T) {
	bot, c := announcementFixture(t)
	d := queueTest(t, "finalization", true)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_announcement_sent BEFORE INSERT ON audit_logs WHEN NEW.action = 'CHANNEL_ANNOUNCEMENT_SENT' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	r := loadDelivery(t, d.ID)
	if r.State != "uncertain" || r.MessageID != 1 || c.sends["-100300"] != 0 {
		t.Fatal("sent-but-unrecorded post must stop before reminder")
	}
	Dispatch(bot)
	if c.sends["-100200"] != 1 {
		t.Fatal("finalization failure duplicated publication")
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_announcement_sent").Error; err != nil {
		t.Fatal(err)
	}
	if err := Resolve(bot, d.ID, "频道公告已发", 1, 1); err != nil {
		t.Fatal(err)
	}
	Dispatch(bot)
	if c.sends["-100200"] != 1 || c.sends["-100300"] != 1 {
		t.Fatal("recovery republished content")
	}
}
func TestEditsWaitingForUnknownParentsDoNotStarveNewPosts(t *testing.T) {
	bot, c := announcementFixture(t)
	for i := 0; i < 35; i++ {
		d := queueTest(t, fmt.Sprintf("unknown-parent:%d", i), false)
		if err := storage.DB.Model(&d).Update("state", "uncertain").Error; err != nil {
			t.Fatal(err)
		}
		if err := QueueEdit(storage.DB, d.Key+":edit", d.Key, "edit", "updated", 1, false); err != nil {
			t.Fatal(err)
		}
	}
	fresh := queueTest(t, "ready", false)
	Dispatch(bot)
	if loadDelivery(t, fresh.ID).State != "sent" || c.sends["-100200"] != 1 {
		t.Fatal("waiting edits blocked a ready post")
	}
}

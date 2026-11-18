package telegram

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRedPacketNoticeRecoveryAndLifecycle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "notices.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previous; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	packet := model.RedPacket{ID: "FS-recovered", NoticeChatID: -100, TotalPoints: 300, Count: 30, LeftPoints: 300, LeftCount: 30}
	if err := db.Create(&packet).Error; err != nil {
		t.Fatal(err)
	}
	sends, fail := 0, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		sends++
		r.ParseForm()
		if !strings.Contains(r.Form.Get("text"), "沾仙气") {
			t.Error("missing claim command")
		}
		if fail {
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"retry later"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100,"type":"supergroup"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, 1)
	if sends != 0 {
		t.Fatal("sent announcement to private chat")
	}
	SyncRedPacketNotices(bot, -100)
	fail = false
	db.Model(&model.RedPacket{}).Where("id = ?", packet.ID).Update("notice_retry_at", time.Now().Add(-time.Second))
	SyncRedPacketNotices(bot, -100)
	SyncRedPacketNotices(bot, -100)
	if sends != 2 {
		t.Fatalf("expected failed send and one recovery, got %d", sends)
	}
	var record model.AutoDeleteMsg
	if err := db.Where("packet_id = ?", packet.ID).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.MessageID != 42 {
		t.Fatal("missing cleanup receipt")
	}
	active, err := redPacketNoticeActive(packet.ID)
	if err != nil || !active {
		t.Fatalf("active packet must retain announcement: %v", err)
	}
	if err := db.Model(&packet).Updates(map[string]any{"left_count": 0, "left_points": 0, "is_finished": true}).Error; err != nil {
		t.Fatal(err)
	}
	active, err = redPacketNoticeActive(packet.ID)
	if err != nil || active {
		t.Fatalf("finished packet must allow cleanup: %v", err)
	}
}

func TestRedPacketNoticeRegistrationFailureCompensation(t *testing.T) {
	for _, target := range []string{"red_packets", "auto_delete_msgs"} {
		t.Run(target, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "failure.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			previous := storage.DB
			storage.DB = db
			conn, _ := db.DB()
			pendingRedPacketNoticeCleanup = map[string]model.AutoDeleteMsg{}
			t.Cleanup(func() {
				storage.DB = previous
				conn.Close()
				pendingRedPacketNoticeCleanup = map[string]model.AutoDeleteMsg{}
			})
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
				t.Fatal(err)
			}
			packet := model.RedPacket{ID: "FS-failed", NoticeChatID: -100, TotalPoints: 300, Count: 30, LeftPoints: 300, LeftCount: 30}
			if err := db.Create(&packet).Error; err != nil {
				t.Fatal(err)
			}
			operation := "UPDATE"
			condition := " WHEN NEW.notice_message_id > 0"
			if target == "auto_delete_msgs" {
				operation = "INSERT"
				condition = ""
			}
			if err := db.Exec(fmt.Sprintf("CREATE TRIGGER reject_registration BEFORE %s ON %s%s BEGIN SELECT RAISE(ABORT, 'registration failed'); END", operation, target, condition)).Error; err != nil {
				t.Fatal(err)
			}
			sends, deletes, rejectDelete := 0, 0, true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
				case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
					deletes++
					if rejectDelete {
						fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"temporary failure"}`)
					} else {
						fmt.Fprint(w, `{"ok":true,"result":true}`)
					}
				default:
					sends++
					fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":-100,"type":"supergroup"}}}`, sends)
				}
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			SyncRedPacketNotices(bot, -100)
			SyncRedPacketNotices(bot, -100)
			if sends != 1 || deletes != 2 {
				t.Fatalf("must retry deletion without republishing: sends=%d deletes=%d", sends, deletes)
			}
			// When persistence is available, the orphan also survives a process restart.
			if target == "red_packets" {
				pendingRedPacketNoticeCleanup = map[string]model.AutoDeleteMsg{}
				SyncRedPacketNotices(bot, -100)
				if sends != 1 {
					t.Fatal("persisted compensation did not suppress duplicate")
				}
				var orphan model.AutoDeleteMsg
				if err := db.First(&orphan).Error; err != nil {
					t.Fatal(err)
				}
				retain, err := retainRedPacketNotice(orphan, time.Now())
				if err != nil || retain {
					t.Fatal("unregistered announcement must not be retained")
				}
				if err := completeRedPacketNoticeCleanup(orphan); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Exec("DROP TRIGGER reject_registration").Error; err != nil {
				t.Fatal(err)
			}
			rejectDelete = false
			SyncRedPacketNotices(bot, -100)
			SyncRedPacketNotices(bot, -100)
			if sends != 2 {
				t.Fatalf("expected exactly one replacement after cleanup, got %d", sends)
			}
		})
	}
}

func TestRedPacketNoticeRotatesBeforeDeletionDeadline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "rotation.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previous; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	packet := model.RedPacket{ID: "FS-old", NoticeChatID: -100, NoticeMessageID: 42, LeftCount: 30, LeftPoints: 300}
	record := model.AutoDeleteMsg{ChatID: -100, MessageID: 42, PacketID: packet.ID, DeleteAt: now.Add(-redPacketNoticeMaxAge + botMessageAutoDeleteDelay)}
	if err := db.Create(&packet).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	retain, err := retainRedPacketNotice(record, now.Add(-time.Second))
	if err != nil || !retain {
		t.Fatal("active recent announcement must remain")
	}
	retain, err = retainRedPacketNotice(record, now)
	if err != nil || retain {
		t.Fatal("active announcement must rotate at 46h")
	}
	if err := completeRedPacketNoticeCleanup(record); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
		t.Fatal(err)
	}
	if packet.NoticeMessageID != 0 || packet.LeftCount != 30 {
		t.Fatal("rotation must clear receipt without expiring packet")
	}
	// Retrying an old cleanup must not clear a newer publication.
	if err := db.Model(&packet).Update("notice_message_id", 43).Error; err != nil {
		t.Fatal(err)
	}
	if err := completeRedPacketNoticeCleanup(record); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
		t.Fatal(err)
	}
	if packet.NoticeMessageID != 43 {
		t.Fatal("stale cleanup cleared new receipt")
	}
}

func TestRedPacketNoticeUnknownSendIsNotRetried(t *testing.T) {
	for _, response := range []string{`{"ok":false,"error_code":500,"description":"unknown outcome"}`, `{"ok":true,"result":`, `{"ok":true,"result":{}}`, "disconnect"} {
		t.Run(response, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "unknown.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			previous := storage.DB
			storage.DB = db
			conn, _ := db.DB()
			t.Cleanup(func() { storage.DB = previous; conn.Close() })
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
				t.Fatal(err)
			}
			packet := model.RedPacket{ID: "FS-unknown", NoticeChatID: -200, LeftCount: 30, LeftPoints: 300}
			if err := db.Create(&packet).Error; err != nil {
				t.Fatal(err)
			}
			sends := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				// The external side effect may already have happened before this response.
				sends++
				var reserved model.RedPacket
				if err := db.First(&reserved, "id = ?", packet.ID).Error; err != nil || reserved.NoticeSendState != "uncertain" {
					t.Error("uncertainty must be durable before send")
				}
				if response == "disconnect" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				}
				fmt.Fprint(w, response)
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			SyncRedPacketNotices(bot, -200)
			SyncRedPacketNotices(bot, -200)
			SyncRedPacketNotices(bot, 0)
			if sends != 1 {
				t.Fatalf("unknown send was retried %d times", sends)
			}
			if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
				t.Fatal(err)
			}
			if packet.NoticeSendState != "uncertain" || packet.NoticeChatID != -200 {
				t.Fatal("unknown result lost durable state or destination")
			}
		})
	}
}

func TestFallbackRedPacketNoticeResumesAfterRotation(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fallback.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previous; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	packet := model.RedPacket{ID: "FS-fallback", NoticeChatID: -200, LeftCount: 30, LeftPoints: 300}
	if err := db.Create(&packet).Error; err != nil {
		t.Fatal(err)
	}
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		r.ParseForm()
		if r.Form.Get("chat_id") != "-200" {
			t.Error("announcement changed fallback destination")
		}
		sends++
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":-200,"type":"supergroup"}}}`, sends)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, -200)
	var receipt model.AutoDeleteMsg
	if err := db.First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if retain, err := retainRedPacketNotice(receipt, time.Now().Add(redPacketNoticeMaxAge)); err != nil || retain {
		t.Fatal("old announcement did not become eligible for rotation")
	}
	if err := completeRedPacketNoticeCleanup(receipt); err != nil {
		t.Fatal(err)
	}
	// Periodic sweep has no configured default group and no new burst event.
	SyncRedPacketNotices(bot, 0)
	SyncRedPacketNotices(bot, 0)
	if sends != 2 {
		t.Fatalf("expected one fallback replacement, got %d sends", sends)
	}
	if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
		t.Fatal(err)
	}
	if packet.NoticeChatID != -200 || packet.NoticeMessageID != 2 {
		t.Fatal("fallback receipt was not restored")
	}
}

func TestBurstFallbackCannotAssignOtherPackets(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "targets.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig := storage.DB, config.AppConfig
	storage.DB = db
	config.AppConfig = &config.Config{}
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previousDB; config.AppConfig = previousConfig; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}, &model.SystemConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX test_pool_key ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"HB-unaddressed", "HB-boss", "FS-old"} {
		packet := model.RedPacket{ID: id, LeftCount: 3, LeftPoints: 30}
		if err := storage.CreateRedPacketInTx(db, &packet); err != nil {
			t.Fatal(err)
		}
	}
	// An ordinary packet captures the configured official group at creation.
	config.AppConfig = &config.Config{NoticeGroupID: -100}
	ordinary := model.RedPacket{ID: "HB-official", LeftCount: 3, LeftPoints: 30}
	if err := storage.CreateRedPacketInTx(db, &ordinary); err != nil {
		t.Fatal(err)
	}
	config.AppConfig = &config.Config{}
	// Each transaction fixes only its own burst's target, including multi-bursts.
	if _, _, err := storage.AddPointsToFusionPool(600, -200); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.AddPointsToFusionPool(300, -300); err != nil {
		t.Fatal(err)
	}
	sends := 0
	groups := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		r.ParseForm()
		group := r.Form.Get("chat_id")
		groups[group]++
		sends++
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":%s,"type":"supergroup"}}}`, sends, group)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	NotifyFusionPoolBurst(bot, -999, "unrelated event")
	if sends != 4 || groups["-100"] != 1 || groups["-200"] != 2 || groups["-300"] != 1 || groups["-999"] != 0 {
		t.Fatalf("misrouted announcements: %v", groups)
	}
	var unaddressed int64
	if err := db.Model(&model.RedPacket{}).Where("id IN ? AND COALESCE(notice_chat_id,0) = 0 AND COALESCE(notice_message_id,0) = 0", []string{"HB-unaddressed", "HB-boss", "FS-old"}).Count(&unaddressed).Error; err != nil || unaddressed != 3 {
		t.Fatalf("unrelated packets acquired destinations: count=%d err=%v", unaddressed, err)
	}
}

func TestFailedAnnouncementDeletionPreservesPublication(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		cleared        bool
	}{
		{"expired", `{"ok":false,"error_code":400,"description":"Bad Request: message can't be deleted"}`, false},
		{"permission", `{"ok":false,"error_code":403,"description":"not enough rights to delete messages"}`, false},
		{"temporary", `{"ok":false,"error_code":500,"description":"temporary failure"}`, false},
		{"missing", `{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`, true},
		{"deleted", `{"ok":true,"result":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "delete.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			previous := storage.DB
			storage.DB = db
			conn, _ := db.DB()
			t.Cleanup(func() { storage.DB = previous; conn.Close() })
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
				t.Fatal(err)
			}
			packet := model.RedPacket{ID: "FS-delete", NoticeChatID: -100, NoticeMessageID: 42, NoticeSendState: "sent", LeftCount: 30, LeftPoints: 300}
			record := model.AutoDeleteMsg{ChatID: -100, MessageID: 42, PacketID: packet.ID, DeleteAt: time.Now().Add(-49 * time.Hour)}
			if err := db.Create(&packet).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&record).Error; err != nil {
				t.Fatal(err)
			}
			sends := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/getMe"):
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
				case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
					fmt.Fprint(w, tc.response)
				default:
					sends++
					fmt.Fprint(w, `{"ok":true,"result":{"message_id":43,"chat":{"id":-100,"type":"supergroup"}}}`)
				}
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = deleteAutoDeleteMessage(bot, record)
			if (err == nil) != tc.cleared {
				t.Fatalf("incorrect cleanup outcome: %v", err)
			}
			if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
				t.Fatal(err)
			}
			var receipts int64
			db.Model(&model.AutoDeleteMsg{}).Count(&receipts)
			if tc.cleared {
				if packet.NoticeMessageID != 0 || receipts != 0 {
					t.Fatal("confirmed deletion did not release receipt")
				}
			} else {
				if packet.NoticeMessageID != 42 || packet.NoticeSendState != "sent" || receipts != 1 {
					t.Fatal("failed deletion lost publication receipt")
				}
			}
			SyncRedPacketNotices(bot, 0)
			if tc.cleared && sends != 1 || !tc.cleared && sends != 0 {
				t.Fatalf("incorrect republication after delete: %d", sends)
			}
		})
	}
}

func TestDefiniteRejectionRecoveryRetriesDatabaseFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "rejection.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	pendingRedPacketNoticeRejections = map[string]redPacketNoticeRejection{}
	t.Cleanup(func() {
		storage.DB = previous
		conn.Close()
		pendingRedPacketNoticeRejections = map[string]redPacketNoticeRejection{}
	})
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	packet := model.RedPacket{ID: "FS-rejection", NoticeChatID: -100, LeftCount: 30, LeftPoints: 300}
	if err := db.Create(&packet).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_result BEFORE UPDATE ON red_packets WHEN NEW.notice_send_state IN ('rejected', 'rate_limited') BEGIN SELECT RAISE(ABORT, 'database locked'); END`).Error; err != nil {
		t.Fatal(err)
	}
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		sends++
		if sends == 1 {
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100,"type":"supergroup"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, 0)
	SyncRedPacketNotices(bot, 0)
	if sends != 1 || len(pendingRedPacketNoticeRejections) != 1 {
		t.Fatal("failed result write was not retained for retry")
	}
	if err := db.Exec("DROP TRIGGER reject_result").Error; err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, 0)
	db.Model(&model.RedPacket{}).Where("id = ?", packet.ID).Update("notice_retry_at", time.Now().Add(-time.Second))
	SyncRedPacketNotices(bot, 0)
	if sends != 2 || len(pendingRedPacketNoticeRejections) != 0 {
		t.Fatalf("definite failure did not recover exactly once: sends=%d", sends)
	}
	if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
		t.Fatal(err)
	}
	if packet.NoticeSendState != "sent" || packet.NoticeMessageID != 42 {
		t.Fatal("retry did not persist successful receipt")
	}
}

func TestLegacyNoticeTransitionWaitsForConfirmedCleanup(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "transition.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previous; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}, &model.WorldBossEvent{}, &model.SystemConfig{}); err != nil {
		t.Fatal(err)
	}
	// Also cover databases where the previous target-only migration already ran.
	if err := db.Create(&model.SystemConfig{Key: "migration_red_packet_notice_targets_v1", Value: "1"}).Error; err != nil {
		t.Fatal(err)
	}
	packets := []model.RedPacket{{ID: "HB-legacy", LeftCount: 3, LeftPoints: 30}, {ID: "FS-previous-migration", NoticeChatID: -100, LeftCount: 3, LeftPoints: 30}}
	if err := db.Create(&packets).Error; err != nil {
		t.Fatal(err)
	}
	old := model.AutoDeleteMsg{ChatID: -100, MessageID: 41, DeleteAt: time.Now().Add(5 * time.Minute)}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.MigrateLegacyRedPacketNoticeTargets(db, -100); err != nil {
		t.Fatal(err)
	}
	var migrated []model.RedPacket
	if err := db.Find(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	for _, packet := range migrated {
		if packet.NoticeLegacyCleanupMaxID != old.ID {
			t.Fatal("migration did not persist cleanup barrier")
		}
	}
	sends, rejectDelete := 0, true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			if rejectDelete {
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"message can't be deleted"}`)
			} else {
				fmt.Fprint(w, `{"ok":true,"result":true}`)
			}
		default:
			sends++
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":-100,"type":"supergroup"}}}`, 100+sends)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, 0)
	if sends != 0 {
		t.Fatal("upgrade duplicated a legacy announcement")
	}
	if err := deleteAutoDeleteMessage(bot, old); err == nil {
		t.Fatal("failed legacy deletion released transition")
	}
	SyncRedPacketNotices(bot, 0)
	if sends != 0 {
		t.Fatal("deletion error allowed duplicate publication")
	}
	// Later commands must not prolong the migration-time barrier.
	later := model.AutoDeleteMsg{ChatID: -100, MessageID: 42, DeleteAt: time.Now().Add(10 * time.Minute)}
	if err := db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}
	rejectDelete = false
	if err := deleteAutoDeleteMessage(bot, old); err != nil {
		t.Fatal(err)
	}
	SyncRedPacketNotices(bot, 0)
	SyncRedPacketNotices(bot, 0)
	if sends != 2 {
		t.Fatalf("expected exactly one announcement per remaining packet, got %d", sends)
	}
}

func TestNoticeReservationRechecksPacketAvailability(t *testing.T) {
	for _, change := range []string{"is_finished", "left_count", "left_points"} {
		t.Run(change, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "reservation.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			previous := storage.DB
			storage.DB = db
			conn, _ := db.DB()
			t.Cleanup(func() { storage.DB = previous; conn.Close() })
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
				t.Fatal(err)
			}
			packets := []model.RedPacket{{ID: "HB-first", NoticeChatID: -100, LeftCount: 3, LeftPoints: 30, CreatedAt: time.Now().Add(-time.Minute)}, {ID: "HB-waiting", NoticeChatID: -100, LeftCount: 3, LeftPoints: 30, CreatedAt: time.Now()}}
			if err := db.Create(&packets).Error; err != nil {
				t.Fatal(err)
			}
			sends := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				sends++
				var value any = 0
				if change == "is_finished" {
					value = true
				}
				// The entire packet list was read before the first send. Deplete the
				// queued packet while that request is in flight, before its reservation.
				if err := db.Model(&model.RedPacket{}).Where("id = ?", "HB-waiting").Update(change, value).Error; err != nil {
					t.Error(err)
				}
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100,"type":"supergroup"}}}`)
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			SyncRedPacketNotices(bot, 0)
			if sends != 1 {
				t.Fatalf("depleted queued packet was announced: sends=%d", sends)
			}
			var waiting model.RedPacket
			if err := db.First(&waiting, "id = ?", "HB-waiting").Error; err != nil {
				t.Fatal(err)
			}
			if waiting.NoticeSendState != "" || waiting.NoticeMessageID != 0 {
				t.Fatal("ineligible packet was reserved")
			}
		})
	}
}

func TestNoticeRetryPolicyIsPersistedAndHonorsFloodWait(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
		delay int
	}{{429, "rate_limited", 3600}, {408, "rejected", 60}, {400, "failed", 0}, {403, "failed", 0}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "policy.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			previous := storage.DB
			storage.DB = db
			conn, _ := db.DB()
			t.Cleanup(func() { storage.DB = previous; conn.Close() })
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}); err != nil {
				t.Fatal(err)
			}
			packet := model.RedPacket{ID: "HB-policy", NoticeChatID: -100, LeftCount: 3, LeftPoints: 30}
			if err := db.Create(&packet).Error; err != nil {
				t.Fatal(err)
			}
			sends := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				sends++
				if sends == 1 {
					fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"rejected","parameters":{"retry_after":%d}}`, tc.code, tc.delay)
					return
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":-100,"type":"supergroup"}}}`, sends)
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			SyncRedPacketNotices(bot, 0)
			if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
				t.Fatal(err)
			}
			if packet.NoticeSendState != tc.state {
				t.Fatalf("state=%s want %s", packet.NoticeSendState, tc.state)
			}
			if tc.delay > 0 && (packet.NoticeRetryAt == nil || packet.NoticeRetryAt.Before(before.Add(time.Duration(tc.delay)*time.Second))) {
				t.Fatal("retry deadline not persisted")
			}
			for i := 0; i < 3; i++ {
				SyncRedPacketNotices(bot, 0)
			}
			if sends != 1 {
				t.Fatal("premature or permanent-error retry")
			}
			if tc.code == 429 {
				// A different packet and destination must also respect the bot's flood wait.
				if err := db.Create(&model.RedPacket{ID: "HB-other", NoticeChatID: -200, LeftCount: 3, LeftPoints: 30}).Error; err != nil {
					t.Fatal(err)
				}
				SyncRedPacketNotices(bot, 0)
				if sends != 1 {
					t.Fatal("another packet bypassed flood wait")
				}
			}
			if tc.delay > 0 {
				if err := db.Model(&packet).Update("notice_retry_at", time.Now().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
				SyncRedPacketNotices(bot, 0)
				expected := 2
				if tc.code == 429 {
					expected = 3
				}
				if sends != expected {
					t.Fatalf("due retry failed: sends=%d want=%d", sends, expected)
				}
			}
		})
	}
}

func TestLegacyBarrierReleaseIsAuditedAndPreservesMessages(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "release.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previous; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	record := model.AutoDeleteMsg{ChatID: -100, MessageID: 41, DeleteAt: time.Now().Add(-49 * time.Hour)}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	packets := []model.RedPacket{{ID: "HB-blocked", NoticeChatID: -100, NoticeLegacyCleanupMaxID: record.ID, LeftCount: 3, LeftPoints: 30}, {ID: "HB-unknown", NoticeChatID: -100, NoticeLegacyCleanupMaxID: record.ID, NoticeSendState: "uncertain", LeftCount: 3, LeftPoints: 30}}
	if err := db.Create(&packets).Error; err != nil {
		t.Fatal(err)
	}
	// Failure to audit must roll back release.
	if err := db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.ReleaseLegacyRedPacketNoticeBarrier(db, "HB-blocked", 99); err == nil {
		t.Fatal("release succeeded without audit")
	}
	var packet model.RedPacket
	db.First(&packet, "id = ?", "HB-blocked")
	if packet.NoticeLegacyCleanupMaxID != record.ID {
		t.Fatal("failed audited release changed barrier")
	}
	if err := db.Exec("DROP TRIGGER reject_audit").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.ReleaseLegacyRedPacketNoticeBarrier(db, "HB-unknown", 99); err == nil {
		t.Fatal("release altered unknown send")
	}
	if err := storage.ReleaseLegacyRedPacketNoticeBarrier(db, "HB-blocked", 99); err != nil {
		t.Fatal(err)
	}
	db.First(&packet, "id = ?", "HB-blocked")
	if packet.NoticeLegacyCleanupMaxID != 0 || packet.NoticeChatID != -100 {
		t.Fatal("release changed wrong fields")
	}
	var count int64
	db.Model(&model.AutoDeleteMsg{}).Count(&count)
	if count != 1 {
		t.Fatal("release erased cleanup evidence")
	}
	db.Model(&model.AuditLog{}).Where("actor_id = ? AND target = ?", 99, "HB-blocked").Count(&count)
	if count != 1 {
		t.Fatal("release missing audit")
	}
	var eligible int64
	db.Model(&model.RedPacket{}).Where("id = ?", "HB-blocked").Where(redPacketLegacyCleanupCompleteSQL).Count(&eligible)
	if eligible != 1 {
		t.Fatal("manual release did not unblock publication")
	}
}

func TestBarrierReleaseCommandRequiresPrivateSuperAdminAndReview(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "command.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig := storage.DB, config.AppConfig
	storage.DB = db
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{99: true}}
	conn, _ := db.DB()
	t.Cleanup(func() { storage.DB = previousDB; config.AppConfig = previousConfig; conn.Close() })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}, &model.AuditLog{}, &model.User{}); err != nil {
		t.Fatal(err)
	}
	packet := model.RedPacket{ID: "HB-review", NoticeChatID: -100, NoticeLegacyCleanupMaxID: 1, LeftCount: 3, LeftPoints: 30}
	if err := db.Create(&packet).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":99,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user           int64
		chatType, text string
	}{{99, "group", "解除红包公告过渡 HB-review 已核查旧公告不存在"}, {1, "private", "解除红包公告过渡 HB-review 已核查旧公告不存在"}, {99, "private", "解除红包公告过渡 HB-review"}} {
		msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 99, Type: tc.chatType}, From: &tgbotapi.User{ID: tc.user}}
		if !HandleRedPacketNoticeRecoveryCommand(bot, msg, tc.text) {
			t.Fatal("command not handled")
		}
		if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
			t.Fatal(err)
		}
		if packet.NoticeLegacyCleanupMaxID != 1 {
			t.Fatal("unreviewed or unauthorized release")
		}
	}
	msg := &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 99, Type: "private"}, From: &tgbotapi.User{ID: 99}}
	HandleRedPacketNoticeRecoveryCommand(bot, msg, "解除红包公告过渡 HB-review 已核查旧公告不存在")
	if err := db.First(&packet, "id = ?", packet.ID).Error; err != nil {
		t.Fatal(err)
	}
	if packet.NoticeLegacyCleanupMaxID != 0 {
		t.Fatal("reviewed admin release failed")
	}
}

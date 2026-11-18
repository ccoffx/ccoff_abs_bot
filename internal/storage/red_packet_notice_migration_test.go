package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLegacyRedPacketNoticeTargetMigration(t *testing.T) {
	for _, official := range []int64{0, -100} {
		t.Run(fmt.Sprint(official), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := db.DB()
			defer conn.Close()
			if err := db.AutoMigrate(&model.RedPacket{}, &model.AutoDeleteMsg{}, &model.WorldBossEvent{}, &model.SystemConfig{}); err != nil {
				t.Fatal(err)
			}
			packets := []model.RedPacket{
				{ID: "HB-legacy"}, {ID: "FS-legacy"},
				{ID: "HB-boss", RefType: "world_boss", RefID: "boss"},
				{ID: "HB-missing-boss", ClaimScope: RedPacketClaimScopeWorldBossParticipant, RefID: "missing"},
				{ID: "FS-receipt"}, {ID: "FS-ambiguous"},
				{ID: "FS-bound", NoticeChatID: -400},
				{ID: "HB-finished", IsFinished: true},
				{ID: "unrecognized"},
				{ID: "FS-uncertain", NoticeSendState: "uncertain"},
			}
			for i := range packets {
				packets[i].LeftCount = 3
				packets[i].LeftPoints = 30
			}
			if err := db.Create(&packets).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.RedPacket{}).Where("id = ?", "HB-legacy").Updates(map[string]any{"notice_chat_id": nil, "notice_send_state": nil}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.WorldBossEvent{BossID: "boss", ChatID: -200}).Error; err != nil {
				t.Fatal(err)
			}
			receipts := []model.AutoDeleteMsg{{PacketID: "FS-receipt", ChatID: -300, MessageID: 42}, {PacketID: "FS-ambiguous", ChatID: -500, MessageID: 43}, {PacketID: "FS-ambiguous", ChatID: -600, MessageID: 44}}
			if err := db.Create(&receipts).Error; err != nil {
				t.Fatal(err)
			}
			// A failed migration rolls back both data and its completion marker.
			if err := db.Exec(`CREATE TRIGGER reject_migration BEFORE UPDATE ON red_packets BEGIN SELECT RAISE(ABORT, 'locked'); END`).Error; err != nil {
				t.Fatal(err)
			}
			if err := MigrateLegacyRedPacketNoticeTargets(db, official); err == nil {
				t.Fatal("migration should fail")
			}
			var markers int64
			db.Model(&model.SystemConfig{}).Where("key = ?", legacyRedPacketNoticeTargetsKey).Count(&markers)
			if markers != 0 {
				t.Fatal("failed migration stamped completion")
			}
			if err := db.Exec("DROP TRIGGER reject_migration").Error; err != nil {
				t.Fatal(err)
			}
			if err := MigrateLegacyRedPacketNoticeTargets(db, official); err != nil {
				t.Fatal(err)
			}
			want := map[string]int64{"HB-legacy": official, "FS-legacy": official, "HB-boss": -200, "HB-missing-boss": 0, "FS-receipt": -300, "FS-ambiguous": 0, "FS-bound": -400, "HB-finished": 0, "unrecognized": 0, "FS-uncertain": official}
			var migrated []model.RedPacket
			if err := db.Find(&migrated).Error; err != nil {
				t.Fatal(err)
			}
			for _, packet := range migrated {
				if packet.NoticeChatID != want[packet.ID] {
					t.Errorf("%s: got target %d want %d", packet.ID, packet.NoticeChatID, want[packet.ID])
				}
				if packet.ID == "FS-receipt" && (packet.NoticeMessageID != 42 || packet.NoticeSendState != "sent") {
					t.Error("existing receipt lost; would cause duplicate")
				}
				if packet.ID == "FS-uncertain" && packet.NoticeSendState != "uncertain" {
					t.Error("migration released an unknown send")
				}
			}
			// Later unaddressed packets must never be captured by startup backfill.
			if err := db.Create(&model.RedPacket{ID: "HB-new", LeftCount: 3, LeftPoints: 30}).Error; err != nil {
				t.Fatal(err)
			}
			if err := MigrateLegacyRedPacketNoticeTargets(db, -999); err != nil {
				t.Fatal(err)
			}
			var later model.RedPacket
			if err := db.First(&later, "id = ?", "HB-new").Error; err != nil {
				t.Fatal(err)
			}
			if later.NoticeChatID != 0 {
				t.Fatal("migration was applied again")
			}
		})
	}
}

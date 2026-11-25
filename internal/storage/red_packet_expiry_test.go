package storage

import (
	"path/filepath"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRedPacketExpirySettlement(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "expiry.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	defer conn.Close()
	old := DB
	DB = db
	t.Cleanup(func() { DB = old })
	if err := db.AutoMigrate(&model.RedPacket{}, &model.SystemConfig{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX test_pool_key ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	packets := []model.RedPacket{
		{ID: "FS-old", LeftPoints: 300, LeftCount: 30, CreatedAt: now.Add(-25 * time.Hour)},
		{ID: "HB-boss", LeftPoints: 35, LeftCount: 2, CreatedAt: now.Add(-RedPacketLifetime)},
		{ID: "HB-partial", LeftPoints: 12, LeftCount: 1, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "HB-fresh", LeftPoints: 50, LeftCount: 3, CreatedAt: now.Add(-RedPacketLifetime + time.Second)},
		{ID: "HB-finished", IsFinished: true, CreatedAt: now.Add(-48 * time.Hour)},
	}
	if err := db.Create(&packets).Error; err != nil {
		t.Fatal(err)
	}
	receipt := model.AutoDeleteMsg{PacketID: "FS-old", ChatID: -100, MessageID: 42, DeleteAt: now}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	// A pool failure must roll back closure as well as the returned balance.
	if err := db.Exec(`CREATE TRIGGER reject_pool BEFORE INSERT ON system_configs BEGIN SELECT RAISE(ABORT, 'pool unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := SettleExpiredRedPackets(now); err == nil {
		t.Fatal("expected pool failure")
	}
	var packet model.RedPacket
	db.First(&packet, "id = ?", "FS-old")
	if packet.IsFinished || packet.LeftPoints != 300 || packet.ExpiredAt != nil {
		t.Fatalf("partial settlement: %+v", packet)
	}
	db.Exec("DROP TRIGGER reject_pool")
	for i := 0; i < 2; i++ {
		if err := SettleExpiredRedPackets(now); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"FS-old", "HB-boss", "HB-partial"} {
		var p model.RedPacket
		if err := db.First(&p, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if !p.IsFinished || p.LeftPoints != 0 || p.LeftCount != 0 || p.ExpiredAt == nil || p.ReturnedPoints <= 0 {
			t.Fatalf("not settled: %+v", p)
		}
	}
	var pool model.SystemConfig
	if err := db.First(&pool, "key = ?", "fusion_pool_points").Error; err != nil || pool.Value != "47" {
		t.Fatalf("pool: %+v %v", pool, err)
	}
	var fresh model.RedPacket
	db.First(&fresh, "id = ?", "HB-fresh")
	if fresh.IsFinished || fresh.LeftPoints != 50 {
		t.Fatalf("fresh packet changed: %+v", fresh)
	}
	var bursts int64
	db.Model(&model.RedPacket{}).Where("id LIKE ? AND is_finished = ?", "FS-%", false).Count(&bursts)
	if bursts != 1 {
		t.Fatalf("expected one new pool packet, got %d", bursts)
	}
	var receipts int64
	db.Model(&model.AutoDeleteMsg{}).Where("packet_id = ?", "FS-old").Count(&receipts)
	if receipts != 1 {
		t.Fatal("settlement discarded deletion receipt")
	}
}

package activity

import (
	"errors"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestWorldBossPacketRequiresCurrentParticipationIdentity(t *testing.T) {
	db := setupLotteryDB(t)
	if err := db.AutoMigrate(&model.RedPacket{}, &model.RedPacketGrab{}, &model.WorldBossParticipant{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&model.User{TelegramID: 11, Username: "participant", AbsUserID: "replacement"},
		&model.WorldBossParticipant{BossID: "boss-1", UserID: 11, AbsUserID: "original", Damage: 3, IsRewarded: true},
		&model.RedPacket{ID: "HB-WB-1", RefID: "boss-1", ClaimScope: storage.RedPacketClaimScopeWorldBossParticipant, ClaimRuleVersion: storage.RedPacketWorldBossEffectiveClaimVersion, Count: 3, LeftCount: 3, TotalPoints: 30, LeftPoints: 30},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := grabRedPacketInTx(db, 11, "participant", "HB-"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("replacement account claimed the previous account's Boss packet: %v", err)
	}
	if ineligible, err := hasActiveIneligibleWorldBossRedPacket(11, "HB-"); err != nil || !ineligible {
		t.Fatalf("replacement account did not receive the ineligible response: %t %v", ineligible, err)
	}
	var packet model.RedPacket
	if err := db.First(&packet, "id = ?", "HB-WB-1").Error; err != nil || packet.LeftCount != 3 {
		t.Fatalf("rejected claim spent packet shares: %+v %v", packet, err)
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 11).Update("abs_user_id", "original").Error; err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]any{
		{"damage": 2.99, "is_rewarded": true},
		{"damage": 3, "is_rewarded": false},
	} {
		if err := db.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", "boss-1", 11).Updates(values).Error; err != nil {
			t.Fatal(err)
		}
		if _, _, err := grabRedPacketInTx(db, 11, "participant", "HB-"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("ineffective or unsettled participant claimed packet: %v", err)
		}
	}
	if err := db.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", "boss-1", 11).Updates(map[string]any{"damage": 3, "is_rewarded": true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, points, err := grabRedPacketInTx(db, 11, "participant", "HB-"); err != nil || points < 1 {
		t.Fatalf("original participant lost packet eligibility: points=%d err=%v", points, err)
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 11).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.RedPacket{ID: "HB-WB-2", RefID: "boss-1", ClaimScope: storage.RedPacketClaimScopeWorldBossParticipant, ClaimRuleVersion: storage.RedPacketWorldBossEffectiveClaimVersion, Count: 3, LeftCount: 3, TotalPoints: 30, LeftPoints: 30}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := grabRedPacketInTx(db, 11, "participant", "HB-"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("unbound participant claimed Boss packet: %v", err)
	}
	if ineligible, err := hasActiveIneligibleWorldBossRedPacket(11, "HB-"); err != nil || !ineligible {
		t.Fatalf("unbound participant did not receive the ineligible response: %t %v", ineligible, err)
	}

	if err := db.Create(&model.User{TelegramID: 12, Username: "ordinary", Points: 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.RedPacket{ID: "HB-ordinary", Count: 3, LeftCount: 3, TotalPoints: 30, LeftPoints: 30}).Error; err != nil {
		t.Fatal(err)
	}
	if _, points, err := grabRedPacketInTx(db, 12, "ordinary", "HB-"); err != nil || points < 1 {
		t.Fatalf("ordinary packet was restricted by Boss eligibility: points=%d err=%v", points, err)
	}
}

func TestLegacyWorldBossPacketKeepsEligibilityAfterMigration(t *testing.T) {
	db := setupLotteryDB(t)
	if err := db.AutoMigrate(&model.RedPacket{}, &model.RedPacketGrab{}, &model.WorldBossParticipant{}); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-version schema and an outstanding packet before upgrading.
	if err := db.Migrator().DropColumn(&model.RedPacket{}, "ClaimRuleVersion"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO red_packets
 (id, sender_id, ref_id, claim_scope, count, left_count, total_points, left_points, is_finished, created_at)
 VALUES ('HB-WB-old', 0, 'legacy-boss', ?, 10, 10, 30, 30, 0, ?)`, storage.RedPacketClaimScopeWorldBossParticipant, time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.AutoMigrate(&model.RedPacket{}); err != nil {
			t.Fatal(err)
		}
	}
	var packet model.RedPacket
	if err := db.First(&packet, "id = ?", "HB-WB-old").Error; err != nil || packet.ClaimRuleVersion != 0 {
		t.Fatalf("legacy version changed: %+v err=%v", packet, err)
	}
	for _, row := range []any{
		&model.User{TelegramID: 21, AbsUserID: "legacy-abs", Status: "active"},
		&model.WorldBossParticipant{BossID: "legacy-boss", UserID: 21, AbsUserID: "legacy-abs", Damage: 2.99, IsRewarded: false},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 21).Update("abs_user_id", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := grabRedPacketInTx(db, 21, "legacy", "HB-"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("legacy packet allowed changed identity: %v", err)
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 21).Update("abs_user_id", "legacy-abs").Error; err != nil {
		t.Fatal(err)
	}
	if _, points, err := grabRedPacketInTx(db, 21, "legacy", "HB-"); err != nil || points < 1 {
		t.Fatalf("legacy participant lost existing eligibility: points=%d err=%v", points, err)
	}
}

func TestExpiredRedPacketsCannotBeClaimedBeforeSweep(t *testing.T) {
	db := setupLotteryDB(t)
	if err := db.AutoMigrate(&model.RedPacket{}, &model.RedPacketGrab{}, &model.WorldBossParticipant{}); err != nil {
		t.Fatal(err)
	}
	packets := []model.RedPacket{
		{ID: "HB-expired", LeftCount: 3, LeftPoints: 30, CreatedAt: time.Now().Add(-25 * time.Hour)},
		{ID: "FS-expired", LeftCount: 3, LeftPoints: 30, CreatedAt: time.Now().Add(-25 * time.Hour)},
	}
	if err := db.Create(&packets).Error; err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"HB-", "FS-"} {
		if _, _, err := grabRedPacketInTx(db, 11, "user", prefix); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expired packet claim: %v", err)
		}
		if already, err := userAlreadyGrabbedAllActiveRedPackets(11, prefix); err != nil || already {
			t.Fatalf("expired packet treated as active: %t %v", already, err)
		}
	}
}

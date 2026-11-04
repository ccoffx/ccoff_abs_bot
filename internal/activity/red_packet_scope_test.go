package activity

import (
	"errors"
	"testing"

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
		&model.WorldBossParticipant{BossID: "boss-1", UserID: 11, AbsUserID: "original"},
		&model.RedPacket{ID: "HB-WB-1", RefID: "boss-1", ClaimScope: storage.RedPacketClaimScopeWorldBossParticipant, Count: 3, LeftCount: 3, TotalPoints: 30, LeftPoints: 30},
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
	if _, points, err := grabRedPacketInTx(db, 11, "participant", "HB-"); err != nil || points < 1 {
		t.Fatalf("original participant lost packet eligibility: points=%d err=%v", points, err)
	}
	if err := db.Model(&model.User{}).Where("telegram_id = ?", 11).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.RedPacket{ID: "HB-WB-2", RefID: "boss-1", ClaimScope: storage.RedPacketClaimScopeWorldBossParticipant, Count: 3, LeftCount: 3, TotalPoints: 30, LeftPoints: 30}).Error; err != nil {
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

package game

import (
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
)

func TestSpiritFireBoardsAndHornUseDisplayNames(t *testing.T) {
	setupGuardTestDB(t)
	if err := storage.DB.AutoMigrate(spiritfire.Models()...); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := spiritfire.Initialize(storage.DB, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Updates(map[string]any{"username": "ghost_tg_1", "display_name": "真实道友"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.SpiritFire{}).Where("id = 19").Updates(map[string]any{"owner_id": 1, "acquired_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	for _, holders := range []bool{false, true} {
		text, err := spiritFireBoard(holders, now)
		if err != nil || !strings.Contains(text, "真实道友") || strings.Contains(text, "ghost") {
			t.Fatalf("holders=%v text=%q err=%v", holders, text, err)
		}
	}
	recipients, err := querySectHornRecipientsTx(storage.DB, sectHornScopeWorld, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) == 0 || recipients[0].UserID != 1 || recipients[0].UserName != "真实道友" {
		t.Fatal("horn did not load display name", recipients)
	}
}

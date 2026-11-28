package titles

import (
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

func TestRankingTitlesUseWalletDisplayName(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Now()
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Updates(map[string]any{
		"username": "ghost_tg_1", "display_name": "真实道友", "points": 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	createActivityTitleRows(t,
		&model.Sect{Name: "测试宗门", OwnerID: 1},
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02"), CappedSeconds: 3600},
		&model.GardenPlanting{UserID: 1, Status: "harvested", HarvestedAt: &now, Quantity: 9},
		&model.GardenAlchemyRecord{UserID: 1, Quantity: 7},
		&model.WorldBossEvent{BossID: "display-name", Status: "settled", EndAt: now.Add(-time.Minute)},
		&model.WorldBossParticipant{BossID: "display-name", UserID: 1, Damage: 100, IsRewarded: true},
	)
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	refreshBossTitleTest(t, now)
	for _, key := range []string{rankings.WealthTitle, rankings.CultivationTitle, rankings.SectTitle, rankings.ListeningTitle, rankings.HarvestTitle, rankings.AlchemyTitle, rankings.BossTitle} {
		holder := rankHolderTest(t, key, 1)
		if holder.UserName != "真实道友" || strings.Contains(holder.UserName, "ghost") {
			t.Fatalf("title %s exposes incorrect name: %+v", key, holder)
		}
	}
}

package titles

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

func createActivityTitleRows(t *testing.T, rows ...any) {
	t.Helper()
	for _, row := range rows {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestMonthlyListeningTitleUsesValidatedCurrentBindingAndBeijingDates(t *testing.T) {
	setupTitleTestDB(t)
	// UTC 11 月 30 日下午已经是北京时间 12 月 1 日。
	now := time.Date(2026, 11, 30, 16, 30, 0, 0, time.UTC)
	first := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-12-01", CappedSeconds: 3600, RawSeconds: 999999, EffectiveHours: 9999, SpiritFireHours: 9999}
	deleted := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-12-01", CappedSeconds: 999999}
	createActivityTitleRows(t, &deleted)
	if err := storage.DB.Delete(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	createActivityTitleRows(t, &first,
		&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2026-12-01", CappedSeconds: 7200},
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-30", CappedSeconds: 999999},
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-12-02", CappedSeconds: 999999},
		&model.DailyListeningStat{UserID: 1, AbsUserID: "old-binding", DayKey: "2026-12-01", CappedSeconds: 999999},
		&model.DailyListeningStat{UserID: 3, DayKey: "2026-12-01", CappedSeconds: 999999},
		&model.DailyListeningStat{UserID: 999, AbsUserID: "deleted-user", DayKey: "2026-12-01", CappedSeconds: 999999})
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	h := rankHolderTest(t, rankings.ListeningTitle, 2)
	if h.Score != 2 || h.PeriodKey != "2026-12" {
		t.Fatal("month or score includes cultivation bonuses/invalid records", h)
	}
	if err := storage.DB.Model(&first).Update("capped_seconds", 7200).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 2)
	updateRankUserTest(t, 2, map[string]any{"status": "unbound"})
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 1)
}

func TestRankRefreshRejectsListeningBatchCrossingMonthWhileWaitingForDatabase(t *testing.T) {
	setupTitleTestDB(t)
	// 2027-02-01 同时是新月和周一。模拟刷新等待连接时跨过零点。
	before := time.Date(2027, 1, 31, 15, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2027-01-31", CappedSeconds: 3600},
		&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2027-02-01", CappedSeconds: 7200},
		&model.WorldBossEvent{BossID: "old-week", EndAt: before, Status: "settled"},
		&model.WorldBossParticipant{BossID: "old-week", UserID: 1, Damage: 10, IsRewarded: true},
		&model.WorldBossEvent{BossID: "new-week", EndAt: after, Status: "settled"},
		&model.WorldBossParticipant{BossID: "new-week", UserID: 2, Damage: 20, IsRewarded: true})
	sqlDB, err := storage.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	synctest.Test(t, func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- RefreshRankTitlesAfterListeningBatch(before, before) }()
		synctest.Wait() // 刷新已在 database/sql 的连接请求通道上等待。
		time.Sleep(2 * time.Second)
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrListeningBatchPeriodChanged) {
			t.Fatal("cross-month batch was not rejected", err)
		}
	})
	if h := rankHolderTest(t, rankings.ListeningTitle, 0); h.PeriodKey != "2027-02" {
		t.Fatal("delayed refresh published an incomplete new month", h)
	}
	if h := rankHolderTest(t, rankings.BossTitle, 2); h.PeriodKey != "2027-02-01" {
		t.Fatal("delayed refresh used the previous week", h)
	}
}

func TestListeningBatchCrossingMonthExpiresOldHolderWithoutNewAward(t *testing.T) {
	setupTitleTestDB(t)
	before := time.Date(2026, 11, 30, 15, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-30", CappedSeconds: 3600},
		&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2026-12-01", CappedSeconds: 1})
	if err := RefreshRankTitlesAfterListeningBatch(before, before); err != nil {
		t.Fatal(err)
	}
	if err := setDesiredInTx(storage.DB, -100, 1, rankings.ListeningTitle, "博览群书", true); err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(before, after); !errors.Is(err, ErrListeningBatchPeriodChanged) {
		t.Fatal("cross-month batch was not rejected", err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 0)
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != RealmKey {
		t.Fatal("old-month equipment was not released", e)
	}
	var old model.RankTitleHistory
	if err := storage.DB.Where("title_key = ?", rankings.ListeningTitle).Take(&old).Error; err != nil || old.ReleasedAt == nil || old.NoticeState != "obsolete" {
		t.Fatal("old-month history was not released", old, err)
	}
	var count int64
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key = ? AND period_key = ?", rankings.ListeningTitle, "2026-12").Count(&count).Error; err != nil || count != 0 {
		t.Fatal("incomplete new-month batch generated an award", count, err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(after, after); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 2)
}

func TestActivityTitleRolloverResetsTiesReleasesHistoryAndFallbacks(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	createActivityTitleRows(t, &model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2026-11-08", CappedSeconds: 7200})
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	if err := setDesiredInTx(storage.DB, -100, 2, rankings.ListeningTitle, "博览群书", true); err != nil {
		t.Fatal(err)
	}
	// 换期时不能用上月持有者打破新月首次并列。
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-12-01", CappedSeconds: 3600},
		&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2026-12-01", CappedSeconds: 3600})
	december := time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC)
	if err := RefreshRankTitlesAfterListeningBatch(december, december); err != nil {
		t.Fatal(err)
	}
	h := rankHolderTest(t, rankings.ListeningTitle, 1)
	if h.PeriodKey != "2026-12" || h.Score != 1 {
		t.Fatal("new month retained previous scores or tie preference", h)
	}
	if e := titleTestEquipment(t, -100, 2); e.TitleKey != RealmKey {
		t.Fatal("previous month's worn title did not fall back", e)
	}
	var history model.RankTitleHistory
	if err := storage.DB.Where("title_key = ? AND period_key = ?", rankings.ListeningTitle, "2026-11").Take(&history).Error; err != nil || history.ReleasedAt == nil || history.NoticeState != "obsolete" {
		t.Fatal("rollover lost old period history or left a pending notice", history, err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(december, december); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key = ?", rankings.ListeningTitle).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("rollover retried duplicate award", count, err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 0)
}

func TestWeeklyBossTitleCountsOnlySettledRewardEligibleDamage(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Date(2026, 11, 8, 15, 59, 0, 0, time.UTC) // 周日 23:59
	for _, row := range []struct {
		id, status string
		end        time.Time
	}{
		{"sat", "settled", now.Add(-24 * time.Hour)}, {"sun", "settled", now},
		{"old", "settled", now.AddDate(0, 0, -7)}, {"active", "active", now},
		{"settling", "settling", now}, {"future", "settled", now.Add(2 * time.Minute)},
	} {
		createActivityTitleRows(t, &model.WorldBossEvent{BossID: row.id, Status: row.status, EndAt: row.end})
	}
	createActivityTitleRows(t,
		&model.WorldBossParticipant{BossID: "sat", UserID: 2, Damage: 5, IsRewarded: true},
		&model.WorldBossParticipant{BossID: "sun", UserID: 2, Damage: 4, IsRewarded: true},
		&model.WorldBossParticipant{BossID: "sun", UserID: 1, Damage: 8, IsRewarded: true},
		&model.WorldBossParticipant{BossID: "sun", UserID: 1, Damage: 999, IsRewarded: false})
	for _, id := range []string{"old", "active", "settling", "future"} {
		createActivityTitleRows(t, &model.WorldBossParticipant{BossID: id, UserID: 1, Damage: 999, IsRewarded: true})
	}
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	h := rankHolderTest(t, rankings.BossTitle, 2)
	if h.Score != 9 || h.PeriodKey != "2026-11-02" {
		t.Fatal("weekly boss title counted invalid damage", h)
	}
	// 北京时间周一 00:00 重新开榜。
	if err := RefreshRankTitlesAfterListeningBatch(now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.BossTitle, 0)
	if err := RefreshRankTitlesAfterListeningBatch(now.Add(3*time.Minute), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.BossTitle, 1) // future 记录此时才归入新周。
}

func TestWeeklyBossTitleUsesActualCutoffForEarlyKillsAndDelayedSettlement(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Date(2026, 11, 8, 13, 35, 0, 0, time.UTC) // 北京时间周日 21:35
	cutoff := now.Add(-5 * time.Minute)
	createActivityTitleRows(t,
		&model.WorldBossEvent{BossID: "early", Status: "settled", EndAt: now.Add(25 * time.Minute), ListeningCutoffAt: &cutoff, SettledAt: &now},
		&model.WorldBossParticipant{BossID: "early", UserID: 1, Damage: 100, IsRewarded: true})
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	if h := rankHolderTest(t, rankings.BossTitle, 1); h.Score != 100 || h.PeriodKey != "2026-11-02" {
		t.Fatal("early kill was not counted immediately", h)
	}
	// 实际截止在周日，原定结束与重试结算跨到周一，不能计入新周。
	monday := time.Date(2026, 11, 8, 16, 10, 0, 0, time.UTC)
	if err := storage.DB.Model(&model.WorldBossEvent{}).Where("boss_id = ?", "early").Updates(map[string]any{"end_at": monday, "settled_at": monday}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(monday, monday); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.BossTitle, 0)
}

func TestMonthlyTitleContinuedLeaderGetsOneHistoryPerPeriod(t *testing.T) {
	setupTitleTestDB(t)
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-08", CappedSeconds: 3600},
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-12-01", CappedSeconds: 3600})
	if err := RefreshRankTitlesAfterListeningBatch(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := setDesiredInTx(storage.DB, -100, 1, rankings.ListeningTitle, "博览群书", true); err != nil {
		t.Fatal(err)
	}
	december := time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC)
	for range 2 {
		if err := RefreshRankTitlesAfterListeningBatch(december, december); err != nil {
			t.Fatal(err)
		}
	}
	var history []model.RankTitleHistory
	if err := storage.DB.Where("title_key = ?", rankings.ListeningTitle).Order("id").Find(&history).Error; err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].PeriodKey != "2026-11" || history[0].ReleasedAt == nil || history[1].PeriodKey != "2026-12" || history[1].ReleasedAt != nil {
		t.Fatal("continued leader lost per-period history", history)
	}
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != rankings.ListeningTitle || e.Version != 1 {
		t.Fatal("continued leader's still-valid equipment was reset", e)
	}
}

func TestLifetimeGardenTitlesUseProducedQuantitiesAndSurviveInventoryChanges(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Now()
	createActivityTitleRows(t,
		&model.GardenPlanting{UserID: 1, Status: "harvested", HarvestedAt: &now, Quantity: 2},
		&model.GardenPlanting{UserID: 1, Status: "harvested", HarvestedAt: &now, Quantity: 3},
		&model.GardenPlanting{UserID: 2, Status: "harvested", HarvestedAt: &now, Quantity: 6},
		&model.GardenPlanting{UserID: 1, Status: "growing", Quantity: 999},
		&model.GardenPlanting{UserID: 1, Status: "harvested", Quantity: 999},
		&model.GardenAlchemyRecord{UserID: 1, Quantity: 1}, &model.GardenAlchemyRecord{UserID: 1, Quantity: 1},
		&model.GardenAlchemyRecord{UserID: 2, Quantity: 2})
	refreshRankTest(t)
	h := rankHolderTest(t, rankings.HarvestTitle, 2)
	if h.Score != 6 {
		t.Fatal("harvest title counted plots instead of actual herbs", h)
	}
	rankHolderTest(t, rankings.AlchemyTitle, 1)
	createActivityTitleRows(t, &model.GardenAlchemyRecord{UserID: 2, Quantity: 1})
	refreshRankTest(t)
	h = rankHolderTest(t, rankings.AlchemyTitle, 2)
	if h.Score != 3 {
		t.Fatal("alchemy title used inventory or failed to transfer", h)
	}
	// 长期记录跨周跨月保持，不重复发奖。
	if err := RefreshRankTitlesAfterListeningBatch(now.AddDate(1, 0, 0), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.HarvestTitle, 2)
	rankHolderTest(t, rankings.AlchemyTitle, 2)
	updateRankUserTest(t, 2, map[string]any{"points": 999})
	if err := storage.DB.Migrator().DropTable(&model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err == nil {
		t.Fatal("source failure treated as an empty leaderboard")
	}
	rankHolderTest(t, rankings.WealthTitle, 1)
	rankHolderTest(t, rankings.AlchemyTitle, 2)
}

func TestNewActivityTitlesAppearAndCanBeEquipped(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Now()
	day := now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 3600},
		&model.WorldBossEvent{BossID: "now", EndAt: now, Status: "settled"},
		&model.WorldBossParticipant{BossID: "now", UserID: 1, Damage: 10, IsRewarded: true},
		&model.GardenPlanting{UserID: 1, Status: "harvested", HarvestedAt: &now, Quantity: 3},
		&model.GardenAlchemyRecord{UserID: 1, Quantity: 2})
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	updateRankUserTest(t, 1, map[string]any{"username": strings.Repeat("😀", 50)})
	if err := TrackMember(-100, strings.Repeat("😀", 40), 1); err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	out, _, err := Render(-100, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(utf16.Encode([]rune(out))) > 4096 {
		t.Fatal("expanded title panel exceeds Telegram message limit")
	}
	for _, key := range []string{rankings.ListeningTitle, rankings.BossTitle, rankings.HarvestTitle, rankings.AlchemyTitle} {
		d, _ := ByKey(key)
		if !strings.Contains(out, d.Name) {
			t.Fatal("missing new title", key)
		}
		if err := Equip(-100, 1, key); err != nil {
			t.Fatal("new title cannot be equipped", key, err)
		}
		if e := titleTestEquipment(t, -100, 1); e.TitleKey != key || e.DesiredTag != d.Name {
			t.Fatal("wrong new title equipment", e)
		}
	}
}

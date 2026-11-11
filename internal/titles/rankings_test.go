package titles

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

func refreshRankTest(t *testing.T) {
	t.Helper()
	if err := RefreshRankTitlesAfterListeningBatch(time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func rankHolderTest(t *testing.T, key string, userID int64) model.RankTitleHolder {
	t.Helper()
	var h model.RankTitleHolder
	if err := storage.DB.Where("title_key = ?", key).Take(&h).Error; err != nil || h.UserID != userID {
		t.Fatalf("holder %s = %+v, want %d; err=%v", key, h, userID, err)
	}
	return h
}

func updateRankUserTest(t *testing.T, id int64, values map[string]any) {
	t.Helper()
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", id).Updates(values).Error; err != nil {
		t.Fatal(err)
	}
}

func TestWealthTitleEligibilityTiesTransferAndHistory(t *testing.T) {
	setupTitleTestDB(t)
	config.AppConfig.AdminIDs = map[int64]bool{5: true}
	for _, u := range []model.User{
		{TelegramID: 4, Username: "dbadmin", Points: 9999, Role: "admin"},
		{TelegramID: 5, Username: "envadmin", Points: 9999},
		{TelegramID: 6, Username: "superadmin", Points: 9999, Role: "super_admin"},
	} {
		if err := storage.DB.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	updateRankUserTest(t, 2, map[string]any{"points": 100})
	refreshRankTest(t)
	rankHolderTest(t, rankings.WealthTitle, 2)
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -100, 2); e.TitleKey != RealmKey {
		t.Fatal("new title replaced current selection", e)
	}
	for _, chat := range []int64{-100, -200} {
		if err := Equip(chat, 2, rankings.WealthTitle); err != nil {
			t.Fatal(err)
		}
	}
	if err := Equip(-100, 1, rankings.WealthTitle); !errors.Is(err, ErrNotHeld) {
		t.Fatal("non-holder wore global title", err)
	}
	updateRankUserTest(t, 1, map[string]any{"points": 100})
	refreshRankTest(t)
	rankHolderTest(t, rankings.WealthTitle, 2)
	var first model.User
	if err := rankings.Wealth(storage.DB).Take(&first).Error; err != nil || first.TelegramID != 2 {
		t.Fatal("leaderboard and incumbent tie disagree", first, err)
	}
	if err := storage.DB.Create(&model.Cultivation{UserID: 2, MajorRealm: 5, MinorRealm: 7}).Error; err != nil {
		t.Fatal(err)
	}
	updateRankUserTest(t, 1, map[string]any{"points": 101})
	refreshRankTest(t)
	rankHolderTest(t, rankings.WealthTitle, 1)
	for _, chat := range []int64{-100, -200} {
		if e := titleTestEquipment(t, chat, 2); e.TitleKey != RealmKey || e.DesiredTag != "七星斗王" {
			t.Fatal("lost global title did not fall back in every group", e)
		}
	}
	var old model.RankTitleHistory
	if err := storage.DB.Where("title_key = ? AND user_id = ?", rankings.WealthTitle, 2).Take(&old).Error; err != nil || old.ReleasedAt == nil || old.NoticeState != "obsolete" {
		t.Fatal("old acquisition not retained/released", old, err)
	}
	out, _, err := Render(-100, 2, false)
	if err != nil || !strings.Contains(out, "富甲天下") || !strings.Contains(out, "曾经获得（当前不可佩戴）") {
		t.Fatal("historical title not shown", out, err)
	}
	// 夺回会新增一次历史，重复巡检不会新增。
	updateRankUserTest(t, 2, map[string]any{"points": 102})
	refreshRankTest(t)
	refreshRankTest(t)
	var n int64
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key = ? AND user_id = ?", rankings.WealthTitle, 2).Count(&n).Error; err != nil || n != 2 {
		t.Fatal("reacquisition or no-op refresh duplicated history", n, err)
	}
}

func TestCultivationTitleUsesFullLeaderboardOrder(t *testing.T) {
	setupTitleTestDB(t)
	if err := storage.DB.Create(&model.Cultivation{UserID: 2, MajorRealm: 3, MinorRealm: 1, TotalAudioTime: 20}).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.CultivationTitle, 2)
	// 境界、星级、总修为相同时，先比较苦修；全部相同才保留现任。
	for _, c := range []struct {
		major, minor int
		total, pill  float64
		want         int64
	}{
		{3, 1, 20, 0, 2},
		{3, 1, 19, 1, 2},
		{3, 1, 21, 0, 1},
		{3, 2, 0, 0, 1},
		{4, 1, 0, 0, 1},
	} {
		if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]any{"major_realm": c.major, "minor_realm": c.minor, "total_audio_time": c.total, "pill_audio_time": c.pill}).Error; err != nil {
			t.Fatal(err)
		}
		refreshRankTest(t)
		rankHolderTest(t, rankings.CultivationTitle, c.want)
		var first model.Cultivation
		if err := rankings.Cultivation(storage.DB).Select("cultivations.*").Take(&first).Error; err != nil || first.UserID != c.want {
			t.Fatal("cultivation board/title disagree", first, err)
		}
	}
	// 软删除账号不能继续占据榜首；彻底空榜时称号空缺。
	if err := storage.DB.Where("telegram_id = ?", 1).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.CultivationTitle, 2)
	if err := Equip(-100, 2, rankings.CultivationTitle); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Where("1 = 1").Delete(&model.Cultivation{}).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.CultivationTitle, 0)
	if e := titleTestEquipment(t, -100, 2); e.TitleKey != RealmKey {
		t.Fatal("empty board left title equipped", e)
	}
}

func TestSectTitleFollowsLeadingSectAndCurrentOwner(t *testing.T) {
	setupTitleTestDB(t)
	a := model.Sect{Name: "甲宗", Level: 1, OwnerID: 1}
	b := model.Sect{Name: "乙宗", Level: 2, OwnerID: 2}
	if err := storage.DB.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&b).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.SectTitle, 2)
	if err := storage.DB.Model(&a).Update("level", 2).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	h := rankHolderTest(t, rankings.SectTitle, 2)
	if h.SubjectID != b.ID {
		t.Fatal("tied sect replaced incumbent", h)
	}
	if err := Equip(-100, 2, rankings.SectTitle); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&b).Update("owner_id", 3).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.SectTitle, 3)
	if e := titleTestEquipment(t, -100, 2); e.TitleKey != RealmKey {
		t.Fatal("former owner kept worn title", e)
	}
	// 后续排序指标同样决定宗门先后。
	for _, field := range []string{"prestige", "funds", "member_count"} {
		if err := storage.DB.Model(&a).Update(field, 1).Error; err != nil {
			t.Fatal(err)
		}
		refreshRankTest(t)
		rankHolderTest(t, rankings.SectTitle, 1)
		if err := storage.DB.Model(&b).Update(field, 1).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.Delete(&a).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.SectTitle, 3)
	if err := storage.DB.Delete(&b).Error; err != nil {
		t.Fatal(err)
	}
	refreshRankTest(t)
	rankHolderTest(t, rankings.SectTitle, 0)
}

func TestRankTitleRefreshRollbackAndConcurrentIdempotency(t *testing.T) {
	setupTitleTestDB(t)
	refreshRankTest(t)
	if err := Equip(-100, 1, rankings.WealthTitle); err != nil {
		t.Fatal(err)
	}
	old := titleTestEquipment(t, -100, 1)
	updateRankUserTest(t, 2, map[string]any{"points": 1})
	if err := storage.DB.Exec(`CREATE TRIGGER fail_rank_history BEFORE INSERT ON rank_title_histories BEGIN SELECT RAISE(FAIL, 'history unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(time.Now(), time.Now()); err == nil {
		t.Fatal("history failure ignored")
	}
	rankHolderTest(t, rankings.WealthTitle, 1)
	if e := titleTestEquipment(t, -100, 1); e.Version != old.Version || e.TitleKey != old.TitleKey {
		t.Fatal("failed transfer left partial fallback", e)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_rank_history").Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := RefreshRankTitlesAfterListeningBatch(time.Now(), time.Now()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rankHolderTest(t, rankings.WealthTitle, 2)
	var n int64
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key = ?", rankings.WealthTitle).Count(&n).Error; err != nil || n != 2 {
		t.Fatal("duplicate acquisitions after concurrent/restarted refresh", n, err)
	}
	// 读取失败不可按空榜撤销称号。
	if err := storage.DB.Migrator().DropTable(&model.Sect{}); err != nil {
		t.Fatal(err)
	}
	if err := RefreshRankTitlesAfterListeningBatch(time.Now(), time.Now()); err == nil {
		t.Fatal("rank source error treated as empty board")
	}
	rankHolderTest(t, rankings.WealthTitle, 2)
}

func TestRejectedStaleEquipStillCommitsRankTransfer(t *testing.T) {
	setupTitleTestDB(t)
	refreshRankTest(t)
	if err := Equip(-100, 1, rankings.WealthTitle); err != nil {
		t.Fatal(err)
	}
	updateRankUserTest(t, 2, map[string]any{"points": 1})
	if err := Equip(-100, 1, rankings.WealthTitle); !errors.Is(err, ErrNotHeld) {
		t.Fatal("former leader could equip stale title", err)
	}
	rankHolderTest(t, rankings.WealthTitle, 2)
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != RealmKey || e.DesiredTag != "三星斗师" {
		t.Fatal("rejected action rolled back authoritative title transfer", e)
	}
}

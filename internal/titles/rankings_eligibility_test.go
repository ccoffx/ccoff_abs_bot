package titles

import (
	"errors"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningDependentTitlesReleaseIneligibleIncumbentBeforeNextBatch(t *testing.T) {
	for _, tc := range []struct {
		name, key  string
		invalidate func(*testing.T)
	}{
		{"listening unbound", rankings.ListeningTitle, func(t *testing.T) {
			updateRankUserTest(t, 1, map[string]any{"abs_user_id": "", "status": "unbound"})
		}},
		{"listening binding changed", rankings.ListeningTitle, func(t *testing.T) {
			updateRankUserTest(t, 1, map[string]any{"abs_user_id": "replacement"})
		}},
		{"listening records invalidated", rankings.ListeningTitle, func(t *testing.T) {
			if err := storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", 1).Update("capped_seconds", 0).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"listening account deleted", rankings.ListeningTitle, deleteRankTitleTestUser},
		{"cultivation account deleted", rankings.CultivationTitle, deleteRankTitleTestUser},
		{"cultivation profile deleted", rankings.CultivationTitle, func(t *testing.T) {
			if err := storage.DB.Where("user_id = ?", 1).Delete(&model.Cultivation{}).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"cultivation profile migrated", rankings.CultivationTitle, func(t *testing.T) {
			if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("user_id", 3).Error; err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTitleTestDB(t)
			now := time.Now()
			day := now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
			createActivityTitleRows(t,
				&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 7200},
				&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: day, CappedSeconds: 3600},
				&model.Cultivation{UserID: 2, MajorRealm: 1, MinorRealm: 1})
			refreshRankTest(t)
			for _, chat := range []int64{-100, -200} {
				if err := Equip(chat, 1, tc.key); err != nil {
					t.Fatal(err)
				}
			}
			tc.invalidate(t)
			// The equip path must commit revocation even when it rejects the button.
			if err := Equip(-100, 1, tc.key); !errors.Is(err, ErrNotHeld) {
				t.Fatalf("ineligible incumbent can still equip title: %v", err)
			}
			rankHolderTest(t, tc.key, 0)
			for _, chat := range []int64{-100, -200} {
				if e := titleTestEquipment(t, chat, 1); e.TitleKey != RealmKey {
					t.Fatal("revoked title remained equipped", e)
				}
			}
			var history model.RankTitleHistory
			if err := storage.DB.Where("title_key = ? AND user_id = ?", tc.key, 1).Take(&history).Error; err != nil || history.ReleasedAt == nil || history.NoticeState != "obsolete" {
				t.Fatal("revocation did not release history and cancel notice", history, err)
			}
			if err := RefreshRankTitles(now); err != nil {
				t.Fatal(err)
			}
			rankHolderTest(t, tc.key, 0)
			// Recovery is published only after the next complete batch.
			refreshRankTest(t)
			var next model.RankTitleHolder
			if err := storage.DB.Where("title_key = ?", tc.key).Take(&next).Error; err != nil || next.UserID <= 0 || next.UserID == 1 {
				t.Fatal("complete batch did not publish a qualified successor", next, err)
			}
		})
	}
}

func deleteRankTitleTestUser(t *testing.T) {
	t.Helper()
	if err := storage.DB.Where("telegram_id = ?", 1).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
}

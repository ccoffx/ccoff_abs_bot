package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningRefreshRejectsBindingChangedDuringABSRequest(t *testing.T) {
	for _, backfill := range []bool{false, true} {
		t.Run(fmt.Sprintf("backfill=%t", backfill), func(t *testing.T) {
			setupRetreatBackfillDB(t)
			start, _ := time.Parse(time.RFC3339, "2026-10-27T23:30:00+08:00")
			retreat := model.SectCaveRetreat{UserID: 1, StartAt: start, EndAt: start.Add(time.Hour), Status: "expired"}
			if err := storage.DB.Create(&retreat).Error; err != nil {
				t.Fatal(err)
			}
			if backfill {
				if err := EnsureABSCrossDaySessionScanCursor(1, "abs1"); err != nil {
					t.Fatal(err)
				}
			}
			var once sync.Once
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/listening-stats") {
					fmt.Fprint(w, `{"days":{"2026-10-27":3600}}`)
					return
				}
				once.Do(func() {
					if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Update("abs_user_id", "replacement").Error; err != nil {
						t.Error(err)
					}
				})
				session := bossSession("old-session", start, start.Add(time.Hour), 3600)
				session["date"], session["userId"] = "2026-10-27", "abs1"
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
			})
			if backfill {
				if err := advanceABSCrossDaySessionBackfill(time.Now()); err == nil {
					t.Fatal("historical scan accepted an obsolete binding")
				}
				var cursor model.ABSCrossDaySessionScanCursor
				if err := storage.DB.First(&cursor).Error; err != nil || cursor.NextPage != 0 {
					t.Fatalf("obsolete response advanced the cursor: %+v %v", cursor, err)
				}
			} else if _, ok := refreshDailyListeningStatsFromABS(1, "abs1", true); ok {
				t.Fatal("refresh accepted an obsolete binding")
			}
			for _, table := range []any{&model.DailyListeningStat{}, &model.ABSCrossDaySessionAdjustment{}} {
				var count int64
				if err := storage.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("obsolete response wrote %T: count=%d err=%v", table, count, err)
				}
			}
			if err := storage.DB.First(&retreat, retreat.ID).Error; err != nil || retreat.ListeningEvidenceJSON != "" {
				t.Fatalf("obsolete response wrote retreat evidence: %+v %v", retreat, err)
			}
		})
	}
}

func TestListeningCacheRequiresCurrentBindingAndValidFetchTime(t *testing.T) {
	setupRetreatBackfillDB(t)
	now := time.Now()
	stat := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now), LastFetchedAt: now, RawSeconds: 3600}
	if err := storage.DB.Create(&stat).Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedDailyListeningDaysWithinTTL(1, "abs1", now); !ok {
		t.Fatal("current cache was rejected")
	}
	if _, ok := cachedDailyListeningDaysWithinTTL(1, "replacement", now); ok {
		t.Fatal("cache from another ABS identity was reused")
	}
	if _, ok := cachedDailyListeningDaysWithinTTL(1, "abs1", now.Add(-time.Minute)); ok {
		t.Fatal("future cache timestamp bypassed refresh")
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 1").Update("abs_user_id", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedDailyListeningDaysWithinTTL(1, "abs1", now); ok {
		t.Fatal("obsolete binding reused its old cache")
	}
}

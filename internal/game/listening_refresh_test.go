package game

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/titles"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestListeningBatchIsolatesConcurrentPersonalRefreshUntilPublication(t *testing.T) {
	setupRetreatBackfillDB(t)
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.SectMember{}, &model.Sect{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec("CREATE UNIQUE INDEX ux_isolated_cultivation ON cultivations(user_id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 2, Username: "second", AbsUserID: "abs2", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	var firstRequests atomic.Int32
	personalStarted := make(chan struct{})
	personalDone := make(chan error, 1)
	today := SectDayKey(time.Now())
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/listening-stats") {
			fmt.Fprint(w, `{"sessions":[]}`)
			return
		}
		seconds := 3600
		if strings.Contains(r.URL.Path, "/abs1/") {
			if firstRequests.Add(1) > 1 {
				// The personal request must reach ABS only after batch publication.
				var holder model.RankTitleHolder
				if err := storage.DB.Where("title_key = ?", rankings.ListeningTitle).Take(&holder).Error; err != nil || holder.UserID != 2 {
					t.Errorf("personal refresh ran before batch publication: holder=%+v err=%v", holder, err)
				}
				seconds = 10800
			}
		} else {
			go func() {
				close(personalStarted)
				_, _, _, err := refreshUserDailyListeningAndCultivation(1)
				personalDone <- err
			}()
			<-personalStarted
			seconds = 7200
		}
		fmt.Fprintf(w, `{"days":{%q:%d}}`, today, seconds)
	})
	if _, _, _, err := RefreshAllDailyListeningStatsWithOptions(false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-personalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("personal refresh did not resume after the batch")
	}
	for _, key := range []string{rankings.ListeningTitle, rankings.CultivationTitle} {
		var holder model.RankTitleHolder
		if err := storage.DB.Where("title_key = ?", key).Take(&holder).Error; err != nil || holder.UserID != 2 {
			t.Fatalf("personal refresh changed published title %s: holder=%+v err=%v", key, holder, err)
		}
	}
	if firstRequests.Load() != 2 {
		t.Fatal("personal refresh never reached ABS", firstRequests.Load())
	}
}

func TestListeningReportWriteWaitsForBatch(t *testing.T) {
	// Report/secret-realm paths receive ABS days externally rather than going
	// through refreshDailyListeningStatsFromABS. They must obey the same barrier.
	listeningRefreshMu.Lock()
	locked := true
	defer func() {
		if locked {
			listeningRefreshMu.Unlock()
		}
	}()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- recordDailyListeningStatsFromABSDays(1, "abs1", nil, time.Now())
	}()
	<-started
	select {
	case <-done:
		t.Fatal("report write bypassed the active batch")
	case <-time.After(50 * time.Millisecond):
	}
	listeningRefreshMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid payload unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("report write did not resume after the batch")
	}

}

func TestListeningBatchPublishesOnlyAfterAllAccountsSucceed(t *testing.T) {
	setupRetreatBackfillDB(t)
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.SectMember{}, &model.Sect{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec("CREATE UNIQUE INDEX ux_refresh_cultivation ON cultivations(user_id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 2, Username: "second", AbsUserID: "abs2", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	var phase, requests atomic.Int32
	today := SectDayKey(time.Now())
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/listening-stats") {
			fmt.Fprint(w, `{"sessions":[]}`)
			return
		}
		requests.Add(1)
		p := phase.Load()
		if p > 0 {
			// 即使页面或后台恰好在下一个账号的请求中运行，也不能用
			// 上一个账号刚写入的中间值变更榜首。
			if err := titles.RefreshRankTitles(time.Now()); err != nil {
				t.Errorf("intermediate title refresh: %v", err)
			}
			var holder model.RankTitleHolder
			if err := storage.DB.Where("title_key = ?", rankings.ListeningTitle).Take(&holder).Error; err != nil || holder.UserID != 2 {
				t.Errorf("batch published intermediate holder: %+v err=%v", holder, err)
			}
		}
		seconds := 3600
		if p > 0 {
			seconds = 10800
		}
		if p == 3 {
			seconds = 18000
		}
		if strings.Contains(r.URL.Path, "/abs2/") {
			if p == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			seconds = 7200
			if p > 1 {
				seconds = 14400
			}
		}
		fmt.Fprintf(w, `{"days":{%q:%d}}`, today, seconds)
	})
	for p := int32(0); p < 4; p++ {
		phase.Store(p)
		success, total, skipped, err := RefreshAllDailyListeningStatsWithOptions(false)
		if p == 1 {
			if err == nil || success != 1 {
				t.Fatalf("partial failure not reported: success=%d err=%v", success, err)
			}
		} else if err != nil || success != 2 {
			t.Fatalf("batch %d: success=%d err=%v", p, success, err)
		}
		if total != 2 || skipped != 0 {
			t.Fatalf("batch reused individual caches: total=%d skipped=%d", total, skipped)
		}
		var holder model.RankTitleHolder
		want := int64(2)
		if p == 3 {
			want = 1
		}
		if err := storage.DB.Where("title_key = ?", rankings.ListeningTitle).Take(&holder).Error; err != nil || holder.UserID != want {
			t.Fatalf("batch %d: holder=%+v want=%d err=%v", p, holder, want, err)
		}
	}
	if requests.Load() != 8 {
		t.Fatal("full batches skipped fresh accounts", requests.Load())
	}
	var count int64
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key = ?", rankings.ListeningTitle).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("partial batches generated title history", count, err)
	}
}

func TestRefreshAllDailyListeningStatsReportsUserQueryFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = db
	t.Cleanup(func() { storage.DB = previous })

	_, _, _, refreshErr := RefreshAllDailyListeningStatsWithOptions(false)
	if refreshErr == nil {
		t.Fatal("missing users table was reported as a successful refresh")
	}
}

func TestRefreshAllDailyListeningStatsReportsPerUserFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.SystemConfig{}, &model.DailyListeningStat{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{TelegramID: 91346, Username: "listener", AbsUserID: "abs-listener"}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousClient := storage.DB, abs.DefaultClient
	storage.DB, abs.DefaultClient = db, nil
	t.Cleanup(func() { storage.DB, abs.DefaultClient = previousDB, previousClient })

	success, total, skipped, refreshErr := RefreshAllDailyListeningStatsWithOptions(false)
	if refreshErr == nil || success != 0 || total != 1 || skipped != 0 {
		t.Fatalf("failed ABS refresh reported success=%d total=%d skipped=%d err=%v", success, total, skipped, refreshErr)
	}
	stored, err := storage.GetSystemConfigStringChecked(storage.DailyListeningRefreshLastErrorKey)
	if err != nil || stored == "" {
		t.Fatalf("failure state was not stored: value=%q err=%v", stored, err)
	}
}

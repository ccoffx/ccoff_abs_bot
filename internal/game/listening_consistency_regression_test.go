package game

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func TestListeningSettlementCannotRecreateDeletedAccount(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	assertListeningSync(t, now, 15.84)
	contributionMust(t, db.Where("telegram_id = 1").Delete(&model.User{}).Error)
	contributionMust(t, db.Where("user_id = 1").Delete(&model.Cultivation{}).Error)
	contributionMust(t, db.Delete(&stat).Error)
	// An already queued refresh may reach settlement after account cleanup.
	if total, ok := syncCultivationFromDailyListeningStatsAt(1, now); ok {
		t.Fatalf("deleted account was settled: total=%v", total)
	}
	var count int64
	contributionMust(t, db.Model(&model.Cultivation{}).Where("user_id = 1").Count(&count).Error)
	if count != 0 {
		t.Fatal("late settlement recreated a deleted cultivation profile")
	}
}

func TestCultivationReadCannotCreateProfileForMissingAccount(t *testing.T) {
	db, _, _ := setupListeningContributionTest(t)
	contributionMust(t, db.Where("telegram_id = 1").Delete(&model.User{}).Error)
	if cul := GetOrCreateCultivation(1); cul != nil {
		t.Fatalf("read recreated a profile after account deletion: %+v", cul)
	}
}

func TestStaleRealmSyncCannotUpdateReplacementProfile(t *testing.T) {
	db, _, _ := setupListeningContributionTest(t)
	old := model.Cultivation{UserID: 1, TotalAudioTime: 4, MinorRealm: 1}
	contributionMust(t, db.Create(&old).Error)
	contributionMust(t, db.Delete(&old).Error)
	replacement := model.Cultivation{UserID: 1, TotalAudioTime: 4, MinorRealm: 1}
	contributionMust(t, db.Create(&replacement).Error)
	SyncCultivationRealm(&old)
	contributionMust(t, db.First(&replacement, replacement.ID).Error)
	if replacement.MinorRealm != 1 {
		t.Fatalf("a stale profile wrote into a replacement row: %+v", replacement)
	}
}

func TestListeningIntegerThresholdsSurviveFloatingPointSums(t *testing.T) {
	for _, bootstrap := range []bool{true, false} {
		t.Run(fmt.Sprint("bootstrap=", bootstrap), func(t *testing.T) {
			db, now, original := setupListeningContributionTest(t)
			contributionMust(t, db.Delete(&original).Error)
			if !bootstrap {
				assertListeningSync(t, now, 0)
			}
			// 100 days of six minutes must equal exactly ten eligible hours for
			// contribution and realm thresholds, despite 0.1's binary rounding.
			stats := make([]model.DailyListeningStat, 100)
			for i := range stats {
				stats[i] = model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now.AddDate(0, 0, -i-2)), RawSeconds: 360, CappedSeconds: 360, EffectiveHours: .1}
			}
			contributionMust(t, db.Create(&stats).Error)
			assertListeningSync(t, now, 10)
			assertContribution(t, db, 10)
			var cul model.Cultivation
			contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
			if cul.MajorRealm != 1 || cul.MinorRealm != 1 {
				t.Fatalf("ten hours failed automatic promotion: %+v", cul)
			}
			// A real reduction followed by recovery must not award twice.
			contributionMust(t, db.Model(&stats[0]).Updates(map[string]any{"capped_seconds": 324, "effective_hours": .09}).Error)
			assertListeningSync(t, now, 9.99)
			contributionMust(t, db.Model(&stats[0]).Updates(map[string]any{"capped_seconds": 360, "effective_hours": .1}).Error)
			assertListeningSync(t, now, 10)
			assertContribution(t, db, 10)
			below := model.Cultivation{TotalAudioTime: 10 - 1.0/3600, MinorRealm: 1}
			calculateCultivationRealm(&below)
			if below.MajorRealm != 0 || math.Abs(cul.TotalAudioTime-10) > 1e-9 {
				t.Fatal("threshold tolerance admitted a real one-second shortfall")
			}
		})
	}
}

func TestListeningContributionRepairsLegacyRoundingCursor(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 1, "capped_seconds": 3600}).Error)
	// The previous floor-based implementation paid zero and saved the sum just
	// below one. The fix must pay the missing point once, then save a safe cursor.
	contributionMust(t, db.Create(&model.Cultivation{UserID: 1, TotalAudioTime: 1, SectListeningSettledHours: math.Nextafter(1, 0)}).Error)
	assertListeningSync(t, now, 1)
	assertContribution(t, db, 1)
	assertListeningSync(t, now, 1)
	assertContribution(t, db, 1)
}

func TestListeningSettlementRealmAndEventRollbackTogether(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 1, "capped_seconds": 3600}).Error)
	assertListeningSync(t, now, 1)
	contributionMust(t, db.Model(&model.Cultivation{}).Where("user_id = 1").UpdateColumn("pill_audio_time", 4).Error)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 2, "capped_seconds": 7200}).Error)
	contributionMust(t, db.Exec("CREATE TRIGGER reject_settlement_event BEFORE INSERT ON cultivation_upgrade_events BEGIN SELECT RAISE(ABORT, 'event failure'); END").Error)
	if _, ok := syncCultivationFromDailyListeningStatsAt(1, now); ok {
		t.Fatal("event write failure accepted")
	}
	var cul model.Cultivation
	contributionMust(t, db.Where("user_id = 1").First(&cul).Error)
	if cul.MinorRealm != 1 || cul.TotalAudioTime != 1 || cul.PillAudioTime != 4 {
		t.Fatalf("failed settlement changed the profile outside its transaction: %+v", cul)
	}
	contributionMust(t, db.Exec("DROP TRIGGER reject_settlement_event").Error)
	assertListeningSync(t, now, 2)
	var events []model.CultivationUpgradeEvent
	contributionMust(t, db.Find(&events).Error)
	if len(events) != 1 || events[0].FromMinor != 5 || events[0].ToMinor != 6 {
		t.Fatalf("pill-only growth was counted as a listening upgrade: %+v", events)
	}
	assertListeningSync(t, now, 2)
	var count int64
	contributionMust(t, db.Model(&model.CultivationUpgradeEvent{}).Count(&count).Error)
	if count != 1 {
		t.Fatal("retry created duplicate upgrade events")
	}
}

func TestBookProgressDoesNotHoldListeningBatchBarrier(t *testing.T) {
	setupRetreatBackfillDB(t)
	contributionMust(t, storage.DB.AutoMigrate(&model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.TitleEquipment{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}))
	contributionMust(t, storage.DB.Exec("CREATE UNIQUE INDEX ux_progress_cultivation ON cultivations(user_id)").Error)
	progressStarted, releaseProgress := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseProgress) }) }
	personalStarted := make(chan struct{}, 1)
	var statsRequests atomic.Int32
	today := SectDayKey(time.Now())
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/listening-stats"):
			if statsRequests.Add(1) > 1 {
				personalStarted <- struct{}{}
			}
			fmt.Fprintf(w, `{"days":{%q:360}}`, today)
		case strings.HasSuffix(r.URL.Path, "/listening-sessions"):
			fmt.Fprint(w, `{"sessions":[]}`)
		default:
			close(progressStarted)
			<-releaseProgress
			fmt.Fprint(w, `{"mediaProgress":[]}`)
		}
	})
	t.Cleanup(release)
	batchDone := make(chan error, 1)
	go func() { _, _, _, err := RefreshAllDailyListeningStatsWithOptions(false); batchDone <- err }()
	select {
	case <-progressStarted:
		release()
		contributionMust(t, <-batchDone)
		t.Fatal("full listening refresh waited for optional book progress")
	case err := <-batchDone:
		contributionMust(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("full listening refresh did not finish")
	}
	progressDone := make(chan struct{})
	go func() { refreshListeningReportProgress(1, "abs1", time.Now()); close(progressDone) }()
	select {
	case <-progressStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("book progress never started")
	}
	personalDone := make(chan error, 1)
	go func() { _, _, _, err := refreshUserDailyListeningAndCultivation(1); personalDone <- err }()
	blocked := false
	select {
	case <-personalStarted:
	case <-time.After(time.Second):
		blocked = true
	}
	release()
	<-progressDone
	contributionMust(t, <-personalDone)
	if blocked {
		t.Fatal("optional book progress blocked a personal listening refresh")
	}
}

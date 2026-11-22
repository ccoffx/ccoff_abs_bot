package game

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func TestIncrementalListeningRetainsBootstrapDeletionAndFutureDays(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	// An existing profile is replaced by its daily sum once, without doubling it.
	contributionMust(t, db.Create(&model.Cultivation{UserID: 1, TotalAudioTime: 15.84, SectListeningSettledHours: 6.4}).Error)
	assertListeningSync(t, now, 15.84)
	assertListeningSync(t, now, 15.84)
	contributionMust(t, db.Delete(&stat).Error)
	assertListeningSync(t, now, 0)
	assertListeningSync(t, now, 0)
	assertContribution(t, db, 0)
	contributionMust(t, db.Unscoped().Model(&stat).UpdateColumn("deleted_at", nil).Error)
	assertListeningSync(t, now, 15.84)
	assertContribution(t, db, 0)
	// A future row skipped now must still enter the total after several offline days.
	contributionMust(t, db.Create(&model.DailyListeningStat{UserID: 1, DayKey: SectDayKey(now.AddDate(0, 0, 1)), CappedSeconds: 3600, EffectiveHours: 1}).Error)
	assertListeningSync(t, now, 15.84)
	assertListeningSync(t, now.AddDate(0, 0, 3), 16.84)
	assertListeningSync(t, now.AddDate(0, 0, 3), 16.84)
}

func TestListeningUpgradeCorrectionRecoveryAndPaymentRetry(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	previous := config.AppConfig
	config.AppConfig = &config.Config{NoticeGroupID: -100}
	t.Cleanup(func() { config.AppConfig = previous })
	contributionMust(t, db.AutoMigrate(&model.SystemConfig{}, &model.RedPacket{}, &model.AutoDeleteMsg{}))
	contributionMust(t, db.Exec("CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL").Error)
	// Start below a minor-stage threshold without fire bonuses.
	contributionMust(t, db.Model(&model.SpiritFireState{}).Where("id=1").UpdateColumn("launch_day", SectDayKey(now.AddDate(0, 0, 2))).Error)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 1, "capped_seconds": 3600}).Error)
	assertListeningSync(t, now, 1)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 4, "capped_seconds": 4 * 3600}).Error)
	assertListeningSync(t, now, 4)
	var events []model.CultivationUpgradeEvent
	contributionMust(t, db.Find(&events).Error)
	if len(events) != 1 {
		t.Fatalf("upgrade events=%+v", events)
	}
	event := events[0]
	// A failed payment-intent commit rolls back the pool injection too.
	contributionMust(t, db.Exec("CREATE TRIGGER reject_upgrade_payment BEFORE UPDATE ON cultivation_upgrade_events BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error)
	if err := settleListeningUpgradeReward(&event); err == nil {
		t.Fatal("expected payment rollback")
	}
	var pool model.SystemConfig
	if err := db.Where("key = ?", "fusion_pool_points").First(&pool).Error; err == nil {
		t.Fatalf("pool escaped rollback: %+v", pool)
	}
	contributionMust(t, db.Exec("DROP TRIGGER reject_upgrade_payment").Error)
	event = events[0]
	contributionMust(t, settleListeningUpgradeReward(&event))
	paid := event.PoolPoints
	if paid < 2 || paid > 4 {
		t.Fatal(event)
	}
	for i := 0; i < 3; i++ {
		event = model.CultivationUpgradeEvent{ID: events[0].ID}
		contributionMust(t, settleListeningUpgradeReward(&event))
	}
	contributionMust(t, db.Where("key = ?", "fusion_pool_points").First(&pool).Error)
	if pool.Value != fmt.Sprint(paid) {
		t.Fatal("duplicate payment", pool.Value, paid)
	}
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 1, "capped_seconds": 3600}).Error)
	assertListeningSync(t, now, 1)
	contributionMust(t, db.Model(&stat).Updates(map[string]any{"effective_hours": 4, "capped_seconds": 4 * 3600}).Error)
	assertListeningSync(t, now, 4)
	var count int64
	contributionMust(t, db.Model(&model.CultivationUpgradeEvent{}).Count(&count).Error)
	if count != 1 {
		t.Fatal("correction recovery duplicated upgrade", count)
	}
	// A failed private notification can retry without re-sending the group or paying.
	var private, group atomic.Int32
	failPrivate := atomic.Bool{}
	failPrivate.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("chat_id") == "1" {
			private.Add(1)
			if failPrivate.Load() {
				fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"blocked"}`)
				return
			}
		} else {
			group.Add(1)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	contributionMust(t, err)
	DeliverListeningUpgrades(bot)
	failPrivate.Store(false)
	contributionMust(t, db.Model(&event).UpdateColumn("next_attempt_at", nil).Error)
	DeliverListeningUpgrades(bot)
	DeliverListeningUpgrades(bot)
	if private.Load() != 2 || group.Load() != 1 {
		t.Fatal("notification retries", private.Load(), group.Load())
	}
	contributionMust(t, db.First(&event, event.ID).Error)
	if !event.Completed || !event.RewardSettled {
		t.Fatal(event)
	}
	contributionMust(t, db.Where("key = ?", "fusion_pool_points").First(&pool).Error)
	if pool.Value != fmt.Sprint(paid) {
		t.Fatal("delivery paid twice", pool.Value)
	}
}

func TestListeningReportReadsSnapshotsWithoutABSOrBatchWait(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	contributionMust(t, db.Model(&stat).UpdateColumn("abs_user_id", "abs1").Error)
	assertListeningSync(t, now, 15.84)
	contributionMust(t, db.Create(&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: SectDayKey(now), RawSeconds: 7200, EffectiveHours: 2, CappedSeconds: 7200, LastFetchedAt: now}).Error)
	assertListeningSync(t, now, 17.84)
	contributionMust(t, saveListeningReportStats(1, "abs1", now))
	var requests atomic.Int32
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"mediaProgress":[{"isFinished":true},{"isFinished":false}]}`)
	})
	refreshListeningReportProgress(1, "abs1", now)
	if requests.Load() != 1 {
		t.Fatal("background progress not fetched")
	}
	listeningRefreshMu.Lock()
	done := make(chan string, 1)
	go func() { done <- GetPersonalReport(abs.DefaultClient, "abs1") }()
	var report string
	select {
	case report = <-done:
		listeningRefreshMu.Unlock()
	case <-time.After(time.Second):
		listeningRefreshMu.Unlock()
		<-done
		t.Fatal("DB report waited for batch")
	}
	if requests.Load() != 1 {
		t.Fatal("report contacted ABS")
	}
	if strings.Contains(report, "累计实际听书") || strings.Contains(report, "累计上报时长") {
		t.Fatalf("report still displayed account-wide ABS playback: %s", report)
	}
	for _, want := range []string{"净修炼时长: 17.84 小时", "今日净修为: 2.00 小时", "已经听完书籍: 1 本", "当前正在收听: 1 本"} {
		if !strings.Contains(report, want) {
			t.Fatalf("missing %q: %s", want, report)
		}
	}
	// Ignore older responses even when SQLite timestamp strings use other offsets.
	contributionMust(t, saveListeningReportStats(1, "abs1", now.Add(-time.Minute).UTC()))
	var snapshot model.ListeningReportSnapshot
	contributionMust(t, db.First(&snapshot, "user_id=1").Error)
	if !snapshot.StatsFetchedAt.Equal(now) {
		t.Fatal("stale metadata overwrote snapshot", snapshot)
	}
}

func TestUnchangedHistoryDoesNotRecalculateSettledDays(t *testing.T) {
	db, now, _ := setupListeningContributionTest(t)
	assertListeningSync(t, now, 15.84)
	reads, writes := int64(0), int64(0)
	contributionMust(t, db.Callback().Query().After("gorm:query").Register("count_settled_days", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			reads += tx.RowsAffected
		}
	}))
	contributionMust(t, db.Callback().Update().After("gorm:update").Register("count_settled_writes", func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_listening_stats" {
			writes += tx.RowsAffected
		}
	}))
	defer db.Callback().Query().Remove("count_settled_days")
	defer db.Callback().Update().Remove("count_settled_writes")
	assertListeningSync(t, now, 15.84)
	if reads != 0 || writes != 0 {
		t.Fatalf("unchanged historic days: reads=%d writes=%d", reads, writes)
	}
}

func TestIncrementalListeningSettlementStaysWithinUser(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	assertListeningSync(t, now, 15.84)
	other := model.DailyListeningStat{
		UserID: 2, DayKey: stat.DayKey, RawSeconds: 3600,
		CappedSeconds: 3600, EffectiveHours: 1,
	}
	contributionMust(t, db.Create(&other).Error)
	contributionMust(t, db.Model(&stat).UpdateColumn("effective_hours", 6).Error)
	if _, ok := syncCultivationFromDailyListeningStatsAt(1, now); !ok {
		t.Fatal("user 1 settlement failed")
	}
	contributionMust(t, db.First(&other, other.ID).Error)
	if other.CultivationApplied || other.BaseCultivationHours != 0 || other.AppliedTotalHours != 0 {
		t.Fatalf("user 1 settlement changed user 2's record: %+v", other)
	}
}

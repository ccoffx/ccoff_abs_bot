package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/activity"
	configpkg "ccoff-abs-bot/internal/config"
	modelpkg "ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupTreasureTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "treasure.db")
	database, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	previous := storage.DB
	previousConfig := configpkg.AppConfig
	storage.DB = database
	configpkg.AppConfig = &configpkg.Config{}
	t.Cleanup(func() { storage.DB = previous; configpkg.AppConfig = previousConfig; _ = sqlDB.Close() })
	if err := storage.DB.AutoMigrate(&modelpkg.TreasureRound{}, &modelpkg.TreasurePick{}, &modelpkg.User{}, &modelpkg.PointTransaction{}, &modelpkg.SystemConfig{}, &modelpkg.RedPacket{}, &modelpkg.AutoDeleteMsg{}, &modelpkg.SecurityAttemptLock{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_treasure_active_chat ON treasure_rounds(chat_id) WHERE status IN ('publishing','active')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_system_configs_key ON system_configs(key) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 12; id++ {
		if err := storage.DB.Create(&modelpkg.User{TelegramID: id, Username: fmt.Sprintf("player%d", id), Points: 100}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func newTreasureTestRound(t *testing.T, chatID int64) modelpkg.TreasureRound {
	t.Helper()
	round, err := activity.CreateTreasureRound(chatID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 固定布局仅用于验证确切的积分守恒，生产使用密码学随机洗牌。
	if err := storage.DB.Model(&round).Update("rewards_json", "[0,0,0,0,2,2,2,5,10]").Error; err != nil {
		t.Fatal(err)
	}
	if err := activity.ActivateTreasureRound(round.ID, 123, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&round, round.ID).Error; err != nil {
		t.Fatal(err)
	}
	return round
}

func treasureTestPick(t *testing.T, round modelpkg.TreasureRound, box int, userID int64) modelpkg.TreasurePick {
	t.Helper()
	pick, err := activity.PickTreasureBox(round.ID, round.ChatID, round.MessageID, box, &tgbotapi.User{ID: userID, FirstName: fmt.Sprintf("User%d", userID)})
	if err != nil {
		t.Fatal(err)
	}
	return pick
}

func treasureTestBalance(t *testing.T, id int64) int {
	t.Helper()
	var user modelpkg.User
	if err := storage.DB.Where("telegram_id = ?", id).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user.Points
}

func treasureTestCount(t *testing.T, model interface{}) int64 {
	t.Helper()
	var count int64
	if err := storage.DB.Model(model).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func treasureTestPool(t *testing.T) int {
	t.Helper()
	var config modelpkg.SystemConfig
	if err := storage.DB.Where("key = ?", "fusion_pool_points").First(&config).Error; err != nil {
		t.Fatal(err)
	}
	var pool int
	if _, err := fmt.Sscan(config.Value, &pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestTreasureFullRoundConservationAndIdempotency(t *testing.T) {
	setupTreasureTestDB(t)
	round := newTreasureTestRound(t, -100)
	rewards, err := activity.TreasureRewards(round)
	if err != nil {
		t.Fatal(err)
	}
	for box, reward := range rewards {
		pick := treasureTestPick(t, round, box, int64(box+1))
		if pick.Reward != reward || treasureTestBalance(t, int64(box+1)) != 100-3+reward {
			t.Fatalf("incorrect payout for box %d", box)
		}
	}
	closed, burst, err := activity.CloseTreasureRound(round.ID, time.Now())
	if err != nil || !closed || burst {
		t.Fatalf("close = %v %v %v", closed, burst, err)
	}
	if treasureTestPool(t) != 6 {
		t.Fatal("full round must put 6 points into fusion pool")
	}
	closed, burst, err = activity.CloseTreasureRound(round.ID, time.Now())
	if err != nil || closed || burst || treasureTestPool(t) != 6 {
		t.Fatal("duplicate settlement changed pool")
	}
	if _, err := activity.PickTreasureBox(round.ID, round.ChatID, 123, 0, &tgbotapi.User{ID: 10}); !errors.Is(err, activity.ErrTreasureExpired) {
		t.Fatal(err)
	}
	if treasureTestCount(t, &modelpkg.TreasurePick{}) != 9 || treasureTestCount(t, &modelpkg.PointTransaction{}) != 14 {
		t.Fatal("unexpected pick or ledger count")
	}
}

func TestTreasureGuardsAndAtomicRollback(t *testing.T) {
	for _, scenario := range []string{"insufficient funds", "reward ledger failure", "taken box", "duplicate user", "wrong chat", "wrong message", "expired", "invalid box", "corrupt rewards"} {
		t.Run(scenario, func(t *testing.T) {
			setupTreasureTestDB(t)
			round := newTreasureTestRound(t, -100)
			chat, message, box, user := round.ChatID, round.MessageID, 8, int64(1)
			var expected error
			switch scenario {
			case "insufficient funds":
				if err := storage.DB.Model(&modelpkg.User{}).Where("telegram_id = ?", 1).Update("points", 2).Error; err != nil {
					t.Fatal(err)
				}
				expected = storage.ErrPointsNotEnough
			case "reward ledger failure":
				if err := storage.DB.Exec(`CREATE TRIGGER fail_reward BEFORE INSERT ON point_transactions WHEN NEW.type = 'treasure_reward' BEGIN SELECT RAISE(ABORT, 'ledger failed'); END`).Error; err != nil {
					t.Fatal(err)
				}
			case "taken box":
				treasureTestPick(t, round, box, 2)
				expected = activity.ErrTreasureBoxTaken
			case "duplicate user":
				treasureTestPick(t, round, 0, user)
				expected = activity.ErrTreasurePicked
			case "wrong chat":
				chat = -200
				expected = activity.ErrTreasurePanel
			case "wrong message":
				message++
				expected = activity.ErrTreasurePanel
			case "expired":
				if err := storage.DB.Model(&round).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
				expected = activity.ErrTreasureExpired
			case "invalid box":
				box = -1
				expected = activity.ErrTreasurePanel
			case "corrupt rewards":
				if err := storage.DB.Model(&round).Update("rewards_json", "[100]").Error; err != nil {
					t.Fatal(err)
				}
			}
			beforeBalance := treasureTestBalance(t, user)
			beforePicks := treasureTestCount(t, &modelpkg.TreasurePick{})
			beforeLedger := treasureTestCount(t, &modelpkg.PointTransaction{})
			_, err := activity.PickTreasureBox(round.ID, chat, message, box, &tgbotapi.User{ID: user})
			if err == nil || (expected != nil && !errors.Is(err, expected)) {
				t.Fatalf("unexpected error %v, want %v", err, expected)
			}
			if treasureTestBalance(t, user) != beforeBalance || treasureTestCount(t, &modelpkg.TreasurePick{}) != beforePicks || treasureTestCount(t, &modelpkg.PointTransaction{}) != beforeLedger {
				t.Fatal("failed pick changed assets or records")
			}
		})
	}
}

func TestTreasureConcurrentClaims(t *testing.T) {
	for _, sameUser := range []bool{false, true} {
		t.Run(fmt.Sprint(sameUser), func(t *testing.T) {
			setupTreasureTestDB(t)
			round := newTreasureTestRound(t, -100)
			var wg sync.WaitGroup
			for i := 0; i < 9; i++ {
				wg.Go(func() {
					box, user := 8, int64(i+1)
					if sameUser {
						box, user = i, 1
					}
					_, err := activity.PickTreasureBox(round.ID, round.ChatID, 123, box, &tgbotapi.User{ID: user})
					if err != nil && !errors.Is(err, activity.ErrTreasurePicked) && !errors.Is(err, activity.ErrTreasureBoxTaken) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if treasureTestCount(t, &modelpkg.TreasurePick{}) != 1 {
				t.Fatal("concurrent clicks claimed more than one pick")
			}
			var picks []modelpkg.TreasurePick
			if err := storage.DB.Find(&picks).Error; err != nil {
				t.Fatal(err)
			}
			pick := picks[0]
			if treasureTestBalance(t, pick.UserID) != 100-3+pick.Reward {
				t.Fatal("concurrent clicks caused extra charges")
			}
		})
	}
}

func TestTreasurePartialSettlementAndPoolRollback(t *testing.T) {
	for _, scenario := range []string{"loss", "subsidy", "pool burst", "settlement failure"} {
		t.Run(scenario, func(t *testing.T) {
			setupTreasureTestDB(t)
			if err := storage.DB.Create(&modelpkg.SystemConfig{Key: "fusion_pool_points", Value: "298"}).Error; err != nil {
				t.Fatal(err)
			}
			round := newTreasureTestRound(t, -100)
			box := 0
			if scenario == "subsidy" {
				box = 8
			}
			treasureTestPick(t, round, box, 1)
			if closed, _, err := activity.CloseTreasureRound(round.ID, time.Now()); err != nil || closed {
				t.Fatal("round closed before expiry")
			}
			if scenario == "settlement failure" {
				if err := storage.DB.Exec(`CREATE TRIGGER fail_close BEFORE UPDATE ON treasure_rounds WHEN NEW.status = 'closed' BEGIN SELECT RAISE(ABORT, 'close failed'); END`).Error; err != nil {
					t.Fatal(err)
				}
				closed, burst, err := activity.CloseTreasureRound(round.ID, round.ExpiresAt)
				if err == nil || closed || burst || treasureTestPool(t) != 298 || treasureTestCount(t, &modelpkg.RedPacket{}) != 0 {
					t.Fatal("failed settlement leaked pool changes or red packet")
				}
				if err := storage.DB.Exec("DROP TRIGGER fail_close").Error; err != nil {
					t.Fatal(err)
				}
			}
			closed, burst, err := activity.CloseTreasureRound(round.ID, round.ExpiresAt)
			if err != nil || !closed {
				t.Fatal(err)
			}
			if scenario == "subsidy" {
				if burst || treasureTestPool(t) != 298 || treasureTestBalance(t, 1) != 107 {
					t.Fatal("subsidy deducted fusion pool")
				}
			} else if !burst || treasureTestPool(t) != 1 || treasureTestCount(t, &modelpkg.RedPacket{}) != 1 {
				t.Fatal("lost points did not reach fusion pool exactly once")
			}
		})
	}
}

func TestTreasureRestartPreservesLayoutAndRewards(t *testing.T) {
	path := setupTreasureTestDB(t)
	round := newTreasureTestRound(t, -100)
	treasureTestPick(t, round, 8, 1)
	reopened, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	previous := storage.DB
	storage.DB = reopened
	t.Cleanup(func() { sqlDB, _ := reopened.DB(); _ = sqlDB.Close(); storage.DB = previous })
	if _, err := activity.PickTreasureBox(round.ID, round.ChatID, 123, 0, &tgbotapi.User{ID: 1}); !errors.Is(err, activity.ErrTreasurePicked) {
		t.Fatal("restart allowed second pick", err)
	}
	pick := treasureTestPick(t, round, 7, 2)
	if pick.Reward != 5 || treasureTestBalance(t, 1) != 107 {
		t.Fatal("restart redrew rewards or duplicated payout")
	}
	if closed, _, err := activity.CloseTreasureRound(round.ID, round.ExpiresAt); err != nil || !closed {
		t.Fatal(err)
	}
	if treasureTestBalance(t, 2) != 102 {
		t.Fatal("restart settlement changed settled pick payout")
	}
}

func TestTreasureRoundLimitsAndPanelPrivacy(t *testing.T) {
	setupTreasureTestDB(t)
	start := time.Date(2026, 10, 27, 15, 50, 0, 0, time.UTC)
	round, err := activity.CreateTreasureRound(-100, start)
	if err != nil || round.DayKey != "2026-10-27" {
		t.Fatal(err, round.DayKey)
	}
	if _, err := activity.CreateTreasureRound(-100, start); !errors.Is(err, activity.ErrTreasureBusy) {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&round).Updates(map[string]interface{}{"status": activity.TreasureClosed, "closed_at": start}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := activity.CreateTreasureRound(-100, start.Add(time.Second)); !errors.Is(err, activity.ErrTreasureCooldown) {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		now := start.Add(time.Duration(i) * 2 * time.Minute)
		round, err = activity.CreateTreasureRound(-100, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.DB.Model(&round).Updates(map[string]interface{}{"status": activity.TreasureClosed, "closed_at": now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := activity.CreateTreasureRound(-100, start.Add(6*time.Minute)); !errors.Is(err, activity.ErrTreasureDailyLimit) {
		t.Fatal(err)
	}
	round, err = activity.CreateTreasureRound(-100, start.Add(11*time.Minute))
	if err != nil || round.DayKey != "2026-10-28" {
		t.Fatal("Beijing midnight did not reset quota", err)
	}
	text, markup, err := activity.RenderTreasurePanel(round, nil)
	if err != nil || len(markup.InlineKeyboard) != 3 || strings.Contains(text, round.RewardsJSON) {
		t.Fatal("invalid or leaking active panel", err)
	}
	for _, row := range markup.InlineKeyboard {
		if len(row) != 3 {
			t.Fatal("panel is not 3x3")
		}
		for _, button := range row {
			if !strings.Contains(button.Text, "3分") || len(*button.CallbackData) > 64 {
				t.Fatal("invalid unopened box")
			}
		}
	}
	round.Status = activity.TreasureClosed
	_, closedMarkup, err := activity.RenderTreasurePanel(round, nil)
	if err != nil || !strings.Contains(closedMarkup.InlineKeyboard[0][0].Text, "未开") {
		t.Fatal("closed panel did not reveal unopened box")
	}
	for range 20 {
		rewards, err := activity.ShuffledTreasureRewards()
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(rewards)
		if _, err := activity.TreasureRewards(modelpkg.TreasureRound{RewardsJSON: string(encoded), EntryCost: 3}); err != nil {
			t.Fatal("invalid shuffled reward distribution", err)
		}
	}
}

func TestTreasureFreshDatabaseInitialization(t *testing.T) {
	// InitDB 的建库失败会 log.Fatal，放在子进程中校验完整首次启动。
	if os.Getenv("TREASURE_INITIALIZATION_TEST_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestTreasureFreshDatabaseInitialization$")
		command.Env = append(os.Environ(), "TREASURE_INITIALIZATION_TEST_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fresh database initialization failed: %v\n%s", err, output)
		}
		return
	}
	configpkg.AppConfig = &configpkg.Config{DbURL: filepath.Join(t.TempDir(), "fresh.db"), DatabaseMaxOpenConns: 1, DatabaseMaxIdleConns: 1, DatabaseBusyTimeoutMS: 1000, SecurityPepper: strings.Repeat("s", 64), BackupEncryptKey: strings.Repeat("b", 64)}
	InitDB()
	for _, table := range []string{"treasure_rounds", "treasure_picks"} {
		if !storage.DB.Migrator().HasTable(table) {
			t.Fatalf("missing table %s", table)
		}
	}
	if err := storage.DB.Create(&modelpkg.User{TelegramID: 1, Username: "fresh_player", Points: 100}).Error; err != nil {
		t.Fatal(err)
	}
	round := newTreasureTestRound(t, -100)
	treasureTestPick(t, round, 0, 1)
}

package activity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupKeywordDB(t *testing.T) *gorm.DB {
	t.Helper()
	old := storage.DB
	oldConfig := config.AppConfig
	config.AppConfig = &config.Config{SecurityPepper: strings.Repeat("s", 64), AdminIDs: map[int64]bool{1: true}}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "lottery.db")+"?_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	storage.DB = db
	t.Cleanup(func() {
		storage.DB = old
		config.AppConfig = oldConfig
		sqlDB.Close()
	})
	if err := db.AutoMigrate(&model.LotteryActivity{}, &model.LotteryParticipant{}, &model.User{}, &model.PointTransaction{}, &model.AutoDeleteMsg{}, &model.LotteryPrize{}, &model.AuditLog{}, &model.LotteryWinner{}, &model.LotteryClaimLog{}, &model.Inventory{}, &model.InviteCode{}, &model.RenewCode{}, &model.SystemConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, index := range storage.DatabaseIndexes {
		if strings.Contains(index, "idx_lottery_active_join_keyword") || strings.Contains(index, "idx_lottery_participants_activity_user_unique") || strings.Contains(index, "idx_lottery_winners_activity_user_unique") || strings.Contains(index, "idx_inventory_user_item_unique") {
			if err := db.Exec(index).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func newKeywordLottery(keyword string) model.LotteryActivity {
	return model.LotteryActivity{Title: "测试活动", JoinKeyword: keyword, Mode: lotteryModeCount, Status: lotteryStatusActive, ParticipantLimit: 10}
}

func TestLotteryKeywordUniqueAndReusable(t *testing.T) {
	db := setupKeywordDB(t)
	a := newKeywordLottery("秋日好运")
	if err := db.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			b := newKeywordLottery("秋日好运")
			if db.Create(&b).Error == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 0 {
		t.Fatal("concurrent duplicate keyword accepted")
	}
	if err := db.Model(&a).Update("status", lotteryStatusDrawing).Error; err != nil {
		t.Fatal(err)
	}
	b := newKeywordLottery("秋日好运")
	if db.Create(&b).Error == nil {
		t.Fatal("drawing keyword must remain reserved")
	}
	for _, status := range []string{lotteryStatusDrawn, lotteryStatusClosed} {
		if err := db.Model(&a).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		b = newKeywordLottery("秋日好运")
		if err := db.Create(&b).Error; err != nil {
			t.Fatalf("reuse after %s: %v", status, err)
		}
		if err := db.Delete(&b).Error; err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		b = newKeywordLottery("")
		if err := db.Create(&b).Error; err != nil {
			t.Fatal("legacy blank keywords conflict", err)
		}
	}
}

func TestLotteryKeywordJoinAccounting(t *testing.T) {
	db := setupKeywordDB(t)
	lot := newKeywordLottery("秋日好运")
	lot.EntryCost = 5
	lot.ParticipantLimit = 2
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	for i, points := range []int{10, 0, 10} {
		if err := db.Create(&model.User{TelegramID: int64(i + 1), Username: fmt.Sprint("user", i), Points: points}).Error; err != nil {
			t.Fatal(err)
		}
	}
	user := func(id int64) *tgbotapi.User { return &tgbotapi.User{ID: id, FirstName: "用户"} }
	if n, draw, err := joinLotteryActivity(lot.ID, user(1), -100); err != nil || n != 1 || draw {
		t.Fatalf("join: %d %v %v", n, draw, err)
	}
	if _, _, err := joinLotteryActivity(lot.ID, user(1), 0); !errors.Is(err, storage.ErrLotteryAlreadyJoined) {
		t.Fatalf("repeat: %v", err)
	}
	if _, _, err := joinLotteryActivity(lot.ID, user(2), 0); !errors.Is(err, storage.ErrPointsNotEnough) {
		t.Fatalf("balance: %v", err)
	}
	if n, draw, err := joinLotteryActivity(lot.ID, user(3), 0); err != nil || n != 2 || !draw {
		t.Fatalf("full draw: %d %v %v", n, draw, err)
	}
	if _, _, err := joinLotteryActivity(lot.ID, user(4), 0); !errors.Is(err, storage.ErrLotteryFull) {
		t.Fatalf("full: %v", err)
	}
	var u model.User
	db.Where("telegram_id = ?", 1).First(&u)
	if u.Points != 5 {
		t.Fatalf("double charge: %d", u.Points)
	}
	var n int64
	db.Model(&model.LotteryParticipant{}).Where("activity_id = ?", lot.ID).Count(&n)
	if n != 2 {
		t.Fatalf("failed join not rolled back: %d", n)
	}
	past := time.Now().Add(-time.Minute)
	db.Model(&lot).Updates(map[string]interface{}{"mode": lotteryModeTime, "draw_at": past})
	if _, _, err := joinLotteryActivity(lot.ID, user(4), 0); !errors.Is(err, storage.ErrLotteryWaitingDraw) {
		t.Fatalf("expired: %v", err)
	}
}

func TestLotteryCreationWithoutKeyword(t *testing.T) {
	db := setupKeywordDB(t)
	session := &telegram.SessionState{}
	for key, value := range map[string]string{"lottery_title": "秋日福利", "lottery_join_keyword": "秋日好运", "lottery_mode": lotteryModeCount, "lottery_limit": "10", "lottery_entry_cost": "0", "lottery_prizes": "积分 100 1"} {
		session.SetTemp(key, value)
	}
	specs, err := parseLotteryPrizeSpecs(session.GetTemp("lottery_prizes"))
	if err != nil {
		t.Fatal(err)
	}
	confirmation := formatLotteryCreateConfirm(session, specs)
	if strings.Contains(confirmation, "秋日好运") || strings.Contains(confirmation, "口令") || strings.Contains(confirmation, "暗号") {
		t.Fatalf("confirmation contains obsolete input: %q", confirmation)
	}
	id, err := createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "管理员"}}, session)
	if err != nil {
		t.Fatal(err)
	}
	var got model.LotteryActivity
	if err := db.First(&got, id).Error; err != nil {
		t.Fatal(err)
	}
	if got.JoinKeyword != "" {
		t.Fatalf("independent fields missing: %+v", got)
	}
	_, err = createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}}, session)
	if err != nil {
		t.Fatalf("duplicate creation error: %v", err)
	}
}

func TestLotteryStuckDrawingRecovery(t *testing.T) {
	for _, trigger := range []string{"manual", "scheduler_count", "scheduler_time"} {
		t.Run(trigger, func(t *testing.T) {
			db := setupKeywordDB(t)
			bot, _ := newClaimBot(t)
			lot := newKeywordLottery("恢复抽奖")
			lot.Status = lotteryStatusDrawing
			if trigger == "scheduler_time" {
				lot.Mode = lotteryModeTime
				past := time.Now().Add(-time.Minute)
				lot.DrawAt = &past
			}
			if err := db.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.LotteryParticipant{ActivityID: lot.ID, UserID: 101, JoinedAt: time.Now()}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.LotteryPrize{ActivityID: lot.ID, PrizeType: lotteryPrizePoints, Amount: 10, Quantity: 1}).Error; err != nil {
				t.Fatal(err)
			}
			if trigger == "manual" {
				if err := drawLotteryActivity(bot, lot.ID, "manual", 1); err != nil {
					t.Fatal(err)
				}
			} else {
				runLotterySchedulerTick(bot)
			}
			if err := db.First(&lot, lot.ID).Error; err != nil {
				t.Fatal(err)
			}
			if lot.Status != lotteryStatusDrawn || lot.WinnersCount != 1 {
				t.Fatalf("draw did not recover: %+v", lot)
			}
			if err := drawLotteryActivity(bot, lot.ID, "manual", 1); !errors.Is(err, storage.ErrLotteryNotActive) {
				t.Fatalf("repeated draw: %v", err)
			}
			assertRewardCount(t, &model.LotteryWinner{}, 1)
			replacement := newKeywordLottery(lot.JoinKeyword)
			if err := createLotteryActivityInTx(db, &replacement); err != nil {
				t.Fatalf("keyword not released: %v", err)
			}
		})
	}
}

func TestLotteryStuckDrawingRejectsExistingResults(t *testing.T) {
	for _, evidence := range []string{"drawn_at", "winner", "deleted_winner", "fusion_pool_points"} {
		t.Run(evidence, func(t *testing.T) {
			db := setupKeywordDB(t)
			bot, _ := newClaimBot(t)
			lot := newKeywordLottery("保护开奖结果")
			lot.Status = lotteryStatusDrawing
			if evidence == "drawn_at" {
				now := time.Now()
				lot.DrawnAt = &now
			}
			if evidence == "fusion_pool_points" {
				lot.FusionPoolPoints = 1
			}
			if err := db.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			if evidence == "winner" || evidence == "deleted_winner" {
				winner := model.LotteryWinner{ActivityID: lot.ID, UserID: 101, PrizeType: lotteryPrizePoints, Amount: 10}
				if err := db.Create(&winner).Error; err != nil {
					t.Fatal(err)
				}
				if evidence == "deleted_winner" {
					if err := db.Delete(&winner).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := drawLotteryActivity(bot, lot.ID, "manual", 1); err == nil || !strings.Contains(err.Error(), "不能重复开奖") {
				t.Fatalf("unsafe recovery accepted: %v", err)
			}
			if err := db.First(&lot, lot.ID).Error; err != nil {
				t.Fatal(err)
			}
			if lot.Status != lotteryStatusDrawing {
				t.Fatalf("unsafe recovery changed status: %s", lot.Status)
			}
		})
	}
}

func TestLotteryTextEntryRemovedAndButtonsAvailable(t *testing.T) {
	db := setupKeywordDB(t)
	bot, capture := newClaimBot(t)
	lot := newKeywordLottery("历史口令")
	lot.AnnounceChatID = -100
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	session := &telegram.SessionState{}
	session.SetStep("IDLE")
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 101}, Chat: &tgbotapi.Chat{ID: 101, Type: "private"}}
	for _, chat := range []*tgbotapi.Chat{msg.Chat, {ID: -100, Type: "supergroup"}} {
		msg.Chat = chat
		for _, input := range []string{lot.JoinKeyword, fmt.Sprintf("参加抽奖 %d", lot.ID), fmt.Sprintf("/join_lottery %d", lot.ID), "领取奖励"} {
			if HandleLotteryCommand(bot, msg, input, session, "user") {
				t.Fatalf("text entry still active: %s", input)
			}
		}
	}
	assertRewardCount(t, &model.LotteryParticipant{}, 0)
	showActiveLotteries(bot, 101)
	announceLotteryCreated(bot, 0, lot.ID)
	messages := capture.snapshot()
	if len(messages) != 2 {
		t.Fatalf("expected list and group announcement: %+v", messages)
	}
	for _, m := range messages {
		if strings.Contains(m.Text, lot.JoinKeyword) || !strings.Contains(m.Markup, fmt.Sprintf("lottery_%d", lot.ID)) {
			t.Fatalf("missing button or exposed keyword: %+v", m)
		}
	}
	if !strings.Contains(messages[0].Markup, "lottery_claim") {
		t.Fatal("list has no claim button")
	}
	// The same legacy activity remains available through the button preview.
	msg.Chat = &tgbotapi.Chat{ID: 101, Type: "private"}
	HandleLotteryStart(bot, msg, fmt.Sprintf("/start lottery_%d", lot.ID))
	assertRewardCount(t, &model.LotteryParticipant{}, 0)
	messages = capture.snapshot()
	if !strings.Contains(messages[len(messages)-1].Markup, fmt.Sprintf("lottery_enter:%d:101", lot.ID)) {
		t.Fatal("missing confirmation button")
	}
}

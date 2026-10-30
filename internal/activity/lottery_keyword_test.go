package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

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

func TestLotteryKeywordValidation(t *testing.T) {
	for _, s := range []string{"秋日好运", "好运 🎉", "好运`\\_*"} {
		if err := validateLotteryJoinKeyword(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "好", strings.Repeat("好", 41), "１２３", "1234", "好运\n来", "好运\t来", "好运\u200b来", "取消", "返回好运", "生成白名单卡", "参加抽奖 1", "/start", "开启宗门秘境", "确认购买商品", "领取奖励", "任命长老 123456789", "举报订单 1 卡密失效"} {
		if err := validateLotteryJoinKeyword(s); err == nil {
			t.Errorf("accepted reserved/invalid keyword %q", s)
		}
	}
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

func TestLotteryKeywordRoutingAndCopyableReply(t *testing.T) {
	db := setupKeywordDB(t)
	lot := newKeywordLottery("秋日好运`\\")
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	var outputMu sync.Mutex
	var lastText string
	var lastEntities []tgbotapi.MessageEntity
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		outputMu.Lock()
		if r.Form.Get("text") != "" {
			lastText = r.Form.Get("text")
			json.Unmarshal([]byte(r.Form.Get("entities")), &lastEntities)
		}
		outputMu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	session := &telegram.SessionState{}
	session.SetStep("WAITING_REG_USER")
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 10, FirstName: "测试"}, Chat: &tgbotapi.Chat{ID: 10, Type: "private"}}
	if HandleLotteryCommand(bot, msg, lot.JoinKeyword, session, "user") {
		t.Fatal("active form consumed keyword")
	}
	session.SetStep("IDLE")
	if !HandleLotteryCommand(bot, msg, "  "+lot.JoinKeyword+"  ", session, "user") {
		t.Fatal("private keyword not routed")
	}
	msg.From.ID = 11
	msg.Chat = &tgbotapi.Chat{ID: -100, Type: "supergroup"}
	if !HandleLotteryCommand(bot, msg, lot.JoinKeyword, session, "user") {
		t.Fatal("group keyword not routed")
	}
	var n int64
	db.Model(&model.LotteryParticipant{}).Count(&n)
	if n != 2 {
		t.Fatalf("participants: %d", n)
	}
	showActiveLotteries(bot, 10)
	outputMu.Lock()
	if !strings.Contains(lastText, lot.JoinKeyword) || strings.ContainsAny(lastText, "\x02\x03") || len(lastEntities) == 0 {
		t.Fatalf("list not copyable: %q %+v", lastText, lastEntities)
	}
	outputMu.Unlock()
	// Both old syntaxes remain usable for historical announcements.
	for i, command := range []string{fmt.Sprintf("参加抽奖 %d", lot.ID), fmt.Sprintf("/join_lottery %d", lot.ID)} {
		msg.From.ID = int64(20 + i)
		msg.Chat = &tgbotapi.Chat{ID: msg.From.ID, Type: "private"}
		if !HandleLotteryCommand(bot, msg, command, session, "user") {
			t.Fatalf("legacy command not routed: %s", command)
		}
	}
	db.Model(&model.LotteryParticipant{}).Count(&n)
	if n != 4 {
		t.Fatalf("legacy participants: %d", n)
	}
	config.AppConfig.NoticeGroupID = -100
	announceLotteryCreated(bot, 0, lot.ID)
	outputMu.Lock()
	defer outputMu.Unlock()
	if !strings.HasSuffix(lastText, "参与方式：发送 "+lot.JoinKeyword) || len(lastEntities) != 1 {
		t.Fatalf("group announcement: %q %+v", lastText, lastEntities)
	}
	e := lastEntities[0]
	units := utf16.Encode([]rune(lastText))
	if got := string(utf16.Decode(units[e.Offset : e.Offset+e.Length])); got != lot.JoinKeyword {
		t.Fatalf("group copied %q", got)
	}
}

func TestLotteryKeywordMigrationPreservesLegacyActivity(t *testing.T) {
	db := setupKeywordDB(t)
	legacy := newKeywordLottery("")
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"DROP INDEX idx_lottery_active_join_keyword",
		"DROP INDEX idx_lottery_activities_join_keyword",
		"ALTER TABLE lottery_activities DROP COLUMN join_keyword",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.AutoMigrate(&model.LotteryActivity{}); err != nil {
			t.Fatal(err)
		}
		for _, index := range storage.DatabaseIndexes {
			if strings.Contains(index, "idx_lottery_active_join_keyword") {
				if err := db.Exec(index).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var got model.LotteryActivity
	if err := db.First(&got, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Title != legacy.Title || got.JoinKeyword != "" || lotteryJoinInput(got) != fmt.Sprintf("参加抽奖 %d", legacy.ID) {
		t.Fatalf("migration changed legacy data: %+v", got)
	}
	if n, _, err := joinLotteryActivity(got.ID, &tgbotapi.User{ID: 1}, 0); err != nil || n != 1 {
		t.Fatalf("legacy activity cannot be joined: %d %v", n, err)
	}
}

func TestLotteryCreationPersistsKeywordWithoutClaimCode(t *testing.T) {
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
	if !strings.Contains(confirmation, "秋日好运") || strings.Contains(confirmation, "暗号") {
		t.Fatalf("confirmation leaked claim secret or lost keyword: %q", confirmation)
	}
	id, err := createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1, FirstName: "管理员"}}, session)
	if err != nil {
		t.Fatal(err)
	}
	var got model.LotteryActivity
	if err := db.First(&got, id).Error; err != nil {
		t.Fatal(err)
	}
	if got.JoinKeyword != "秋日好运" {
		t.Fatalf("independent fields missing: %+v", got)
	}
	_, err = createLotteryActivityFromSession(&tgbotapi.Message{From: &tgbotapi.User{ID: 1}}, session)
	if err == nil || !strings.Contains(err.Error(), "参与口令正在使用") {
		t.Fatalf("duplicate creation error: %v", err)
	}
}

func TestLotteryKeywordWizardAfterActivityEnds(t *testing.T) {
	for _, ending := range []string{"draw", "empty_draw", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			db := setupKeywordDB(t)
			bot, capture := newClaimBot(t)
			lot := newKeywordLottery("秋日好运")
			lot.Mode = lotteryModeTime
			past := time.Now().Add(-time.Minute)
			lot.DrawAt = &past
			if err := db.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
			session := &telegram.SessionState{}
			session.SetStep("WAITING_LOTTERY_JOIN_KEYWORD")
			handleLotteryCreateStep(bot, msg, lot.JoinKeyword, session, "super_admin")
			if session.GetStep() != "WAITING_LOTTERY_JOIN_KEYWORD" {
				t.Fatal("overdue activity released keyword before completing draw")
			}
			messages := capture.snapshot()
			last := messages[len(messages)-1].Text
			for _, want := range []string{fmt.Sprintf("抽奖 #%d", lot.ID), "进行中", "尚未完成开奖"} {
				if !strings.Contains(last, want) {
					t.Fatalf("conflict message lacks %q: %s", want, last)
				}
			}
			if ending == "draw" {
				if err := db.Create(&model.LotteryParticipant{ActivityID: lot.ID, UserID: 101, JoinedAt: time.Now()}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&model.LotteryPrize{ActivityID: lot.ID, PrizeType: lotteryPrizePoints, Amount: 10, Quantity: 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if ending == "cancel" {
				if _, err := cancelLotteryActivityWithFullRefund(lot.ID, 1); err != nil {
					t.Fatal(err)
				}
			} else if err := drawLotteryActivity(bot, lot.ID, "scheduled", 0); err != nil {
				t.Fatal(err)
			}
			handleLotteryCreateStep(bot, msg, lot.JoinKeyword, session, "super_admin")
			if session.GetStep() != "WAITING_LOTTERY_MODE" {
				t.Fatal("ended activity still blocks keyword in wizard")
			}
			replacement := newKeywordLottery(lot.JoinKeyword)
			if err := db.Transaction(func(tx *gorm.DB) error { return createLotteryActivityInTx(tx, &replacement) }); err != nil {
				t.Fatalf("ended activity still blocks creation: %v", err)
			}
		})
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

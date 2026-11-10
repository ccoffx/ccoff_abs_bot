package activity

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func openTribulationTestDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func setupTribulationTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tribulation.db")
	previous, previousConfig := storage.DB, config.AppConfig
	storage.DB = openTribulationTestDB(t, path)
	config.AppConfig = &config.Config{NoticeGroupID: -100}
	t.Cleanup(func() { storage.DB = previous; config.AppConfig = previousConfig })
	if err := storage.DB.AutoMigrate(&model.TribulationRound{}, &model.TribulationEntry{}, &model.TribulationTitleStat{}, &model.TribulationTitleHolder{}, &model.TribulationTitleHistory{}, &model.TitleEquipment{}, &model.TitleGroup{}, &model.TitleMember{}, &model.TitleMemberSyncRequest{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.Sect{}, &model.Cultivation{}, &model.User{}, &model.PointTransaction{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(&model.DailyListeningStat{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_tribulation_active_chat ON tribulation_rounds(chat_id) WHERE status IN ('publishing','active','settling')`).Error; err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 12; id++ {
		if err := storage.DB.Create(&model.User{TelegramID: id, Username: fmt.Sprintf("player%d", id), Points: 100}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func newTribulationTestRound(t *testing.T, chatID int64, now time.Time) model.TribulationRound {
	t.Helper()
	round, err := CreateTribulationRound(chatID, "测试群", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateTribulationRound(round.ID, int(round.ID)+100, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.First(&round, round.ID).Error; err != nil {
		t.Fatal(err)
	}
	return round
}

func tribulationTestChoose(t *testing.T, r model.TribulationRound, id int64, side string, switching bool, now time.Time) model.TribulationEntry {
	t.Helper()
	entry, err := ChooseTribulationSide(r.ID, r.ChatID, r.MessageID, side, switching, &tgbotapi.User{ID: id, FirstName: fmt.Sprintf("玩家%d", id)}, now)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func tribulationTestBalance(t *testing.T, id int64) int {
	t.Helper()
	var u model.User
	if err := storage.DB.Where("telegram_id = ?", id).First(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.Points
}

func tribulationTestCount(t *testing.T, value any) int64 {
	t.Helper()
	var n int64
	if err := storage.DB.Model(value).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func settleTribulationTestRound(t *testing.T, r model.TribulationRound, winner string) {
	t.Helper()
	// 固定结果只用于验证两种结果的金额与荣誉归属，生产开奖使用 crypto/rand。
	if err := storage.DB.Model(&r).Updates(map[string]any{"status": TribulationSettling, "winning_side": winner}).Error; err != nil {
		t.Fatal(err)
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt); err != nil || !closed {
		t.Fatalf("close=%v err=%v", closed, err)
	}
}

func TestTribulationPhasesConcurrentEntryAndRollback(t *testing.T) {
	setupTribulationTestDB(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	r := newTribulationTestRound(t, -100, now)
	if _, err := CreateTribulationRound(-100, "same", now); !errors.Is(err, ErrTribulationBusy) {
		t.Fatalf("busy: %v", err)
	}
	user := &tgbotapi.User{ID: 1, FirstName: "玩家1"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ChooseTribulationSide(r.ID, r.ChatID, r.MessageID, TribulationYang, false, user, now)
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(err, ErrTribulationJoined) {
				t.Errorf("duplicate: %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 || tribulationTestBalance(t, 1) != 95 || tribulationTestCount(t, &model.TribulationEntry{}) != 1 || tribulationTestCount(t, &model.PointTransaction{}) != 1 {
		t.Fatal("duplicate join charged twice")
	}
	checks := []struct {
		user      int64
		chat      int64
		message   int
		side      string
		switching bool
		at        time.Time
		want      error
	}{
		{2, -200, r.MessageID, TribulationYin, false, now, ErrTribulationPanel},
		{2, -100, r.MessageID + 1, TribulationYin, false, now, ErrTribulationPanel},
		{2, -100, r.MessageID, "invalid", false, now, ErrTribulationPanel},
		{1, -100, r.MessageID, TribulationYin, true, r.JoinUntil.Add(-time.Nanosecond), ErrTribulationSwitchDisabled},
		{2, -100, r.MessageID, TribulationYin, false, r.JoinUntil, ErrTribulationJoinClosed},
		{2, -100, r.MessageID, TribulationYin, true, r.JoinUntil, ErrTribulationSwitchDisabled},
		{1, -100, r.MessageID, TribulationYang, true, r.JoinUntil, ErrTribulationSwitchDisabled},
		{1, -100, r.MessageID, TribulationYin, true, r.SwitchUntil, ErrTribulationSwitchDisabled},
	}
	for _, c := range checks {
		_, err := ChooseTribulationSide(r.ID, c.chat, c.message, c.side, c.switching, &tgbotapi.User{ID: c.user}, c.at)
		if !errors.Is(err, c.want) {
			t.Fatalf("want %v got %v", c.want, err)
		}
	}
	if tribulationTestBalance(t, 1) != 95 || tribulationTestCount(t, &model.PointTransaction{}) != 1 {
		t.Fatal("switch charged points")
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 2).Update("points", 4).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ChooseTribulationSide(r.ID, r.ChatID, r.MessageID, TribulationYang, false, &tgbotapi.User{ID: 2}, now); !errors.Is(err, storage.ErrPointsNotEnough) {
		t.Fatal(err)
	}
	if tribulationTestCount(t, &model.TribulationEntry{}) != 1 || tribulationTestBalance(t, 2) != 4 {
		t.Fatal("insufficient funds created entry")
	}
	if err := storage.DB.Exec(`CREATE TRIGGER fail_tribulation_ledger BEFORE INSERT ON point_transactions BEGIN SELECT RAISE(ABORT,'ledger failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ChooseTribulationSide(r.ID, r.ChatID, r.MessageID, TribulationYang, false, &tgbotapi.User{ID: 3}, now); err == nil {
		t.Fatal("ledger failure ignored")
	}
	if tribulationTestBalance(t, 3) != 100 || tribulationTestCount(t, &model.TribulationEntry{}) != 1 {
		t.Fatal("join did not roll back")
	}
}

func TestTribulationThirtySecondEntryAndImmediateSettlement(t *testing.T) {
	setupTribulationTestDB(t)
	now := time.Now()
	r := newTribulationTestRound(t, -100, now)
	deadline := now.Add(30 * time.Second)
	if !r.JoinUntil.Equal(deadline) || !r.SwitchUntil.Equal(deadline) || !r.ExpiresAt.Equal(deadline) {
		t.Fatalf("unexpected round deadlines: %+v", r)
	}
	tribulationTestChoose(t, r, 1, TribulationYang, false, deadline.Add(-time.Nanosecond))
	if closed, err := CloseTribulationRound(r.ID, deadline.Add(-time.Nanosecond)); err != nil || closed {
		t.Fatal("settled before entry deadline", err)
	}
	if _, err := ChooseTribulationSide(r.ID, r.ChatID, r.MessageID, TribulationYin, false, &tgbotapi.User{ID: 2}, deadline); !errors.Is(err, ErrTribulationJoinClosed) {
		t.Fatal("accepted entry at deadline", err)
	}
	if closed, err := CloseTribulationRound(r.ID, deadline); err != nil || !closed {
		t.Fatal("did not settle at entry deadline", err)
	}
	if tribulationTestBalance(t, 2) != 100 {
		t.Fatal("late entry charged points")
	}
}

func TestTribulationSettlementAwardsAndNoSoloFarming(t *testing.T) {
	setupTribulationTestDB(t)
	now := time.Now()
	r := newTribulationTestRound(t, -100, now)
	for id, side := range map[int64]string{1: TribulationYang, 2: TribulationYin, 3: TribulationYin, 4: TribulationYang} {
		tribulationTestChoose(t, r, id, side, false, now)
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt.Add(-time.Nanosecond)); err != nil || closed {
		t.Fatal("settled early", err)
	}
	settleTribulationTestRound(t, r, TribulationYin)
	for id, want := range map[int64]int{1: 95, 2: 105, 3: 105, 4: 95} {
		if got := tribulationTestBalance(t, id); got != want {
			t.Fatalf("user %d: %d", id, got)
		}
	}
	if tribulationTestCount(t, &model.TribulationTitleHolder{}) != 0 || tribulationTestCount(t, &model.TribulationTitleStat{}) != 0 {
		t.Fatal("retired titles were awarded")
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt.Add(time.Hour)); err != nil || closed {
		t.Fatal("duplicate settlement", err)
	}
	if tribulationTestCount(t, &model.PointTransaction{}) != 6 {
		t.Fatal("duplicate reward ledger")
	}
	for _, sides := range [][]string{{TribulationYang}, {TribulationYang, TribulationYang}, {}} {
		round := newTribulationTestRound(t, -200, now)
		for i, side := range sides {
			tribulationTestChoose(t, round, int64(i+5), side, false, now)
		}
		if len(sides) > 0 {
			settleTribulationTestRound(t, round, TribulationYang)
		} else {
			if _, err := CloseTribulationRound(round.ID, round.ExpiresAt); err != nil {
				t.Fatal(err)
			}
		}
		var saved model.TribulationRound
		if err := storage.DB.First(&saved, round.ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.TitleEligible {
			t.Fatal("single-sided round counted titles")
		}
		if len(sides) == 0 && saved.Status != TribulationCancelled {
			t.Fatal("empty round not cancelled")
		}
	}
	if tribulationTestCount(t, &model.TribulationTitleHolder{}) != 0 {
		t.Fatal("solo farming created title")
	}
}

func TestTribulationSavedDrawSurvivesFailedPayoutAndRestart(t *testing.T) {
	path := setupTribulationTestDB(t)
	r := newTribulationTestRound(t, -100, time.Now())
	tribulationTestChoose(t, r, 1, TribulationYang, false, r.CreatedAt)
	tribulationTestChoose(t, r, 2, TribulationYin, false, r.CreatedAt)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_tribulation_close BEFORE UPDATE ON tribulation_rounds WHEN NEW.status = 'closed' BEGIN SELECT RAISE(ABORT,'close failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt); err == nil || closed {
		t.Fatal("close failure ignored")
	}
	var saved model.TribulationRound
	if err := storage.DB.First(&saved, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != TribulationSettling || !validTribulationSide(saved.WinningSide) {
		t.Fatal("draw was not saved")
	}
	if tribulationTestBalance(t, 1) != 95 || tribulationTestBalance(t, 2) != 95 || tribulationTestCount(t, &model.TribulationTitleStat{}) != 0 || tribulationTestCount(t, &model.PointTransaction{}) != 2 {
		t.Fatal("failed settlement partly committed")
	}
	if _, err := CreateTribulationRound(-100, "blocked", r.ExpiresAt); !errors.Is(err, ErrTribulationBusy) {
		t.Fatal("settling round released group")
	}
	// 重新打开同一 SQLite 文件，不依赖进程内计时器恢复。
	storage.DB = openTribulationTestDB(t, path)
	if err := storage.DB.Exec("DROP TRIGGER fail_tribulation_close").Error; err != nil {
		t.Fatal(err)
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt.Add(time.Hour)); err != nil || !closed {
		t.Fatal(closed, err)
	}
	var after model.TribulationRound
	if err := storage.DB.First(&after, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.WinningSide != saved.WinningSide || after.Status != TribulationClosed {
		t.Fatal("restart rerolled result")
	}
	winner := int64(1)
	if saved.WinningSide == TribulationYin {
		winner = 2
	}
	if tribulationTestBalance(t, winner) != 105 || tribulationTestCount(t, &model.PointTransaction{}) != 3 {
		t.Fatal("wrong recovered payout")
	}
	if closed, err := CloseTribulationRound(r.ID, r.ExpiresAt.Add(2*time.Hour)); err != nil || closed {
		t.Fatal("repeated payout", err)
	}
}

func TestTribulationMissingWinnerWalletDoesNotBlockRound(t *testing.T) {
	setupTribulationTestDB(t)
	r := newTribulationTestRound(t, -100, time.Now())
	tribulationTestChoose(t, r, 1, TribulationYang, false, r.CreatedAt)
	tribulationTestChoose(t, r, 2, TribulationYin, false, r.CreatedAt)
	if err := storage.DB.Unscoped().Where("telegram_id = ?", 1).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
	settleTribulationTestRound(t, r, TribulationYang)
	if tribulationTestBalance(t, 1) != 10 || tribulationTestBalance(t, 2) != 95 {
		t.Fatal("missing wallet blocked settlement or wrong recovery payout")
	}
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", 1).First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.AbsUserID != "" {
		t.Fatal("game recreated an ABS binding")
	}
}

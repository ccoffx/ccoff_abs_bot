package game

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSectLotterySummaryDistinguishesUnavailableAndEmptyWinners(t *testing.T) {
	lot := model.SectLottery{Title: "测试抽奖", EntryCount: 5, PrizeCount: 1}
	lot.ID = 42
	failed := sectLotteryCreatorSummaryText(lot, nil, 1, 0, 0, errors.New("database unavailable"))
	if strings.Contains(failed, "中奖名单：无") || !strings.Contains(failed, "中奖名单读取失败") || !strings.Contains(failed, "查看宗门抽奖 42") || !strings.Contains(failed, "成功发送：1") {
		t.Fatalf("query failure misrepresented lottery results: %s", failed)
	}
	empty := sectLotteryCreatorSummaryText(lot, nil, 0, 0, 1, nil)
	if !strings.Contains(empty, "中奖名单：无") {
		t.Fatalf("empty successful query lost its empty state: %s", empty)
	}
	populated := sectLotteryCreatorSummaryText(lot, []model.SectLotteryWinner{{UserID: 7, UserName: "中奖道友", Status: sectLotteryWinnerDelivered}}, 1, 0, 0, nil)
	if !strings.Contains(populated, "中奖道友(7)") || strings.Contains(populated, "中奖名单：无") {
		t.Fatalf("winner missing from summary: %s", populated)
	}
}

func TestSectLotteryActionErrorTextAlreadyJoined(t *testing.T) {
	got := sectLotteryActionErrorText(storage.ErrLotteryAlreadyJoined)
	want := "✅ 你当前已经参与过该宗门抽奖，请发送「宗门抽奖」查看活动列表。"
	if got != want {
		t.Fatalf("unexpected error text: got %q, want %q", got, want)
	}
}

func TestSectLotteryJoinAlreadyJoinedRepliesRoleAware(t *testing.T) {
	setupGuardTestDB(t)
	if err := storage.DB.AutoMigrate(&model.SectLottery{}, &model.SectLotteryEntry{}, &model.SectLotteryPrize{}, &model.SectShopContributionTotal{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_lottery_entries_lottery_user_unique ON sect_lottery_entries(lottery_id, user_id) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	sect := model.Sect{Name: "测试宗门", Level: 1}
	if err := storage.DB.Create(&sect).Error; err != nil {
		t.Fatal(err)
	}
	sectID := int64(sect.ID)
	lot := model.SectLottery{
		SectID: sectID,
		Title:  "测试抽奖",
		Status: sectLotteryStatusActive,
		Mode:   sectLotteryModeCount,
	}
	if err := storage.DB.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	ownerMember := model.SectMember{SectID: sectID, UserID: 1001, Role: "owner", Contribution: 500}
	regMember := model.SectMember{SectID: sectID, UserID: 1002, Role: "member", Contribution: 500}
	if err := storage.DB.Create(&ownerMember).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&regMember).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 1001, Username: "user1001", AbsUserID: "abs1001", AccountType: storage.AccountTypeFormal, Status: "active", IsWhitelist: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: 1002, Username: "user1002", AbsUserID: "abs1002", AccountType: storage.AccountTypeFormal, Status: "active", IsWhitelist: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SectLotteryEntry{LotteryID: lot.ID, SectID: sectID, UserID: 1001, JoinedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.SectLotteryEntry{LotteryID: lot.ID, SectID: sectID, UserID: 1002, JoinedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	bot, replies := secretRealmQueryBot(t)
	// Test regular member
	handleJoinSectLotteryCommand(bot, &tgbotapi.Message{
		From: &tgbotapi.User{ID: 1002, FirstName: "普通弟子"},
		Chat: &tgbotapi.Chat{ID: 1002, Type: "private"},
	}, fmt.Sprintf("%d", lot.ID))
	gotReg := replies()
	if !strings.Contains(gotReg, "请发送「宗门抽奖」查看活动列表") {
		t.Fatalf("regular member repeated join reply unexpected: %s", gotReg)
	}

	// Test owner
	handleJoinSectLotteryCommand(bot, &tgbotapi.Message{
		From: &tgbotapi.User{ID: 1001, FirstName: "宗主"},
		Chat: &tgbotapi.Chat{ID: 1001, Type: "private"},
	}, fmt.Sprintf("%d", lot.ID))
	gotOwner := replies()
	if !strings.Contains(gotOwner, fmt.Sprintf("请发送「查看宗门抽奖 %d」查看详情", lot.ID)) {
		t.Fatalf("owner repeated join reply unexpected: %s", gotOwner)
	}
}

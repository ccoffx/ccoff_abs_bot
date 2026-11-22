package game

import (
	"errors"
	"strings"
	"testing"

	"ccoff-abs-bot/internal/model"
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

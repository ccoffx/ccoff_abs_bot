package game

import (
	"fmt"
	"log"
	"sync"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"ccoff-abs-bot/internal/titles"
)

// A batch owns the write lock through title publication. Other ABS refresh
// paths take a read lock, so individual refreshes can still run concurrently
// with each other, but cannot replace batch data before it is published.
var listeningRefreshMu sync.RWMutex

// The legacy force argument is retained for callers; a full ranking refresh now
// always fetches every account, so fresh individual caches cannot skew the batch.
func RefreshAllDailyListeningStatsWithOptions(_ bool) (int, int, int, error) {
	// 手动全服刷新和后台刷新共用锁，避免并行批次交叉发布。
	listeningRefreshMu.Lock()
	defer listeningRefreshMu.Unlock()
	batchStartedAt := time.Now()
	var users []model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("abs_user_id <> ''").Find(&users).Error; err != nil {
		log.Printf("⚠️ 查询每日净修为刷新用户失败: err=%s", textfmt.FormatPlainError(err))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return 0, 0, 0, err
	}

	success := 0
	skipped := 0
	failed := 0
	for _, u := range users {
		// 发布榜首的批次必须覆盖全员；不能把新拉取的数据与个人页面
		// 留下的缓存混在一起比较，即使该缓存尚未超过新鲜度 TTL。
		if _, ok := refreshDailyListeningStatsFromABSUnlocked(u.TelegramID, u.AbsUserID, true); ok {
			if _, syncOK := syncCultivationFromDailyListeningStats(u.TelegramID); !syncOK {
				failed++
				continue
			}
			success++
		} else {
			failed++
		}
		time.Sleep(storage.DailyListeningRefreshRequestDelay)
	}

	if failed > 0 {
		err := fmt.Errorf("每日净修为刷新失败: %d/%d 个用户", failed, len(users))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return success, len(users), skipped, err
	}
	if err := titles.RefreshRankTitlesAfterListeningBatch(batchStartedAt, time.Now()); err != nil {
		publishErr := fmt.Errorf("完整听书刷新后的称号发布失败: %w", err)
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, publishErr)
		return success, len(users), skipped, publishErr
	}
	return success, len(users), skipped, nil
}

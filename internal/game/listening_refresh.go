package game

import (
	"fmt"
	"log"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func RefreshAllDailyListeningStatsWithOptions(force bool) (int, int, int, error) {
	var users []model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("abs_user_id <> ''").Find(&users).Error; err != nil {
		log.Printf("⚠️ 查询每日净修为刷新用户失败: err=%s", textfmt.FormatPlainError(err))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return 0, 0, 0, err
	}

	success := 0
	skipped := 0
	failed := 0
	now := time.Now()
	for _, u := range users {
		if !force {
			if stat, ok := getTodayDailyListeningStat(u.TelegramID, now); ok && shouldSkipDailyListeningRefresh(stat, now, force) {
				skipped++
				continue
			}
		}
		if _, ok := RefreshDailyListeningStatsFromABS(u.TelegramID, u.AbsUserID); ok {
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
	return success, len(users), skipped, nil
}

func shouldSkipDailyListeningRefresh(stat model.DailyListeningStat, now time.Time, force bool) bool {
	if force || stat.LastFetchedAt.IsZero() {
		return false
	}
	age := now.Sub(stat.LastFetchedAt)
	return age >= 0 && age < storage.DailyListeningRefreshFreshTTL
}

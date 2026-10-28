package game

import (
	"log"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func RefreshAllDailyListeningStatsWithOptions(force bool) (int, int, int) {
	var users []model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("abs_user_id <> ''").Find(&users).Error; err != nil {
		log.Printf("⚠️ 查询每日净修为刷新用户失败: err=%s", textfmt.FormatPlainError(err))
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
		return 0, 0, 0
	}

	success := 0
	skipped := 0
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
				storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, storage.ErrDailyListeningStatsReadFailed)
				continue
			}
			success++
		}
		time.Sleep(storage.DailyListeningRefreshRequestDelay)
	}

	return success, len(users), skipped
}

func shouldSkipDailyListeningRefresh(stat model.DailyListeningStat, now time.Time, force bool) bool {
	if force || stat.LastFetchedAt.IsZero() {
		return false
	}
	age := now.Sub(stat.LastFetchedAt)
	return age >= 0 && age < storage.DailyListeningRefreshFreshTTL
}

package game

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

// Render fresh snapshots locally; synchronize only expired data before reading.
// Keep the client argument for callers compiled against the existing API.
func GetPersonalReport(_ *abs.AbsClient, absUserID string) string {
	if storage.DB == nil {
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}
	var user model.User
	if err := storage.DB.Where("abs_user_id = ?", absUserID).First(&user).Error; err != nil {
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}
	freshness := EnsureListeningFresh(user.TelegramID)
	var snapshot model.ListeningReportSnapshot
	if err := storage.DB.Where("user_id = ? AND abs_user_id = ?", user.TelegramID, absUserID).First(&snapshot).Error; (err == nil || errors.Is(err, gorm.ErrRecordNotFound)) && !listeningTimestampFresh(snapshot.ProgressFetchedAt, time.Now(), ListeningMaxAge) {
		refreshListeningReportProgress(user.TelegramID, absUserID, time.Now())
	}
	report := personalReportFromCache(absUserID)
	if freshness.Notice != "" {
		report += "\n\n" + freshness.Notice
	}
	return report
}

// Pin the rendering reads to one database version after any network work.
func personalReportFromCache(absUserID string) string {
	if storage.DB == nil {
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}
	now := time.Now()
	var user model.User
	var snapshot model.ListeningReportSnapshot
	var stat model.DailyListeningStat
	var cul model.Cultivation
	var raw float64
	var ok, totalOK bool
	// A short read transaction pins one SQLite snapshot without the batch lock.
	// A concurrent settlement or rebind cannot mix different database versions.
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("abs_user_id = ?", absUserID).First(&user).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND abs_user_id = ?", user.TelegramID, absUserID).First(&snapshot).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		err := tx.Where("user_id = ? AND abs_user_id = ? AND day_key = ?", user.TelegramID, absUserID, SectDayKey(now)).First(&stat).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		ok = err == nil
		raw = snapshot.RawTotalSeconds
		if snapshot.StatsFetchedAt.IsZero() {
			if err := tx.Model(&model.DailyListeningStat{}).Where("user_id = ? AND abs_user_id = ?", user.TelegramID, absUserID).Select("COALESCE(SUM(raw_seconds),0)").Scan(&raw).Error; err != nil {
				return err
			}
		}
		err = tx.Where("user_id = ?", user.TelegramID).First(&cul).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		totalOK = err == nil
		return nil
	})
	if err != nil {
		return "❌ 听书统计暂时读取失败，请稍后再试。"
	}
	totalText := "读取失败"
	if totalOK {
		totalText = fmt.Sprintf("%.2f 小时", cul.TotalAudioTime)
	}
	todayRaw, todayTotal := "待同步", "待同步"
	detail := ""
	notice := "\n📭 暂无今日每日统计缓存，请发送「刷新我的今日净修为」同步。"
	if ok {
		todayRaw = fmt.Sprintf("%.2f 小时", abs.PositiveListeningSeconds(stat.RawSeconds)/3600)
		if DailyListeningCultivationSettled(stat) {
			todayTotal = fmt.Sprintf("%.2f 小时", stat.AppliedTotalHours)
			detail = fmt.Sprintf("\n🔥 今日基础净修为: %.2f 小时；异火额外修为: +%.2f 小时", stat.BaseCultivationHours, stat.SpiritFireHours) + SectOwnerDailyBonusLine(stat) + fmt.Sprintf("\n今日有效计入听书: %.2f 小时", stat.CappedSeconds/3600) + dailyListeningValidationNotice(stat)
			notice = fmt.Sprintf("\n🗄 使用上次成功统计（%s）；发送「刷新我的今日净修为」可同步最新听书数据。", stat.LastFetchedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"))
		} else {
			todayTotal = "待结算"
			notice = "\n⏳ 听书数据已同步，净修为待结算；累计修为为上次结算值。可发送「刷新我的今日净修为」重试。"
		}
	}
	finished, listening := "待同步", "待同步"
	if !snapshot.ProgressFetchedAt.IsZero() {
		finished = strconv.Itoa(snapshot.FinishedBooks)
		listening = strconv.Itoa(snapshot.ListeningBooks)
	}
	progressNotice := ""
	if !snapshot.ProgressFetchedAt.IsZero() {
		progressNotice = "\n📚 书籍进度更新时间：" + snapshot.ProgressFetchedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05")
		if !listeningTimestampFresh(snapshot.ProgressFetchedAt, now, ListeningMaxAge) {
			progressNotice += "（已超过5分钟，更新失败，展示旧数据）"
		}
	}
	return fmt.Sprintf("📊 你的专属听书战绩%s\n\n🎧 累计实际听书: %.2f 小时\n🕒 今日实际听书: %s\n✨ 净修炼时长: %s\n🌅 今日净修为: %s%s\n📚 已经听完书籍: %s 本\n📖 当前正在收听: %s 本%s", notice, raw/3600, todayRaw, totalText, todayTotal, detail, finished, listening, progressNotice)
}

// Source rows can commit before cultivation settlement. Readers must not
// present their previous (or uninitialized) component cache as the new amount.
func DailyListeningCultivationSettled(stat model.DailyListeningStat) bool {
	return stat.CultivationApplied && stat.AppliedEligible && !stat.DeletedAt.Valid &&
		stat.AppliedEffectiveHours == stat.EffectiveHours && stat.AppliedCappedSeconds == stat.CappedSeconds &&
		stat.AppliedBaseHours == stat.BaseCultivationHours &&
		stat.AppliedTotalHours == stat.BaseCultivationHours+stat.SpiritFireHours+stat.SectOwnerBonusHours
}

func cachedCultivationTotal(userID int64) (float64, bool) {
	var cul model.Cultivation
	if storage.DB == nil || storage.DB.Where("user_id = ?", userID).First(&cul).Error != nil {
		return 0, false
	}
	return cul.TotalAudioTime, true
}

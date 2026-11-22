package game

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/background"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func HandleDailyListeningStatCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string, role string) bool {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return false
	}

	text = strings.TrimSpace(text)
	if !isDailyListeningStatCommandText(text) {
		return false
	}

	if !msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "每日净修为刷新和查询请私聊 Bot 执行。")
		return true
	}

	switch {
	case text == "刷新我的今日净修为":
		handleRefreshMyDailyListeningStat(bot, msg)
		return true
	case text == "刷新宗门今日净修为":
		handleRefreshSectDailyListeningStatsCommand(bot, msg)
		return true
	case text == "刷新全服今日净修为":
		if role != "super_admin" {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 权限不足：该操作仅限超级管理员。")
			return true
		}
		handleRefreshAllDailyListeningStats(bot, msg)
		return true
	case strings.HasPrefix(text, "查看每日净修为"):
		if role != "super_admin" && role != "admin" {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 权限不足。")
			return true
		}
		handleQueryDailyListeningStat(bot, msg, text)
		return true
	default:
		return false
	}
}

func isDailyListeningStatCommandText(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	if len(fields) == 1 {
		return fields[0] == "刷新我的今日净修为" ||
			fields[0] == "刷新宗门今日净修为" ||
			fields[0] == "刷新全服今日净修为" ||
			fields[0] == "查看每日净修为"
	}
	return fields[0] == "查看每日净修为"
}

func refreshUserDailyListeningAndCultivation(userID int64) (model.DailyListeningStat, float64, bool, error) {
	var u model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		return model.DailyListeningStat{}, 0, false, err
	}
	if strings.TrimSpace(u.AbsUserID) == "" {
		return model.DailyListeningStat{}, 0, false, storage.ErrAbsUserIDEmpty
	}

	_, refreshOK := refreshDailyListeningStatsFromABS(userID, u.AbsUserID, true)
	now := time.Now()
	if !refreshOK {
		if stat, cacheOK := getTodayDailyListeningStat(userID, now); cacheOK {
			totalEffective, totalOK := cachedCultivationTotal(userID)
			if !totalOK {
				return model.DailyListeningStat{}, 0, false, storage.ErrDailyListeningStatsReadFailed
			}
			return stat, totalEffective, true, storage.ErrAbsRefreshFailedUsingCache
		}
		return model.DailyListeningStat{}, 0, false, storage.ErrAbsRefreshFailed
	}

	totalEffective, syncOK := syncCultivationFromDailyListeningStatsAt(userID, now)
	if !syncOK {
		return model.DailyListeningStat{}, 0, false, storage.ErrDailyListeningStatsReadFailed
	}
	stat, ok := getTodayDailyListeningStat(userID, now)
	return stat, totalEffective, ok, nil
}

func syncCultivationFromDailyListeningStats(userID int64) (float64, bool) {
	return syncCultivationFromDailyListeningStatsAt(userID, time.Now())
}

func syncCultivationFromDailyListeningStatsAt(userID int64, now time.Time) (float64, bool) {
	if storage.DB == nil || userID == 0 {
		return 0, false
	}
	total := 0.0
	var cul model.Cultivation
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		cul, err = settleCultivationListeningInTx(tx, userID, now)
		total = cul.TotalAudioTime
		return err
	})
	if err != nil {
		log.Printf("修为与宗门听书贡献结算失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0, false
	}
	return total, true
}

func lockCultivationListeningTx(tx *gorm.DB, userID int64) error {
	res := tx.Model(&model.Cultivation{}).Where("user_id = ?", userID).UpdateColumn("total_audio_time", gorm.Expr("total_audio_time"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return storage.ErrCultivationNotFound
	}
	return nil
}

// Personal cultivation and contribution-eligible totals are derived from the
// same read snapshot. EffectiveHours never includes retreat or fire effects.
func dailyCultivationTotalsInTx(tx *gorm.DB, userID int64, now time.Time) (total, baseTotal float64, err error) {
	var stats []model.DailyListeningStat
	if err = tx.Where("user_id = ?", userID).Find(&stats).Error; err != nil {
		return
	}
	var retreats []model.SectCaveRetreat
	if err = tx.Where("user_id = ? AND start_at <= ?", userID, now).Find(&retreats).Error; err != nil {
		return
	}
	for _, retreat := range retreats {
		if _, err = decodeSectRetreatEvidence(retreat); err != nil {
			return
		}
	}
	validStats := stats[:0]
	bases := make(map[string]float64, len(stats))
	today := SectDayKey(now)
	for _, stat := range stats {
		if !validDailyListeningDayKey(stat.DayKey) || stat.DayKey > today {
			continue
		}
		base := stat.EffectiveHours + dailySectCaveRetreatBonusHours(stat, retreats)
		bases[stat.DayKey] = base
		validStats = append(validStats, stat)
	}
	if err = spiritfire.RefreshDailyBatch(tx, validStats, bases, now); err != nil {
		return
	}
	if err = sectbonus.RefreshDailyBatch(tx, validStats); err != nil {
		return
	}
	for _, stat := range validStats {
		baseTotal += stat.BaseCultivationHours
		total += stat.BaseCultivationHours + stat.SpiritFireHours + stat.SectOwnerBonusHours
	}
	return
}

func totalCultivationEffectiveHoursFromDailyStats(userID int64, now time.Time) (float64, bool) {
	if storage.DB == nil || userID == 0 {
		return 0, false
	}
	total := 0.0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		var err error
		total, _, err = dailyCultivationTotalsInTx(tx, userID, now)
		return err
	})
	if err != nil {
		log.Printf("累计修为读取失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0, false
	}
	return total, true
}

// RefreshSpiritFireDailyStat keeps the original EffectiveHours field unchanged
// for sect calculations and exposes separate personal cultivation components.
func RefreshSpiritFireDailyStat(stat *model.DailyListeningStat, now time.Time) error {
	if storage.DB == nil {
		return fmt.Errorf("DB_NOT_READY")
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		if stat.ID != 0 {
			if err := tx.First(stat, stat.ID).Error; err != nil {
				return err
			}
		}
		var retreats []model.SectCaveRetreat
		if err := tx.Where("user_id = ? AND start_at <= ?", stat.UserID, now).Find(&retreats).Error; err != nil {
			return err
		}
		if err := spiritfire.RefreshDaily(tx, stat, stat.EffectiveHours+dailySectCaveRetreatBonusHours(*stat, retreats), now); err != nil {
			return err
		}
		return sectbonus.RefreshDaily(tx, stat)
	})
}

func handleRefreshMyDailyListeningStat(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	if CachedListeningFreshness(msg.From.ID).Fresh {
		sendMyDailyListeningStatFromCache(bot, msg)
		return
	}
	result := background.Submit(fmt.Sprintf("personal_listening:%d", msg.From.ID), func() {
		_, _, _, err := refreshUserDailyListeningAndCultivation(msg.From.ID)
		if err != nil {
			telegram.ReplyText(bot, msg.Chat.ID, "⚠️ 本次听书同步未完成，保留已保存数据，请稍后重试。")
		}
		sendMyDailyListeningStatFromCache(bot, msg)
	})
	replyBackgroundSubmission(bot, msg.Chat.ID, result, "本人净修为刷新")
}

func sendMyDailyListeningStatFromCache(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	// Share the cache policy with listening reports and other personal queries.
	freshness := CachedListeningFreshness(msg.From.ID)
	if storage.DB == nil {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 今日净修为读取失败，请稍后再试。")
		return
	}
	var stat model.DailyListeningStat
	var cultivation model.Cultivation
	statFound := false
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", msg.From.ID).First(&user).Error; err != nil {
			return err
		}
		if strings.TrimSpace(user.AbsUserID) == "" {
			return gorm.ErrRecordNotFound
		}
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND day_key = ?", msg.From.ID, user.AbsUserID, SectDayKey(time.Now())).First(&stat).Error; err != nil {
			return err
		}
		statFound = true
		return tx.Where("user_id = ?", msg.From.ID).First(&cultivation).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) && !statFound {
		telegram.ReplyText(bot, msg.Chat.ID, "📭 今日暂无听书统计，请等待后台自动更新（约每 5 分钟一次）。\n"+freshness.Notice)
		return
	}
	if err != nil {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 今日净修为读取失败，请稍后再试。")
		return
	}
	reply := formatDailyListeningStat("📊 今日净修为（后台约每 5 分钟自动更新）", stat, cultivation.TotalAudioTime)
	if freshness.Notice != "" {
		reply += "\n\n" + freshness.Notice
	}
	telegram.ReplyText(bot, msg.Chat.ID, reply)
}

func handleRefreshSectDailyListeningStatsCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	var member model.SectMember
	if err := storage.DB.Where("user_id = ?", msg.From.ID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 您当前没有加入宗门。")
		} else {
			log.Printf("⚠️ 刷新宗门每日净修为读取成员档案失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 宗门成员档案读取失败，请稍后再试。")
		}
		return
	}
	if !canViewSectListeningStats(member) {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 仅宗主或长老可以刷新宗门净修为。")
		return
	}

	// 每名成员最多触发 1 次统计 + 最多 5 页会话扫描，一次刷新最多数百个 ABS 请求，加冷却防滥用。
	if !storage.CheckRateLimit(msg.From.ID, "sect_daily_listening_refresh", 60*time.Second) {
		telegram.ReplyText(bot, msg.Chat.ID, "⏳ 宗门净修为刷新太频繁，请每分钟最多刷新一次。")
		return
	}

	result := background.Submit(fmt.Sprintf("sect_listening:%d", member.SectID), func() {
		var current model.SectMember
		if err := storage.DB.Where("user_id = ? AND sect_id = ?", msg.From.ID, member.SectID).First(&current).Error; err != nil || !canViewSectListeningStats(current) {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 宗门刷新权限已变化，任务已取消。")
			return
		}
		count := refreshSectMembersDailyListeningStatsWithOptions(member.SectID, 100, true)
		telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("✅ 宗门净修为后台刷新完成：成功 `%d` 名成员。", count))
	})
	replyBackgroundSubmission(bot, msg.Chat.ID, result, "宗门净修为刷新")
}

func handleRefreshAllDailyListeningStats(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	result := background.Submit("listening_full_refresh", func() {
		if storage.GetUserRole(msg.From.ID) != "super_admin" {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 刷新权限已变化，任务已取消。")
			return
		}
		success, total, skipped, refreshErr := RefreshAllDailyListeningStatsWithOptions(true)
		// 全服级批量重算属于高危管理操作，补齐与其他管理变更一致的审计留痕。
		storage.WriteAuditLog(msg.From.ID, "REFRESH_ALL_DAILY_LISTENING", "daily_listening_stats",
			fmt.Sprintf("全服今日净修为刷新: 成功 %d/%d，跳过 %d", success, total, skipped))
		if refreshErr != nil {
			telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("⚠️ 全服今日净修为刷新未全部完成：成功 `%d/%d`，跳过 `%d`。请检查后台错误日志。", success, total, skipped))
			return
		}
		telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("✅ 全服今日净修为刷新完成：成功 `%d/%d`，跳过 `%d`。", success, total, skipped))
	})
	replyBackgroundSubmission(bot, msg.Chat.ID, result, "全服净修为刷新")
}

func handleQueryDailyListeningStat(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		telegram.ReplyText(bot, msg.Chat.ID, "用法：`查看每日净修为 用户ID [YYYY-MM-DD]`")
		return
	}

	targetID, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || targetID == 0 {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 用户ID格式错误。")
		return
	}

	todayKey := SectDayKey(time.Now())
	dayKey := todayKey
	if len(fields) >= 3 {
		dayKey = strings.TrimSpace(fields[2])
	}

	var freshness ListeningFreshness
	if dayKey == todayKey {
		freshness = CachedListeningFreshness(targetID)
	}
	var stat model.DailyListeningStat
	var totalEffective float64
	statFound := false
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("user_id = ? AND day_key = ?", targetID, dayKey)
		if dayKey == todayKey {
			query = query.Where(currentListeningBindingSQL, targetID)
		}
		if err := query.First(&stat).Error; err != nil {
			return err
		}
		statFound = true
		var cul model.Cultivation
		if err := tx.Where("user_id = ?", targetID).First(&cul).Error; err != nil {
			return err
		}
		totalEffective = cul.TotalAudioTime
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if statFound {
				telegram.ReplyText(bot, msg.Chat.ID, "❌ 每日净修为累计读取失败，请稍后重试。")
			} else {
				telegram.ReplyText(bot, msg.Chat.ID, "📭 未找到该用户该日期的每日净修为记录。")
			}
		} else {
			log.Printf("⚠️ 查询每日净修为记录失败: user=%d day=%s err=%s", targetID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 每日净修为记录读取失败，请稍后再试。")
		}
		return
	}

	reply := formatDailyListeningStat(fmt.Sprintf("📊 用户 `%d` 每日净修为", targetID), stat, totalEffective)
	if freshness.Notice != "" {
		reply += "\n\n" + freshness.Notice
	}
	telegram.ReplyText(bot, msg.Chat.ID, reply)
}

func formatDailyListeningStat(title string, stat model.DailyListeningStat, totalEffective float64) string {
	fetchStatus := stat.FetchStatus
	if fetchStatus == "" {
		fetchStatus = "unknown"
	}
	refreshReason := stat.RefreshReason
	if refreshReason == "" {
		refreshReason = stat.Source
	}

	retreatBonus := stat.BaseCultivationHours - stat.EffectiveHours
	retreatLine := fmt.Sprintf("\n基础净修为：`%.2f` 小时\n异火额外修为：`+%.2f` 小时", stat.EffectiveHours+retreatBonus, stat.SpiritFireHours)
	retreatLine += SectOwnerDailyBonusLine(stat)
	if stat.ValidationJSON != "" {
		retreatLine += fmt.Sprintf("\nABS 原始归属统计：`%.2f` 小时", stat.OfficialRawSeconds/3600)
		retreatLine += dailyListeningValidationNotice(stat)
	}
	if retreatBonus > 0 {
		retreatLine += fmt.Sprintf("\n洞府闭关加成：`+%.2f` 小时", retreatBonus)
	}
	dailyText := fmt.Sprintf("`%.2f` 小时", stat.BaseCultivationHours+stat.SpiritFireHours+stat.SectOwnerBonusHours)
	if !DailyListeningCultivationSettled(stat) {
		dailyText = "待结算"
		retreatLine = "\n⏳ 听书数据已同步，净修为待结算；总净修为为上次结算值。"
		if stat.ValidationJSON != "" {
			retreatLine += fmt.Sprintf("\nABS 原始归属统计：`%.2f` 小时", stat.OfficialRawSeconds/3600) + dailyListeningValidationNotice(stat)
		}
	}

	return fmt.Sprintf(
		"%s\n\n日期：`%s`\n分日听书统计：`%.2f` 小时\n有效计入听书：`%.2f` 小时\n今日净修为：%s%s\n总净修为：`%.2f` 小时\n更新时间：`%s`\n来源：`%s`\n刷新状态：`%s`\n刷新原因：`%s`",
		title,
		stat.DayKey,
		stat.RawSeconds/3600.0,
		stat.CappedSeconds/3600.0,
		dailyText,
		retreatLine,
		totalEffective,
		stat.LastFetchedAt.Format("2006-01-02 15:04:05"),
		stat.Source,
		fetchStatus,
		refreshReason,
	)
}

func settleCultivationListeningInTx(tx *gorm.DB, userID int64, now time.Time) (model.Cultivation, error) {
	var cul model.Cultivation
	var total float64
	// Acquire a SQLite write lock before reading either the totals or the cursor.
	// The cursor, contribution payment and profile update commit together.
	if err := spiritfire.Lock(tx); err != nil {
		return cul, err
	}
	// Account cleanup/rebinding and profile initialization must serialize.
	// A late queued refresh must not recreate a deleted account's profile.
	if err := lockCultivationAccountInTx(tx, userID); err != nil {
		return cul, err
	}
	if err := tx.Where("user_id = ?", userID).First(&cul).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		if _, err := createCultivationIfMissingInTx(tx, userID); err != nil {
			return cul, err
		}
	} else if err != nil {
		return cul, err
	}
	if err := lockCultivationListeningTx(tx, userID); err != nil {
		return cul, err
	}
	if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
		return cul, err
	}
	// Adopt pill/manual changes before considering listening upgrades, within
	// this same transaction rather than through a read-side realm write.
	calculateCultivationRealm(&cul)
	var base float64
	var err error
	var version string
	total, base, version, err = settleListeningDaysInTx(tx, &cul, now)
	if err != nil {
		return cul, err
	}
	settled := cul.SectListeningSettledHours
	nextSettled := cultivationContributionHighWater(base)
	if nextSettled > settled {
		delta := int(math.Floor(nextSettled)) - int(math.Floor(settled))
		if delta > 0 {
			if err := AwardSectContributionInTx(tx, userID, delta, fmt.Sprintf("听书增长奖励 %d 贡献（不含异火修为）", delta), "listening_hours", SectDayKey(now)); err != nil {
				return cul, err
			}
		}
		settled = nextSettled
	}
	cul.TotalAudioTime = total
	if err := recordListeningUpgradeInTx(tx, &cul, now); err != nil {
		return cul, err
	}
	res := tx.Model(&model.Cultivation{}).Where("id = ?", cul.ID).Updates(map[string]interface{}{
		"total_audio_time": total, "sect_listening_settled_hours": settled,
		"listening_initialized": true, "listening_base_hours": base, "listening_retreat_version": version,
		"major_realm": cul.MajorRealm, "minor_realm": cul.MinorRealm, "listening_reward_realm": cul.ListeningRewardRealm,
	})
	if res.Error != nil {
		return cul, res.Error
	}
	if res.RowsAffected != 1 {
		return cul, storage.ErrCultivationNotFound
	}
	cul.TotalAudioTime = total
	cul.SectListeningSettledHours = settled
	return cul, nil
}

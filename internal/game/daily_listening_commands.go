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

	"ccoff-abs-bot/internal/model"
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
	now := time.Now()
	var u model.User
	if err := storage.DB.Select("telegram_id", "abs_user_id").Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		return model.DailyListeningStat{}, 0, false, err
	}
	if strings.TrimSpace(u.AbsUserID) == "" {
		return model.DailyListeningStat{}, 0, false, storage.ErrAbsUserIDEmpty
	}

	if _, ok := RefreshDailyListeningStatsFromABS(userID, u.AbsUserID); !ok {
		if stat, cacheOK := getTodayDailyListeningStat(userID, now); cacheOK {
			totalEffective, totalOK := totalCultivationEffectiveHoursFromDailyStats(userID, now)
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
	if storage.DB == nil || GetOrCreateCultivation(userID) == nil {
		return 0, false
	}
	enabled := spiritfire.Installed(storage.DB)
	total := 0.0
	var cul model.Cultivation
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Acquire a SQLite write lock before reading either the totals or the cursor.
		// The cursor, contribution payment and profile update commit together.
		if err := lockCultivationListeningTx(tx, userID); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
			return err
		}
		var base float64
		var err error
		total, base, err = dailyCultivationTotalsInTx(tx, userID, now, enabled)
		if err != nil {
			return err
		}
		settled := base // Legacy fallback: establish a baseline without backpay.
		if cul.SectListeningSettledHours != nil {
			settled = *cul.SectListeningSettledHours
		}
		if base > settled {
			delta := int(math.Floor(base)) - int(math.Floor(settled))
			if delta > 0 {
				if err := awardSectContributionTx(tx, userID, delta, fmt.Sprintf("听书增长奖励 %d 贡献（不含异火修为）", delta), "listening_hours", SectDayKey(now)); err != nil {
					return err
				}
			}
			settled = base
		}
		res := tx.Model(&model.Cultivation{}).Where("id = ?", cul.ID).Updates(map[string]interface{}{
			"total_audio_time": total, "sect_listening_settled_hours": settled,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return storage.ErrCultivationNotFound
		}
		cul.TotalAudioTime = total
		cul.SectListeningSettledHours = &settled
		return nil
	})
	if err != nil {
		log.Printf("修为与宗门听书贡献结算失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return 0, false
	}
	SyncCultivationRealm(&cul)
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

// InitializeSectListeningContributionProgress runs once for each legacy profile,
// before startup jobs can refresh ABS. Cached ordinary hours + retreat evidence
// form the cutover baseline; historical payouts (including fire) are untouched.
func InitializeSectListeningContributionProgress(db *gorm.DB, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Cultivation{}).Where("sect_listening_settled_hours IS NULL").UpdateColumn("total_audio_time", gorm.Expr("total_audio_time")).Error; err != nil {
			return err
		}
		var profiles []model.Cultivation
		if err := tx.Where("sect_listening_settled_hours IS NULL").Find(&profiles).Error; err != nil {
			return err
		}
		for _, cul := range profiles {
			_, base, err := dailyCultivationTotalsInTx(tx, cul.UserID, now, false)
			if err != nil {
				return err
			}
			// Very old profiles may predate durable daily statistics. Keep
			// their existing total as a conservative no-backpay baseline.
			if base == 0 && cul.TotalAudioTime > 0 {
				var days int64
				if err := tx.Model(&model.DailyListeningStat{}).Where("user_id = ?", cul.UserID).Count(&days).Error; err != nil {
					return err
				}
				if days == 0 {
					base = cul.TotalAudioTime
				}
			}
			res := tx.Model(&model.Cultivation{}).Where("id = ? AND sect_listening_settled_hours IS NULL", cul.ID).UpdateColumn("sect_listening_settled_hours", base)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return storage.ErrCultivationNotFound
			}
			if err := storage.WriteAuditLogInTx(tx, 0, "SECT_LISTENING_BASELINE", fmt.Sprint(cul.UserID), 0, fmt.Sprintf("贡献结算基线 %.6f 小时；保留已发贡献，排除异火收益", base)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Personal cultivation and contribution-eligible totals are derived from the
// same read snapshot. EffectiveHours never includes retreat or fire effects.
func dailyCultivationTotalsInTx(tx *gorm.DB, userID int64, now time.Time, withFire bool) (total, baseTotal float64, err error) {
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
	for _, stat := range stats {
		base := stat.EffectiveHours + dailySectCaveRetreatBonusHours(stat, retreats)
		if withFire {
			if err = spiritfire.RefreshDaily(tx, &stat, base, now); err != nil {
				return
			}
		}
		baseTotal += base
		total += base + stat.SpiritFireHours
	}
	return
}

func totalCultivationEffectiveHoursFromDailyStats(userID int64, now time.Time) (float64, bool) {
	if storage.DB == nil || userID == 0 {
		return 0, false
	}
	enabled := spiritfire.Installed(storage.DB)
	total := 0.0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if enabled {
			if err := spiritfire.Lock(tx); err != nil {
				return err
			}
		}
		var err error
		total, _, err = dailyCultivationTotalsInTx(tx, userID, now, enabled)
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
	if !spiritfire.Installed(storage.DB) {
		return nil
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
		for _, retreat := range retreats {
			if _, err := decodeSectRetreatEvidence(retreat); err != nil {
				return err
			}
		}
		return spiritfire.RefreshDaily(tx, stat, stat.EffectiveHours+dailySectCaveRetreatBonusHours(*stat, retreats), now)
	})
}

func handleRefreshMyDailyListeningStat(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	stat, totalEffective, ok, err := refreshUserDailyListeningAndCultivation(msg.From.ID)
	if err != nil && !ok {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 刷新今日净修为失败，请稍后再试。")
		return
	}
	if !ok {
		telegram.ReplyText(bot, msg.Chat.ID, "📭 今日暂无有效听书记录。")
		return
	}

	telegram.ReplyText(bot, msg.Chat.ID, formatDailyListeningStat("✅ 今日净修为已刷新", stat, totalEffective))
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

	count := refreshSectMembersDailyListeningStats(member.SectID, 100)
	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("✅ 已刷新本宗门 `%d` 名成员的每日净修为。", count))
}

func handleRefreshAllDailyListeningStats(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	success, total, skipped := RefreshAllDailyListeningStatsWithOptions(true)
	// 全服级批量重算属于高危管理操作，补齐与其他管理变更一致的审计留痕。
	storage.WriteAuditLog(msg.From.ID, "REFRESH_ALL_DAILY_LISTENING", "daily_listening_stats",
		fmt.Sprintf("全服今日净修为刷新: 成功 %d/%d，跳过 %d", success, total, skipped))
	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf("✅ 全服今日净修为刷新完成：成功 `%d/%d`，跳过 `%d`。", success, total, skipped))
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

	dayKey := SectDayKey(time.Now())
	if len(fields) >= 3 {
		dayKey = strings.TrimSpace(fields[2])
	}

	var stat model.DailyListeningStat
	if err := storage.DB.Where("user_id = ? AND day_key = ?", targetID, dayKey).First(&stat).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			telegram.ReplyText(bot, msg.Chat.ID, "📭 未找到该用户该日期的每日净修为记录。")
		} else {
			log.Printf("⚠️ 查询每日净修为记录失败: user=%d day=%s err=%s", targetID, textfmt.FormatPlainValue(dayKey), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 每日净修为记录读取失败，请稍后再试。")
		}
		return
	}

	now := time.Now()
	totalEffective, totalOK := totalCultivationEffectiveHoursFromDailyStats(targetID, now)
	if !totalOK {
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 每日净修为累计读取失败，请稍后再试。")
		return
	}
	telegram.ReplyText(bot, msg.Chat.ID, formatDailyListeningStat(fmt.Sprintf("📊 用户 `%d` 每日净修为", targetID), stat, totalEffective))
}

func formatDailyListeningStat(title string, stat model.DailyListeningStat, totalEffective float64) string {
	if err := RefreshSpiritFireDailyStat(&stat, time.Now()); err != nil {
		return "❌ 每日修为及异火加成读取失败，请稍后重试。"
	}
	fetchStatus := stat.FetchStatus
	if fetchStatus == "" {
		fetchStatus = "unknown"
	}
	refreshReason := stat.RefreshReason
	if refreshReason == "" {
		refreshReason = stat.Source
	}

	retreatBonus := 0.0
	now := time.Now()
	if spiritfire.Installed(storage.DB) {
		retreatBonus = stat.BaseCultivationHours - stat.EffectiveHours
	} else if stat.DayKey == SectDayKey(now) {
		retreatBonus = ActiveSectCaveRetreatBonusHours(stat.UserID, now)
	}
	retreatLine := fmt.Sprintf("\n基础净修为：`%.2f` 小时\n异火额外修为：`+%.2f` 小时", stat.EffectiveHours+retreatBonus, stat.SpiritFireHours)
	if retreatBonus > 0 {
		retreatLine += fmt.Sprintf("\n洞府闭关加成：`+%.2f` 小时", retreatBonus)
	}

	return fmt.Sprintf(
		"%s\n\n日期：`%s`\n实际听书：`%.2f` 小时\n封顶计入：`%.2f` 小时\n今日净修为：`%.2f` 小时%s\n总净修为：`%.2f` 小时\n更新时间：`%s`\n来源：`%s`\n刷新状态：`%s`\n刷新原因：`%s`",
		title,
		stat.DayKey,
		stat.RawSeconds/3600.0,
		stat.CappedSeconds/3600.0,
		stat.EffectiveHours+retreatBonus+stat.SpiritFireHours,
		retreatLine,
		totalEffective,
		stat.LastFetchedAt.Format("2006-01-02 15:04:05"),
		stat.Source,
		fetchStatus,
		refreshReason,
	)
}

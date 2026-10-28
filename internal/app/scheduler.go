package app

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/books"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/metrics"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var autoBackupMu sync.Mutex

var dailyLifecycleMu sync.Mutex

var dailyListeningRefreshMu sync.Mutex

var dailyFusionPoolMu sync.Mutex

var sectWeeklyTaskAutoSettleMu sync.Mutex

var dailyFusionPoolLocation = storage.DailyOperationsLocation

func startBackgroundJobs(bot *tgbotapi.BotAPI) {
	game.StartBreakthroughGuardScheduler(bot)
	go func() {
		for {
			runBackgroundSchedulerTickSafely(bot)
			time.Sleep(1 * time.Minute)
		}
	}()

	log.Println("✅ 后台任务调度器已启动：播放异常风控每日北京时间 00:10 后执行；备份与生命周期任务每日 03:00 后执行。")
}

func runBackgroundSchedulerTickSafely(bot *tgbotapi.BotAPI) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ 后台任务调度器发生 panic，已恢复: panic=%s", textfmt.FormatPlainValue(r))
			telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 后台任务调度器发生异常，系统已自动恢复。\n\npanic: %s", textfmt.FormatPlainValue(r)))
		}
	}()

	if bot == nil || storage.DB == nil || config.AppConfig == nil {
		return
	}

	now := time.Now()

	// 求书工单滞留巡检：自带 1 小时节流，不依赖每日运营窗口（超时阈值需要小时级精度）。
	books.RunBookRequestStalePatrolIfNeeded(bot, now)

	// 新人任务自动结算：试用时长以天计、听书进度随时增长，必须 24 小时持续巡检
	// （自带 5 分钟节流），不能放进每日 03:00 运营窗口。
	account.RunReferralAutoClaimIfNeeded(bot, now)

	// 风控自带北京时间 00:10 门槛与重试节流，不依赖 03:00 运营窗口。
	game.RunListeningAbuseMonitorIfNeeded(bot, now)

	if !isDailyOperationsWindowOpen(now) {
		return
	}

	runDailyLifecycleIfNeeded(bot, now)
	runDailyListeningRefreshIfNeeded(bot, now)
	runSectWeeklyTaskAutoSettlementIfNeeded(bot, now)
	runDailyFusionPoolCollectIfNeeded(bot, now)
	runAutoBackupIfNeeded(bot, now)
}

func runDailyFusionPoolCollectIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	now, todayKey, due := dailyFusionPoolSchedule(now)
	if !due {
		return
	}

	lastSuccessDate, err := storage.GetSystemConfigStringChecked(storage.DailyFusionPoolLastSuccessDateKey)
	if err != nil {
		recordDailyFusionPoolStateReadFailure(bot, todayKey, storage.DailyFusionPoolLastSuccessDateKey, err)
		return
	}
	if lastSuccessDate == todayKey {
		return
	}

	if !dailyFusionPoolMu.TryLock() {
		return
	}
	defer dailyFusionPoolMu.Unlock()

	lastSuccessDate, err = storage.GetSystemConfigStringChecked(storage.DailyFusionPoolLastSuccessDateKey)
	if err != nil {
		recordDailyFusionPoolStateReadFailure(bot, todayKey, storage.DailyFusionPoolLastSuccessDateKey, err)
		return
	}
	if lastSuccessDate == todayKey {
		return
	}

	amount, err := randomDailyFusionPoolAmount()
	if err != nil {
		storage.SetSystemConfigError(storage.DailyFusionPoolLastErrorKey, err)
		log.Printf("⚠️ 每日天道灵气随机数生成失败: date=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 每日天道灵气收集失败\n\n日期: %s\n错误: %s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainError(err)))
		return
	}

	currentPool := 0
	isBurst := false
	didInject := false
	err = storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		claimed, err := claimDailyFusionPoolCollectionInTx(tx, todayKey)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}

		currentPool, isBurst, err = storage.AddPointsToFusionPoolInTx(tx, amount)
		if err != nil {
			return err
		}
		didInject = true

		nowText := time.Now().Format(time.RFC3339)
		if err := storage.UpsertSystemConfigValueInTx(tx, storage.DailyFusionPoolLastSuccessAtKey, nowText); err != nil {
			return err
		}
		if err := storage.UpsertSystemConfigValueInTx(tx, storage.DailyFusionPoolLastAmountKey, strconv.Itoa(amount)); err != nil {
			return err
		}
		return storage.UpsertSystemConfigValueInTx(tx, storage.DailyFusionPoolLastErrorKey, "")
	})
	if err != nil {
		storage.SetSystemConfigError(storage.DailyFusionPoolLastErrorKey, err)
		log.Printf("⚠️ 每日天道灵气注入失败: date=%s amount=%d err=%s", textfmt.FormatPlainValue(todayKey), amount, textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 每日天道灵气注入失败\n\n日期: %s\n灵气: %d\n错误: %s", textfmt.FormatPlainValue(todayKey), amount, textfmt.FormatPlainError(err)))
		return
	}

	if !didInject {
		return
	}

	log.Printf("✅ 每日天道灵气已注入奖池: date=%s amount=%d pool=%d/300", textfmt.FormatPlainValue(todayKey), amount, currentPool)
	notifyDailyFusionPoolInjection(bot, todayKey, amount, currentPool, isBurst)
	if isBurst {
		telegram.NotifyFusionPoolBurst(bot, config.AppConfig.NoticeGroupID, "每日天道灵气自然汇聚")
	}
}

func recordDailyFusionPoolStateReadFailure(bot *tgbotapi.BotAPI, todayKey string, key string, err error) {
	storage.SetSystemConfigError(storage.DailyFusionPoolLastErrorKey, err)
	log.Printf("每日天道灵气收集状态读取失败，已跳过本轮注入: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("每日天道灵气收集状态读取失败，已跳过本轮注入。\n\n日期：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func notifyDailyFusionPoolInjection(bot *tgbotapi.BotAPI, dateKey string, amount int, currentPool int, isBurst bool) {
	if bot == nil || config.AppConfig == nil || config.AppConfig.NoticeGroupID == 0 {
		return
	}

	progressText := fmt.Sprintf("当前水位：`%d/300`。", currentPool)
	if isBurst {
		progressText = fmt.Sprintf("本次注入后奖池蓄满，已降下天道机缘红包；新水位：`%d/300`。", currentPool)
	}

	notice := fmt.Sprintf(
		"🌊 **【天地灵气·自然汇聚】** 🌊\n\n"+
			"%s 天道巡游收集到 `%d` 分天地灵气，已注入全服【天道奖池】。\n"+
			"%s",
		dateKey,
		amount,
		progressText,
	)

	msg := tgbotapi.NewMessage(config.AppConfig.NoticeGroupID, notice)
	msg.ParseMode = "Markdown"
	if !telegram.EnqueueAutoDelete(bot, msg, "daily_fusion_pool_notice", telegram.TelegramAsyncPriorityNormal, "daily_fusion_pool_notice:"+dateKey) {
		log.Printf("⚠️ 每日天道灵气群提醒入队失败: chat=%d date=%s", config.AppConfig.NoticeGroupID, textfmt.FormatPlainValue(dateKey))
	}
}

func claimDailyFusionPoolCollectionInTx(tx *gorm.DB, todayKey string) (bool, error) {
	if tx == nil || strings.TrimSpace(todayKey) == "" {
		return false, fmt.Errorf("DAILY_FUSION_POOL_CLAIM_INVALID")
	}

	cfg := model.SystemConfig{Key: storage.DailyFusionPoolLastSuccessDateKey, Value: todayKey}
	createRes := tx.Clauses(storage.SystemConfigKeyDoNothingClause()).Create(&cfg)
	if createRes.Error != nil {
		return false, createRes.Error
	}
	if createRes.RowsAffected > 0 {
		return true, nil
	}

	res := tx.Model(&model.SystemConfig{}).
		Where("key = ? AND value <> ?", storage.DailyFusionPoolLastSuccessDateKey, todayKey).
		Update("value", todayKey)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func randomDailyFusionPoolAmount() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(6))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()) + 5, nil
}

func dailyFusionPoolSchedule(now time.Time) (time.Time, string, bool) {
	localNow := now.In(dailyFusionPoolLocation)
	return localNow, localNow.Format("2006-01-02"), localNow.Hour() >= storage.DailyFusionPoolCollectHour
}

func isDailyOperationsWindowOpen(now time.Time) bool {
	return storage.DailyOperationsLocalTime(now).Hour() >= storage.DailyOperationsStartHour
}

func runDailyListeningRefreshIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	lastAt, err := storage.GetSystemConfigTimeChecked(storage.DailyListeningRefreshLastAtKey)
	if err != nil {
		recordDailyListeningRefreshStateReadFailure(bot, storage.DailyListeningRefreshLastAtKey, err)
		return
	}
	if !lastAt.IsZero() && now.Sub(lastAt) < storage.DailyListeningRefreshInterval {
		return
	}

	if !dailyListeningRefreshMu.TryLock() {
		return
	}
	defer dailyListeningRefreshMu.Unlock()

	lastAt, err = storage.GetSystemConfigTimeChecked(storage.DailyListeningRefreshLastAtKey)
	if err != nil {
		recordDailyListeningRefreshStateReadFailure(bot, storage.DailyListeningRefreshLastAtKey, err)
		return
	}
	if !lastAt.IsZero() && now.Sub(lastAt) < storage.DailyListeningRefreshInterval {
		return
	}

	success, total, skipped := game.RefreshAllDailyListeningStatsWithOptions(false)
	if err := storage.SetSystemConfigStringChecked(storage.DailyListeningRefreshLastAtKey, time.Now().Format(time.RFC3339)); err != nil {
		recordDailyListeningRefreshStateWriteFailure(bot, storage.DailyListeningRefreshLastAtKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.DailyListeningRefreshLastSuccessKey, strconv.Itoa(success)); err != nil {
		recordDailyListeningRefreshStateWriteFailure(bot, storage.DailyListeningRefreshLastSuccessKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.DailyListeningRefreshLastTotalKey, strconv.Itoa(total)); err != nil {
		recordDailyListeningRefreshStateWriteFailure(bot, storage.DailyListeningRefreshLastTotalKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.DailyListeningRefreshLastSkippedKey, strconv.Itoa(skipped)); err != nil {
		recordDailyListeningRefreshStateWriteFailure(bot, storage.DailyListeningRefreshLastSkippedKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.DailyListeningRefreshLastErrorKey, ""); err != nil {
		recordDailyListeningRefreshStateWriteFailure(bot, storage.DailyListeningRefreshLastErrorKey, err)
		return
	}
	log.Printf("✅ 后台每日净修为刷新完成: success=%d total=%d skipped=%d", success, total, skipped)
}

func recordDailyListeningRefreshStateReadFailure(bot *tgbotapi.BotAPI, key string, err error) {
	storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
	log.Printf("每日听书缓存刷新状态读取失败，已跳过本轮刷新: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("每日听书缓存刷新状态读取失败，已跳过本轮刷新。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func recordDailyListeningRefreshStateWriteFailure(bot *tgbotapi.BotAPI, key string, err error) {
	if key != storage.DailyListeningRefreshLastErrorKey {
		storage.SetSystemConfigError(storage.DailyListeningRefreshLastErrorKey, err)
	}
	log.Printf("每日听书缓存刷新状态写入失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("每日听书缓存刷新已执行完成，但状态写入失败，请人工核查，避免重复批量刷新 ABS。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func runSectWeeklyTaskAutoSettlementIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	targetWeek, due := sectWeeklyTaskAutoSettlementTargetWeek(now)
	if !due {
		return
	}

	targetWeekKey := targetWeek.Format("2006-01-02")
	lastSettledWeek, err := storage.GetSystemConfigStringChecked(storage.SectWeeklyTaskAutoSettleLastWeekKey)
	if err != nil {
		recordSectWeeklyTaskAutoSettleStateReadFailure(bot, targetWeekKey, storage.SectWeeklyTaskAutoSettleLastWeekKey, err)
		return
	}
	if strings.TrimSpace(lastSettledWeek) == targetWeekKey {
		return
	}

	if !sectWeeklyTaskAutoSettleMu.TryLock() {
		return
	}
	defer sectWeeklyTaskAutoSettleMu.Unlock()

	lastSettledWeek, err = storage.GetSystemConfigStringChecked(storage.SectWeeklyTaskAutoSettleLastWeekKey)
	if err != nil {
		recordSectWeeklyTaskAutoSettleStateReadFailure(bot, targetWeekKey, storage.SectWeeklyTaskAutoSettleLastWeekKey, err)
		return
	}
	if strings.TrimSpace(lastSettledWeek) == targetWeekKey {
		return
	}

	results, failed := autoSettleSectWeeklyTaskRewards(bot, targetWeek)
	if failed == 0 {
		if err := storage.SetSystemConfigStringChecked(storage.SectWeeklyTaskAutoSettleLastWeekKey, targetWeekKey); err != nil {
			recordSectWeeklyTaskAutoSettleStateWriteFailure(bot, targetWeekKey, storage.SectWeeklyTaskAutoSettleLastWeekKey, err)
			return
		}
	}
	if len(results) > 0 {
		log.Printf("✅ 宗门周目标自动结算完成: week=%s settled=%d failed=%d", textfmt.FormatPlainValue(targetWeekKey), len(results), failed)
	}
}

func recordSectWeeklyTaskAutoSettleStateReadFailure(bot *tgbotapi.BotAPI, targetWeekKey string, key string, err error) {
	log.Printf("⚠️ 宗门周目标自动结算状态读取失败，已跳过本轮结算: week=%s key=%s err=%s", textfmt.FormatPlainValue(targetWeekKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 宗门周目标自动结算状态读取失败，已跳过本轮结算。\n\n周次：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(targetWeekKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func recordSectWeeklyTaskAutoSettleStateWriteFailure(bot *tgbotapi.BotAPI, targetWeekKey string, key string, err error) {
	log.Printf("宗门周目标自动结算状态写入失败: week=%s key=%s err=%s", textfmt.FormatPlainValue(targetWeekKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("宗门周目标自动结算已执行完成，但状态写入失败，请人工核查，避免重复扫描。\n\n周次：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(targetWeekKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func sectWeeklyTaskAutoSettlementTargetWeek(now time.Time) (time.Time, bool) {
	localNow := storage.DailyOperationsLocalTime(now)
	if localNow.Weekday() != time.Monday || localNow.Hour() < storage.SectWeeklyTaskAutoSettleHour {
		return time.Time{}, false
	}
	return game.SectWeekStart(localNow.AddDate(0, 0, -7)), true
}

func autoSettleSectWeeklyTaskRewards(bot *tgbotapi.BotAPI, targetWeek time.Time) ([]game.SectWeeklyTaskSettlementResult, int) {
	var sects []model.Sect
	if err := storage.DB.Select("id", "name").Find(&sects).Error; err != nil {
		log.Printf("⚠️ 查询宗门周目标自动结算列表失败: err=%s", textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 宗门周目标自动结算失败：查询宗门列表失败。\n\n错误：%s", textfmt.FormatPlainError(err)))
		return nil, 1
	}

	results := make([]game.SectWeeklyTaskSettlementResult, 0, len(sects))
	failed := 0
	for _, sect := range sects {
		var result game.SectWeeklyTaskSettlementResult
		err := storage.DB.Transaction(func(tx *gorm.DB) error {
			return game.SettleSectWeeklyTaskRewardForSectTx(tx, int64(sect.ID), targetWeek, 0, "系统自动结算", &result)
		})
		if err != nil {
			switch game.SectErrorCode(err) {
			case "SECT_WEEKLY_TASK_NOT_ACHIEVED", "SECT_WEEKLY_TASK_ALREADY_SETTLED":
				continue
			default:
				failed++
				log.Printf("⚠️ 宗门周目标自动结算失败: sect=%d week=%s err=%s", sect.ID, textfmt.FormatPlainValue(game.SectWeekKey(targetWeek)), textfmt.FormatPlainError(err))
				continue
			}
		}
		results = append(results, result)
		notifySectWeeklyTaskSettlementLeaders(bot, result)
	}

	if failed > 0 {
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 宗门周目标自动结算部分失败\n\n周次：%s\n成功：%d\n失败：%d\n请查看日志并必要时手动补结算。", textfmt.FormatPlainValue(game.SectWeekKey(targetWeek)), len(results), failed))
	}
	return results, failed
}

func notifySectWeeklyTaskSettlementLeaders(bot *tgbotapi.BotAPI, result game.SectWeeklyTaskSettlementResult) {
	if bot == nil || result.SectID == 0 {
		return
	}

	var leaders []model.SectMember
	if err := storage.DB.Select("user_id", "role").
		Where("sect_id = ? AND role IN ?", result.SectID, []string{game.SectRoleOwner, game.SectRoleElder}).
		Find(&leaders).Error; err != nil {
		log.Printf("⚠️ 查询宗门周目标通知对象失败: sect=%d err=%s", result.SectID, textfmt.FormatPlainError(err))
		return
	}

	text := formatSectWeeklyTaskSettlementNotice(result)
	sent := make(map[int64]bool, len(leaders))
	for _, leader := range leaders {
		if leader.UserID == 0 || sent[leader.UserID] {
			continue
		}
		sent[leader.UserID] = true
		msg := tgbotapi.NewMessage(leader.UserID, text)
		msg.ParseMode = "Markdown"
		if _, err := telegram.SendNoAutoDelete(bot, msg); err != nil {
			log.Printf("⚠️ 宗门周目标结算私信提醒失败: sect=%d user=%d err=%s", result.SectID, leader.UserID, textfmt.FormatTelegramSendError(err))
		}
	}
}

func formatSectWeeklyTaskSettlementNotice(result game.SectWeeklyTaskSettlementResult) string {
	return fmt.Sprintf(
		"📋 **宗门周目标已自动结算**\n\n"+
			"宗门：**%s**\n"+
			"周次：`%s`\n"+
			"达成目标：`%d/3`\n"+
			"全宗累计签到：`%d/%d`\n"+
			"全宗净修为增长：`%.1f/%d` 小时\n"+
			"全宗完成个人任务：`%d/%d`\n\n"+
			"基础奖励：资金 +`%d`，声望 +`%d`\n"+
			"超额合计：`%d%%`\n"+
			"超额奖励：资金 +`%d`，声望 +`%d`\n"+
			"最终发放：资金 +`%d`，声望 +`%d`",
		textfmt.EscapeMarkdown(result.SectName),
		result.Stats.WeekKey,
		result.Reward.AchievedCount,
		result.Stats.SignCount,
		game.SectWeeklySignTarget,
		result.Stats.ListenHours,
		game.SectWeeklyListenHourTarget,
		result.Stats.TaskClaimCount,
		game.SectWeeklyTaskTarget,
		result.Reward.BaseFunds,
		result.Reward.BasePrestige,
		result.Reward.ExcessPercentTotal,
		result.Reward.ExcessFunds,
		result.Reward.ExcessPrestige,
		result.Reward.Funds,
		result.Reward.Prestige,
	)
}

func runDailyLifecycleIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	todayKey := storage.DailyOperationsDateKey(now)

	lastSuccessDate, err := storage.GetSystemConfigStringChecked(storage.DailyLifecycleLastSuccessDateKey)
	if err != nil {
		recordDailyLifecycleStateReadFailure(todayKey, storage.DailyLifecycleLastSuccessDateKey, err)
		return
	}
	if strings.TrimSpace(lastSuccessDate) == todayKey {
		return
	}

	if !dailyLifecycleMu.TryLock() {
		return
	}
	defer dailyLifecycleMu.Unlock()

	lastSuccessDate, err = storage.GetSystemConfigStringChecked(storage.DailyLifecycleLastSuccessDateKey)
	if err != nil {
		recordDailyLifecycleStateReadFailure(todayKey, storage.DailyLifecycleLastSuccessDateKey, err)
		return
	}
	if strings.TrimSpace(lastSuccessDate) == todayKey {
		return
	}

	log.Println("🕵️ 开始执行每日用户生命周期巡检...")
	if err := runDailyLifecycleOperations(bot); err != nil {
		storage.SetSystemConfigError(storage.DailyLifecycleLastErrorKey, err)
		log.Printf("⚠️ 每日用户生命周期巡检失败: date=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainError(err))
		return
	}

	if err := storage.SetSystemConfigStringChecked(storage.DailyLifecycleLastSuccessDateKey, todayKey); err != nil {
		recordDailyLifecycleStateWriteFailure(bot, todayKey, storage.DailyLifecycleLastSuccessDateKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.DailyLifecycleLastErrorKey, ""); err != nil {
		recordDailyLifecycleStateWriteFailure(bot, todayKey, storage.DailyLifecycleLastErrorKey, err)
		return
	}
	log.Printf("✅ 每日用户生命周期巡检完成: date=%s", textfmt.FormatPlainValue(todayKey))
}

func recordDailyLifecycleStateReadFailure(todayKey string, key string, err error) {
	storage.SetSystemConfigError(storage.DailyLifecycleLastErrorKey, err)
	log.Printf("⚠️ 每日用户生命周期巡检状态读取失败，已跳过本轮巡检: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
}

func recordDailyLifecycleStateWriteFailure(bot *tgbotapi.BotAPI, todayKey string, key string, err error) {
	if key != storage.DailyLifecycleLastErrorKey {
		storage.SetSystemConfigError(storage.DailyLifecycleLastErrorKey, err)
	}
	log.Printf("每日用户生命周期巡检状态写入失败: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("每日用户生命周期巡检已执行完成，但状态写入失败，请人工核查，避免重复巡检。\n\n日期：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func runAutoBackupIfNeeded(bot *tgbotapi.BotAPI, now time.Time) {
	if config.AppConfig.BackupGroupID == 0 {
		return
	}

	todayKey := storage.DailyOperationsDateKey(now)

	lastSuccessDate, err := storage.GetSystemConfigStringChecked(storage.AutoBackupLastSuccessDateKey)
	if err != nil {
		recordAutoBackupStateReadFailure(todayKey, storage.AutoBackupLastSuccessDateKey, err)
		return
	}
	if strings.TrimSpace(lastSuccessDate) == todayKey {
		return
	}

	retryCount, err := getTodayAutoBackupRetryCountChecked(todayKey)
	if err != nil {
		recordAutoBackupStateReadFailure(todayKey, storage.AutoBackupRetryCountKey, err)
		return
	}
	if retryCount >= storage.AutoBackupMaxRetryCount {
		return
	}

	lastAttemptAt, err := storage.GetSystemConfigTimeChecked(storage.AutoBackupLastAttemptAtKey)
	if err != nil {
		recordAutoBackupStateReadFailure(todayKey, storage.AutoBackupLastAttemptAtKey, err)
		return
	}
	if !lastAttemptAt.IsZero() && storage.DailyOperationsDateKey(lastAttemptAt) == todayKey {
		delay := autoBackupRetryDelay(retryCount)
		if time.Since(lastAttemptAt) < delay {
			return
		}
	}

	if !autoBackupMu.TryLock() {
		return
	}
	defer autoBackupMu.Unlock()

	lastSuccessDate, err = storage.GetSystemConfigStringChecked(storage.AutoBackupLastSuccessDateKey)
	if err != nil {
		recordAutoBackupStateReadFailure(todayKey, storage.AutoBackupLastSuccessDateKey, err)
		return
	}
	if strings.TrimSpace(lastSuccessDate) == todayKey {
		return
	}

	retryCount, err = getTodayAutoBackupRetryCountChecked(todayKey)
	if err != nil {
		recordAutoBackupStateReadFailure(todayKey, storage.AutoBackupRetryCountKey, err)
		return
	}
	if retryCount >= storage.AutoBackupMaxRetryCount {
		return
	}

	runAutoBackupAttempt(bot, now, retryCount)
}

func runAutoBackupAttempt(bot *tgbotapi.BotAPI, now time.Time, retryCount int) {
	todayKey := storage.DailyOperationsDateKey(now)
	attemptNo := retryCount + 1

	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupLastAttemptAtKey, now.Format(time.RFC3339)); err != nil {
		recordAutoBackupAttemptStateWriteFailure(bot, todayKey, storage.AutoBackupLastAttemptAtKey, err)
		return
	}

	log.Printf("📦 开始执行每日自动加密备份: date=%s attempt=%d/%d", textfmt.FormatPlainValue(todayKey), attemptNo, storage.AutoBackupMaxRetryCount)

	messageID, err := telegram.SendEncryptedBackupToTelegram(bot, "daily_auto")
	if err != nil {
		if writeErr := storage.SetSystemConfigStringChecked(storage.AutoBackupRetryCountKey, strconv.Itoa(attemptNo)); writeErr != nil {
			recordAutoBackupAttemptStateWriteFailure(bot, todayKey, storage.AutoBackupRetryCountKey, writeErr)
		}
		if writeErr := storage.SetSystemConfigStringChecked(storage.AutoBackupLastErrorKey, textfmt.FormatPlainError(err)); writeErr != nil {
			recordAutoBackupAttemptStateWriteFailure(bot, todayKey, storage.AutoBackupLastErrorKey, writeErr)
		}

		log.Printf("⚠️ 每日自动加密备份失败: date=%s attempt=%d/%d err=%s", textfmt.FormatPlainValue(todayKey), attemptNo, storage.AutoBackupMaxRetryCount, textfmt.FormatPlainError(err))
		notifyAutoBackupFailure(bot, todayKey, attemptNo, err)
		return
	}

	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupLastSuccessDateKey, todayKey); err != nil {
		recordAutoBackupStateWriteFailure(bot, todayKey, storage.AutoBackupLastSuccessDateKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupLastSuccessAtKey, time.Now().Format(time.RFC3339)); err != nil {
		recordAutoBackupStateWriteFailure(bot, todayKey, storage.AutoBackupLastSuccessAtKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupLastMessageIDKey, strconv.Itoa(messageID)); err != nil {
		recordAutoBackupStateWriteFailure(bot, todayKey, storage.AutoBackupLastMessageIDKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupRetryCountKey, "0"); err != nil {
		recordAutoBackupStateWriteFailure(bot, todayKey, storage.AutoBackupRetryCountKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.AutoBackupLastErrorKey, ""); err != nil {
		recordAutoBackupStateWriteFailure(bot, todayKey, storage.AutoBackupLastErrorKey, err)
		return
	}

	log.Printf("✅ 每日自动加密备份成功: date=%s message_id=%d", textfmt.FormatPlainValue(todayKey), messageID)
}

func autoBackupRetryDelay(retryCount int) time.Duration {
	switch retryCount {
	case 0:
		return 0
	case 1:
		return 5 * time.Minute
	case 2:
		return 15 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return 60 * time.Minute
	}
}

func getTodayAutoBackupRetryCountForStatus(todayKey string) (int, string, bool) {
	count, err := getTodayAutoBackupRetryCountChecked(todayKey)
	if err != nil {
		log.Printf("⚠️ 自动备份重试次数读取失败，状态暂不可用: date=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainError(err))
		return 0, "读取失败", false
	}
	return count, strconv.Itoa(count), true
}

func getTodayAutoBackupRetryCountChecked(todayKey string) (int, error) {
	lastAttemptAt, err := storage.GetSystemConfigTimeChecked(storage.AutoBackupLastAttemptAtKey)
	if err != nil {
		return 0, err
	}
	if lastAttemptAt.IsZero() || storage.DailyOperationsDateKey(lastAttemptAt) != todayKey {
		return 0, nil
	}

	raw, err := storage.GetSystemConfigStringChecked(storage.AutoBackupRetryCountKey)
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid auto backup retry count %s=%q: %w", storage.AutoBackupRetryCountKey, raw, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid auto backup retry count %s=%q", storage.AutoBackupRetryCountKey, raw)
	}

	return n, nil
}

func recordAutoBackupStateReadFailure(todayKey string, key string, err error) {
	storage.SetSystemConfigError(storage.AutoBackupLastErrorKey, err)
	log.Printf("⚠️ 自动备份状态读取失败，已跳过本轮备份: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
}

func recordAutoBackupStateWriteFailure(bot *tgbotapi.BotAPI, todayKey string, key string, err error) {
	if key != storage.AutoBackupLastErrorKey {
		storage.SetSystemConfigError(storage.AutoBackupLastErrorKey, err)
	}
	log.Printf("自动备份状态写入失败: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("每日自动加密备份已发送成功，但状态写入失败，请人工核查，避免重复外发备份。\n\n日期：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func recordAutoBackupAttemptStateWriteFailure(bot *tgbotapi.BotAPI, todayKey string, key string, err error) {
	if key != storage.AutoBackupLastErrorKey {
		if writeErr := storage.SetSystemConfigStringChecked(storage.AutoBackupLastErrorKey, textfmt.FormatPlainError(err)); writeErr != nil {
			log.Printf("自动备份尝试状态写入失败，且最近错误写入也失败: date=%s key=%s err=%s last_error_write_err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err), textfmt.FormatPlainError(writeErr))
		}
	}
	log.Printf("自动备份尝试状态写入失败: date=%s key=%s err=%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("自动备份尝试状态写入失败，请人工核查，避免重复外发备份或重试次数失真。\n\n日期：%s\n配置：%s\n错误：%s", textfmt.FormatPlainValue(todayKey), textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func notifyAutoBackupFailure(bot *tgbotapi.BotAPI, todayKey string, attemptNo int, err error) {
	statusText := "系统会继续按重试策略自动补跑。"
	if attemptNo >= storage.AutoBackupMaxRetryCount {
		statusText = "今日自动备份已达到最大重试次数，请尽快人工检查。"
	}

	telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf(
		"⚠️ 每日自动备份失败\n\n日期: %s\n重试: %d/%d\n错误: %s\n\n%s",
		textfmt.FormatPlainValue(todayKey),
		attemptNo,
		storage.AutoBackupMaxRetryCount,
		textfmt.FormatPlainError(err),
		statusText,
	))
}

func formatBackupStatusReport() string {
	now := time.Now()
	todayKey := storage.DailyOperationsDateKey(now)

	backupGroupStatus := "未配置"
	if config.AppConfig != nil && config.AppConfig.BackupGroupID != 0 {
		backupGroupStatus = fmt.Sprintf("%d", config.AppConfig.BackupGroupID)
	}

	lastSuccessDate, lastSuccessDateAvailable := storage.GetSystemConfigStringForStatus(storage.AutoBackupLastSuccessDateKey, "从未成功")
	if lastSuccessDate == "" {
		lastSuccessDate = "从未成功"
	}

	lastSuccessAt, lastSuccessAtAvailable := storage.FormatSystemConfigTimeForStatus(storage.AutoBackupLastSuccessAtKey)
	lastAttemptAt, lastAttemptAtAvailable := storage.FormatSystemConfigTimeForStatus(storage.AutoBackupLastAttemptAtKey)
	lastMessageID, lastMessageIDAvailable := storage.GetSystemConfigStringForStatus(storage.AutoBackupLastMessageIDKey, "无")
	if lastMessageID == "" {
		lastMessageID = "无"
	}

	retryCount, retryCountText, retryCountAvailable := getTodayAutoBackupRetryCountForStatus(todayKey)
	lastError, lastErrorAvailable := storage.GetSystemConfigStringForStatus(storage.AutoBackupLastErrorKey, "无")
	if lastError == "" {
		lastError = "无"
	}

	pinnedMessageID, pinnedMessageIDAvailable := storage.GetSystemConfigStringForStatus(storage.BackupLastPinnedMessageIDKey, "无")
	if pinnedMessageID == "" {
		pinnedMessageID = "无"
	}
	pinError, pinErrorAvailable := storage.GetSystemConfigStringForStatus(storage.BackupLastPinErrorKey, "无")
	backupStateAvailable := lastSuccessDateAvailable && lastSuccessAtAvailable && lastAttemptAtAvailable &&
		lastMessageIDAvailable && retryCountAvailable && lastErrorAvailable && pinnedMessageIDAvailable && pinErrorAvailable
	if pinError == "" {
		pinError = "无"
	}

	status := "✅ 今日自动备份已成功"
	if config.AppConfig == nil || config.AppConfig.BackupGroupID == 0 {
		status = "⚠️ 未配置备份群组，自动备份不会发送"
	} else if !backupStateAvailable {
		status = "⚠️ 自动备份状态暂不可用"
	} else if lastSuccessDate != todayKey {
		if retryCount >= storage.AutoBackupMaxRetryCount {
			status = "❌ 今日自动备份已达到最大重试次数"
		} else if retryCount > 0 {
			status = "⚠️ 今日自动备份失败后等待重试"
		} else {
			status = "⏳ 今日自动备份尚未成功"
		}
	}

	return fmt.Sprintf(
		"📦 **数据库备份状态**\n\n"+
			"状态：%s\n"+
			"备份群组：`%s`\n"+
			"今日日期：`%s`\n"+
			"今日重试：`%s/%d`\n\n"+
			"最近成功日期：`%s`\n"+
			"最近成功时间：`%s`\n"+
			"最近尝试时间：`%s`\n"+
			"最近备份消息：`%s`\n"+
			"当前置顶消息：`%s`\n\n"+
			"最近备份错误：`%s`\n"+
			"最近置顶错误：`%s`",
		status,
		backupGroupStatus,
		todayKey,
		textfmt.EscapeMarkdown(retryCountText),
		storage.AutoBackupMaxRetryCount,
		textfmt.EscapeMarkdown(lastSuccessDate),
		textfmt.EscapeMarkdown(lastSuccessAt),
		textfmt.EscapeMarkdown(lastAttemptAt),
		textfmt.EscapeMarkdown(lastMessageID),
		textfmt.EscapeMarkdown(pinnedMessageID),
		textfmt.FormatSystemConfigErrorForMarkdown(lastError),
		textfmt.FormatSystemConfigErrorForMarkdown(pinError),
	)
}

func formatBackgroundStatusReport() string {
	now := time.Now()
	todayKey := storage.DailyOperationsDateKey(now)

	lifecycleDate, lifecycleDateAvailable := storage.GetSystemConfigStringForStatus(storage.DailyLifecycleLastSuccessDateKey, "从未完成")
	if lifecycleDate == "" {
		lifecycleDate = "从未完成"
	}
	lifecycleError, lifecycleErrorAvailable := storage.GetSystemConfigStringForStatus(storage.DailyLifecycleLastErrorKey, "无")
	if lifecycleError == "" {
		lifecycleError = "无"
	}
	lifecycleStatus := "✅ 今日日常生命周期巡检已完成"
	lifecycleStateAvailable := lifecycleDateAvailable && lifecycleErrorAvailable
	if !lifecycleStateAvailable {
		lifecycleStatus = "⚠️ 生命周期巡检状态暂不可用"
	} else if lifecycleDate != todayKey {
		if !isDailyOperationsWindowOpen(now) {
			lifecycleStatus = "⏳ 今日调度窗口尚未开始"
		} else {
			lifecycleStatus = "⚠️ 今日日常生命周期巡检尚未完成"
		}
	}

	refreshAt, refreshAtAvailable := storage.FormatSystemConfigTimeForStatus(storage.DailyListeningRefreshLastAtKey)
	refreshSuccess, refreshSuccessAvailable := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastSuccessKey, "0")
	if refreshSuccess == "" {
		refreshSuccess = "0"
	}
	refreshTotal, refreshTotalAvailable := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastTotalKey, "0")
	if refreshTotal == "" {
		refreshTotal = "0"
	}
	refreshSkipped, refreshSkippedAvailable := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastSkippedKey, "0")
	if refreshSkipped == "" {
		refreshSkipped = "0"
	}
	refreshError, refreshErrorAvailable := storage.GetSystemConfigStringForStatus(storage.DailyListeningRefreshLastErrorKey, "无")
	if refreshError == "" {
		refreshError = "无"
	}

	refreshStateAvailable := refreshAtAvailable && refreshSuccessAvailable && refreshTotalAvailable && refreshSkippedAvailable && refreshErrorAvailable
	if !refreshStateAvailable {
		refreshAt = "状态暂不可用"
	}

	backupDate, backupDateAvailable := storage.GetSystemConfigStringForStatus(storage.AutoBackupLastSuccessDateKey, "从未成功")
	if backupDate == "" {
		backupDate = "从未成功"
	}
	backupRetryCount, backupRetryCountText, backupRetryCountAvailable := getTodayAutoBackupRetryCountForStatus(todayKey)
	backupError, backupErrorAvailable := storage.GetSystemConfigStringForStatus(storage.AutoBackupLastErrorKey, "无")
	if backupError == "" {
		backupError = "无"
	}

	backupStatus := "✅ 今日自动备份已成功"
	backupStateAvailable := backupDateAvailable && backupRetryCountAvailable && backupErrorAvailable
	if config.AppConfig == nil || config.AppConfig.BackupGroupID == 0 {
		backupStatus = "⚠️ 未配置备份群组"
	} else if !backupStateAvailable {
		backupStatus = "⚠️ 自动备份状态暂不可用"
	} else if backupDate != todayKey {
		if backupRetryCount >= storage.AutoBackupMaxRetryCount {
			backupStatus = "❌ 今日自动备份已达最大重试次数"
		} else if !isDailyOperationsWindowOpen(now) {
			backupStatus = "⏳ 今日调度窗口尚未开始"
		} else {
			backupStatus = "⚠️ 今日自动备份尚未成功"
		}
	}

	return fmt.Sprintf(
		"🛠 **后台任务状态**\n\n"+
			"当前日期：`%s`\n"+
			"调度窗口：每日 `%02d:00` 后，每分钟巡检一次\n\n"+
			"生命周期巡检：%s\n"+
			"最近完成日期：`%s`\n"+
			"生命周期错误：`%s`\n\n"+
			"每日听书刷新：`%s`\n"+
			"刷新结果：成功 `%s` / 总数 `%s` / 跳过 `%s`\n"+
			"刷新错误：`%s`\n\n"+
			"自动备份：%s\n"+
			"最近成功日期：`%s`\n"+
			"今日备份重试：`%s/%d`\n"+
			"备份错误：`%s`\n\n"+
			"%s",
		todayKey,
		storage.DailyOperationsStartHour,
		lifecycleStatus,
		textfmt.EscapeMarkdown(lifecycleDate),
		textfmt.FormatSystemConfigErrorForMarkdown(lifecycleError),
		textfmt.EscapeMarkdown(refreshAt),
		textfmt.EscapeMarkdown(refreshSuccess),
		textfmt.EscapeMarkdown(refreshTotal),
		textfmt.EscapeMarkdown(refreshSkipped),
		textfmt.FormatSystemConfigErrorForMarkdown(refreshError),
		backupStatus,
		textfmt.EscapeMarkdown(backupDate),
		textfmt.EscapeMarkdown(backupRetryCountText),
		storage.AutoBackupMaxRetryCount,
		textfmt.FormatSystemConfigErrorForMarkdown(backupError),
		metrics.FormatRuntimeMetricsReport(),
	)
}

func runDailyLifecycleOperations(bot *tgbotapi.BotAPI) error {
	now := time.Now()

	var users []model.User
	// 排除白名单和未挂载 ABS 账号的幽灵用户
	if err := storage.DB.Where("is_whitelist = ? AND abs_user_id != ?", false, "").Find(&users).Error; err != nil {
		log.Printf("⚠️ 生命周期巡检读取用户列表失败: err=%s", textfmt.FormatPlainError(err))
		telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 生命周期巡检失败：读取用户列表失败。\n\n错误：%s", textfmt.FormatPlainError(err)))
		return err
	}

	for _, u := range users {
		if u.ExpireAt == nil {
			continue // 永久有效账号跳过
		}

		daysLeft := int(u.ExpireAt.Sub(now).Hours() / 24)

		// 阶段 1：到期前 3 天每日预警
		if daysLeft >= 0 && daysLeft < config.AppConfig.NotifyBeforeDays {
			msg := fmt.Sprintf("⚠️ **账号到期预警**\n\n您的听书账号将在 `%d` 天后到期。\n💡 请及时使用【🪙 积分兑换中心】获取续期卡，以免影响收听！", daysLeft+1)
			telegram.ReplyText(bot, u.TelegramID, msg)
			continue
		}

		// 阶段 2：刚过期 -> 立即封禁，进入宽限期
		if daysLeft < 0 && !u.IsSuspended {
			err := abs.DefaultClient.SetUserActiveStatus(u.AbsUserID, false)
			if err != nil {
				log.Printf("auto suspend ABS status update failed: user=%s tg=%d abs=%s err=%s",
					textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err))
				continue
			}
			if err := applySuspendLocalStatusWithAudit(0, u.TelegramID, u.AbsUserID, true, "AUTO_SUSPEND_EXPIRED_USER",
				fmt.Sprintf("expired lifecycle suspend; expire_at=%s grace_days=%d", u.ExpireAt.Format(time.RFC3339), config.AppConfig.AccountGraceDays)); err != nil {
				log.Printf("auto suspend local state or audit write failed: user=%s tg=%d abs=%s err=%s",
					textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err))
				detail := fmt.Sprintf("expired lifecycle suspend ABS state updated but local state/audit failed: username=%s tg=%d abs_user_id=%s expire_at=%s grace_days=%d error=%s",
					textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), u.ExpireAt.Format(time.RFC3339), config.AppConfig.AccountGraceDays, textfmt.FormatPlainError(err))
				if auditErr := storage.WriteAuditLogInTx(storage.DB, 0, "AUTO_SUSPEND_EXPIRED_USER_LOCAL_FAILED", fmt.Sprintf("%d", u.TelegramID), 0, detail); auditErr != nil {
					log.Printf("auto suspend local failure audit write failed: user=%s tg=%d abs=%s err=%s",
						textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(auditErr))
					telegram.NotifySuperAdminsPlain(bot, fmt.Sprintf("⚠️ 生命周期自动封禁已更新 ABS，但本地状态或成功审计失败，且失败审计写入失败。\n用户：%d\nABS：%s\n本地错误：%s\n审计错误：%s\n请立即人工核查。", u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err), textfmt.FormatPlainError(auditErr)))
				}
				continue
			}
			graceDays := config.AppConfig.AccountGraceDays
			msg := fmt.Sprintf("⛔ **账号已暂停使用**\n\n您的听书账号已到期，系统已暂停您的收听权限。\n\n⏳ **数据保留期**：您的账户数据将为您保留 `%d` 天。宽限期到期后系统将**永久删除**您的账户及所有收听记录。\n\n💡 请尽快使用【💳 使用续期卡】恢复权限！", graceDays)
			telegram.ReplyText(bot, u.TelegramID, msg)
			log.Printf("⛔ 已封禁过期用户: %s (TG: %d)", textfmt.FormatPlainValue(u.Username), u.TelegramID)
			continue
		}

		// 阶段 3：宽限期结束 -> 物理删除、硬销毁
		if u.IsSuspended && daysLeft <= -config.AppConfig.AccountGraceDays {
			err := abs.DefaultClient.DeleteUser(u.AbsUserID)
			absDeleteResult := "success"

			if err != nil && !abs.IsAbsNotFoundError(err) {
				log.Printf("⚠️ ABS 删除失败，已保留本地档案，避免遗孀账号: user=%s tg=%d abs=%s err=%s",
					textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err))
				continue
			}

			if err != nil {
				absDeleteResult = "already_missing"
			}

			if err := deleteLocalUserWithAudit(0, u.TelegramID, u.AbsUserID, "AUTO_DELETE_EXPIRED_USER", func(deleted model.User) string {
				return fmt.Sprintf("生命周期后台任务物理删除过期用户：username=%s tg=%d abs_user_id=%s grace_days=%d abs_delete=%s",
					textfmt.FormatPlainValue(deleted.Username), deleted.TelegramID, textfmt.FormatPlainValue(deleted.AbsUserID), config.AppConfig.AccountGraceDays, textfmt.FormatPlainValue(absDeleteResult))
			}); err != nil {
				log.Printf("自动注销本地档案或审计写入失败: user=%s tg=%d abs=%s err=%s",
					textfmt.FormatPlainValue(u.Username), u.TelegramID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(err))
				continue
			}
			msg := "🗑 **账户已注销**\n\n由于您的账号已超出最长保留期限，系统已自动将您的账户及所有听书进度**永久销毁**。\n\n期待与您的再次相遇！您可以随时重新注册。"
			telegram.ReplyText(bot, u.TelegramID, msg)
			log.Printf("🗑 已彻底销毁逾期不续费用户: %s (TG: %d)", textfmt.FormatPlainValue(u.Username), u.TelegramID)
		}
	}
	return nil
}

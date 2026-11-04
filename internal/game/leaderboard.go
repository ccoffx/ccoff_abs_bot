package game

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

// 解析统计接口的专用结构体
type AbsStatsData struct {
	RecentSessions []struct {
		Date          string  `json:"date"`
		TimeListening float64 `json:"timeListening"`
		MediaMetadata struct {
			Title string `json:"title"`
		} `json:"mediaMetadata"`
	} `json:"recentSessions"`
}

// 排序用的键值对结构
type kv struct {
	Key   string
	Value float64
}

// 新增：用于记录书籍详细数据的结构体
type BookStat struct {
	ListenCount int
	TotalTime   float64
}

// 新增：书籍排序专用的键值对结构
type bookKV struct {
	Key  string
	Stat *BookStat
}

var lastDailyRun, lastWeeklyRun, lastMonthlyRun string

var leaderboardLocation = time.FixedZone("CST", 8*3600)

// ⏱️ 启动纯原生定时调度器 (无第三方依赖)
func StartLeaderboardScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			now := <-ticker.C
			localNow := now.In(leaderboardLocation)
			// 设定每天早上 08:00 准时结算投递
			if localNow.Hour() == 8 && localNow.Minute() == 0 {
				dateStr := localNow.Format("2006-01-02")

				// 1. 日榜：每天都发
				if lastDailyRun != dateStr {
					lastDailyRun = dateStr
					go GenerateAndSendLeaderboard(bot, "daily", localNow)
				}

				// 2. 周榜：每周一发
				if localNow.Weekday() == time.Monday && lastWeeklyRun != dateStr {
					lastWeeklyRun = dateStr
					go GenerateAndSendLeaderboard(bot, "weekly", localNow)
				}

				// 3. 月榜：每月 1 号发
				if localNow.Day() == 1 && lastMonthlyRun != dateStr {
					lastMonthlyRun = dateStr
					go GenerateAndSendLeaderboard(bot, "monthly", localNow)
				}
			}
		}
	}()
	log.Println("✅ 榜单自动化引擎已启动，每日 08:00 准时播报。")
}

// 📊 核心聚合并生成榜单
func GenerateAndSendLeaderboard(bot *tgbotapi.BotAPI, timeframe string, now time.Time) {
	if config.AppConfig.NoticeGroupID == 0 {
		return // 未配置大群，终止发送
	}
	localNow := now.In(leaderboardLocation)

	var users []model.User
	// 提取所有绑定了 ABS 账号的真实用户
	if err := storage.DB.Where("abs_user_id != ''").Find(&users).Error; err != nil {
		log.Printf("⚠️ 榜单用户列表读取失败: timeframe=%s err=%s", textfmt.FormatPlainValue(timeframe), textfmt.FormatPlainError(err))
		return
	}
	if len(users) == 0 {
		return
	}

	// 确定时间过滤范围
	validDates := make(map[string]bool)
	var title string

	switch timeframe {
	case "daily":
		title = "🏆 **昨日净修为榜**"
		validDates[localNow.AddDate(0, 0, -1).Format("2006-01-02")] = true
	case "weekly":
		title = "🏆 **上周净修为榜**"
		for i := 1; i <= 7; i++ {
			validDates[localNow.AddDate(0, 0, -i).Format("2006-01-02")] = true
		}
	case "monthly":
		title = "🏆 **上月净修为榜**"
		lastMonth := localNow.AddDate(0, -1, 0).Format("2006-01")
		validDates[lastMonth] = true
	}
	monthPrefix := localNow.AddDate(0, -1, 0).Format("2006-01")

	userTimeMap := make(map[string]float64)
	// 🚨 升级1：将单一的时长 Map 升级为多维度的结构体 Map
	bookStatsMap := make(map[string]*BookStat)
	statsSuccess := 0
	statsRequestFailures := 0
	statsParseFailures := 0
	cultivationReadFailures := 0

	var wg sync.WaitGroup
	var dataMu sync.Mutex
	semaphore := make(chan struct{}, 5)

	// 遍历查询所有用户数据 (并发执行)
	for _, u := range users {
		wg.Add(1)
		go func(user model.User) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Personal cultivation includes the ordinary daily amount, proven
			// retreat bonus and spirit fire bonus. Keep book popularity on ABS
			// sessions, which have a different unit and purpose.
			cultivationHours, cultivationErr := leaderboardCultivationHours(user.TelegramID, timeframe, validDates, monthPrefix)
			if cultivationErr != nil {
				dataMu.Lock()
				cultivationReadFailures++
				dataMu.Unlock()
				return
			}
			if cultivationHours > 0 {
				dataMu.Lock()
				userTimeMap[user.Username] += cultivationHours
				dataMu.Unlock()
			}

			body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(user.AbsUserID), nil)
			if err != nil || code != 200 {
				dataMu.Lock()
				statsRequestFailures++
				dataMu.Unlock()
				return
			}

			var stats AbsStatsData
			if err := json.Unmarshal(body, &stats); err != nil {
				dataMu.Lock()
				statsParseFailures++
				dataMu.Unlock()
				return
			}

			// 局部字典也同步升级
			localBookMap := make(map[string]*BookStat)

			// 🚨 升级2：统计热门书籍（同时记录次数和时长）
			for _, session := range stats.RecentSessions {
				isValid := isLeaderboardDateValid(session.Date, timeframe, validDates, monthPrefix)
				bookName := session.MediaMetadata.Title
				if isValid && session.TimeListening > 0 && bookName != "" {
					if _, exists := localBookMap[bookName]; !exists {
						localBookMap[bookName] = &BookStat{}
					}
					// 每次有效会话记为 1 次收听
					localBookMap[bookName].ListenCount++
					localBookMap[bookName].TotalTime += session.TimeListening
				}
			}

			dataMu.Lock()
			statsSuccess++
			// 🚨 升级3：合并用户的局部书籍数据到全局
			for book, stat := range localBookMap {
				if _, exists := bookStatsMap[book]; !exists {
					bookStatsMap[book] = &BookStat{}
				}
				bookStatsMap[book].ListenCount += stat.ListenCount
				bookStatsMap[book].TotalTime += stat.TotalTime
			}
			dataMu.Unlock()

		}(u)
	}

	wg.Wait()
	if cultivationReadFailures > 0 {
		log.Printf("⚠️ 榜单净修为读取失败，跳过本期发送: timeframe=%s failed=%d", textfmt.FormatPlainValue(timeframe), cultivationReadFailures)
		return
	}
	if statsRequestFailures > 0 || statsParseFailures > 0 {
		log.Printf("⚠️ 榜单部分用户统计读取失败: timeframe=%s total=%d success=%d request_failed=%d parse_failed=%d",
			textfmt.FormatPlainValue(timeframe), len(users), statsSuccess, statsRequestFailures, statsParseFailures)
	}
	if statsSuccess == 0 && len(userTimeMap) == 0 {
		log.Printf("⚠️ 榜单统计全部失败，跳过本期发送: timeframe=%s total=%d", textfmt.FormatPlainValue(timeframe), len(users))
		return
	}

	finalMsg := title + "\n\n"

	// 1. 排序并截断用户榜
	userTop := sortMap(userTimeMap, 10)
	if len(userTop) > 0 {
		finalMsg += "✨ **净修为排行榜**\n"
		medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}
		for i, kv := range userTop {
			finalMsg += fmt.Sprintf("%s **%s** - `%.2f` 小时\n", medals[i], textfmt.EscapeMarkdown(kv.Key), kv.Value)
		}
	} else {
		finalMsg += "✨ **净修为排行榜**\n🫙 本周期内暂无净修为记录...\n"
	}

	finalMsg += "\n"

	// 🚨 升级4：全新的双维度独立排序逻辑
	var bookTop []bookKV
	for k, v := range bookStatsMap {
		bookTop = append(bookTop, bookKV{k, v})
	}

	sort.Slice(bookTop, func(i, j int) bool {
		// 主条件：按收听次数降序
		if bookTop[i].Stat.ListenCount != bookTop[j].Stat.ListenCount {
			return bookTop[i].Stat.ListenCount > bookTop[j].Stat.ListenCount
		}
		// 次条件：次数相同，按总时长降序
		return bookTop[i].Stat.TotalTime > bookTop[j].Stat.TotalTime
	})

	// 截取前 5 名
	if len(bookTop) > 5 {
		bookTop = bookTop[:5]
	}

	// 🚨 升级5：渲染格式调整，展示双维度
	if len(bookTop) > 0 {
		finalMsg += "📚 **万界风云书单 (热门追更)**\n"
		bookMedals := []string{"🔥", "🌟", "✨", "📖", "📖"}
		for i, kv := range bookTop {
			finalMsg += fmt.Sprintf("%s 《%s》 - 收听 `%d` 次 | 累计 `%.1f` 小时\n", bookMedals[i], textfmt.EscapeMarkdown(kv.Key), kv.Stat.ListenCount, kv.Stat.TotalTime/3600.0)
		}
	} else {
		finalMsg += "📚 **万界风云书单**\n🫙 暂无热门书籍上榜...\n"
	}

	if config.AppConfig.NoticeGroupID != 0 {
		sendAndManageLeaderboardPin(bot, config.AppConfig.NoticeGroupID, finalMsg, timeframe)
	}
}

func normalizeLeaderboardDateKey(date string) string {
	date = strings.TrimSpace(date)
	if len(date) >= len("2006-01-02") {
		return date[:len("2006-01-02")]
	}
	return date
}

func isLeaderboardDateValid(date string, timeframe string, validDates map[string]bool, monthPrefix string) bool {
	dayKey := normalizeLeaderboardDateKey(date)
	if dayKey == "" {
		return false
	}
	if timeframe == "monthly" {
		return strings.HasPrefix(dayKey, monthPrefix)
	}
	return validDates[dayKey]
}

func leaderboardCultivationHours(userID int64, timeframe string, validDates map[string]bool, monthPrefix string) (float64, error) {
	var stats []model.DailyListeningStat
	query := storage.DB.Where("user_id = ?", userID)
	if timeframe == "monthly" {
		query = query.Where("day_key LIKE ?", monthPrefix+"-%")
	} else {
		days := make([]string, 0, len(validDates))
		for day := range validDates {
			days = append(days, day)
		}
		query = query.Where("day_key IN ?", days)
	}
	if err := query.Find(&stats).Error; err != nil {
		return 0, err
	}
	if len(stats) == 0 {
		return 0, nil
	}
	var retreats []model.SectCaveRetreat
	if err := storage.DB.Where("user_id = ?", userID).Find(&retreats).Error; err != nil {
		return 0, err
	}
	for _, retreat := range retreats {
		if _, err := decodeSectRetreatEvidence(retreat); err != nil {
			return 0, err
		}
	}
	total := 0.0
	for _, stat := range stats {
		if isLeaderboardDateValid(stat.DayKey, timeframe, validDates, monthPrefix) {
			total += stat.EffectiveHours + dailySectCaveRetreatBonusHours(stat, retreats) + stat.SpiritFireHours
		}
	}
	return total, nil
}

// 辅助函数：字典按 Value 降序排序并截取 Top N
func sortMap(m map[string]float64, topN int) []kv {
	var ss []kv
	for k, v := range m {
		ss = append(ss, kv{k, v})
	}
	sort.Slice(ss, func(i, j int) bool {
		return ss[i].Value > ss[j].Value
	})
	if len(ss) > topN {
		ss = ss[:topN]
	}
	return ss
}

// =========================================================================
// 🏆 榜单专属：全自动动态置顶与新老替换引擎 (永久留存版)
// =========================================================================
func sendAndManageLeaderboardPin(bot *tgbotapi.BotAPI, chatID int64, text string, timeframe string) {
	if bot == nil || chatID == 0 {
		return
	}
	if telegram.EnqueueTelegramAsync(telegram.TelegramAsyncJob{
		Kind:        "leaderboard_pin",
		DedupeKey:   "leaderboard_pin:" + timeframe,
		Priority:    telegram.TelegramAsyncPriorityNormal,
		MaxAttempts: 1,
		Send: func() error {
			return sendAndManageLeaderboardPinSync(bot, chatID, text, timeframe)
		},
	}) {
		return
	}

	log.Printf("⚠️ [%s] 榜单发送异步入队失败，改为同步发送", textfmt.FormatPlainValue(timeframe))
	if err := sendAndManageLeaderboardPinSync(bot, chatID, text, timeframe); err != nil {
		log.Printf("❌ 【天机阁异常】[%s] 榜单同步发送失败: %s", textfmt.FormatPlainValue(timeframe), textfmt.FormatTelegramSendError(err))
	}
}

func sendAndManageLeaderboardPinSync(bot *tgbotapi.BotAPI, chatID int64, text string, timeframe string) error {
	if storage.DB == nil {
		return fmt.Errorf("LEADERBOARD_DB_UNAVAILABLE")
	}
	configKey := "pinned_leaderboard_" + timeframe

	storedID, err := storage.GetSystemConfigStringChecked(configKey)
	if err != nil {
		return fmt.Errorf("read leaderboard pin state: %w", err)
	}
	oldMsgID := 0
	if storedID = strings.TrimSpace(storedID); storedID != "" {
		oldMsgID, err = strconv.Atoi(storedID)
		if err != nil || oldMsgID < 0 {
			return fmt.Errorf("invalid leaderboard pin state: key=%s value=%s", configKey, textfmt.FormatPlainValue(storedID))
		}
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	sentMsg, err := telegram.SendNoAutoDelete(bot, msg)
	if err != nil {
		log.Printf("❌ 【天机阁异常】新一期 [%s] 榜单发送失败: %s", textfmt.FormatPlainValue(timeframe), textfmt.FormatTelegramSendError(err))
		return err
	}

	// Keep the previous pin until its replacement is pinned and persisted.
	pinCfg := tgbotapi.PinChatMessageConfig{
		ChatID:              chatID,
		MessageID:           sentMsg.MessageID,
		DisableNotification: true, // 🚨 关键：开启静默置顶
	}

	if _, pinErr := bot.Request(pinCfg); pinErr != nil {
		log.Printf("⚠️ 【天道受限】榜单置顶失败！请检查 Bot 是否在群内拥有「置顶消息」的管理员特权: %s", textfmt.FormatTelegramSendError(pinErr))
		return pinErr
	}

	newValue := strconv.Itoa(sentMsg.MessageID)
	if err := storage.SetSystemConfigStringChecked(configKey, newValue); err != nil {
		log.Printf("⚠️ 榜单置顶状态写入失败: key=%s value=%s err=%s", textfmt.FormatPlainValue(configKey), textfmt.FormatPlainValue(newValue), textfmt.FormatPlainError(err))
		return err
	}
	if oldMsgID > 0 && oldMsgID != sentMsg.MessageID {
		unpinCfg := tgbotapi.UnpinChatMessageConfig{ChatID: chatID, MessageID: oldMsgID}
		if _, err := bot.Request(unpinCfg); err != nil && !telegram.IsTerminalTelegramUnpinError(err) {
			log.Printf("⚠️ [%s] 取消旧榜单置顶失败: chat=%d message_id=%d err=%s", textfmt.FormatPlainValue(timeframe), chatID, oldMsgID, textfmt.FormatTelegramSendError(err))
			return err
		}
	}

	log.Printf("✅ 【天道榜单巡查】[%s] 榜单交接顺利完成，新置顶 ID: %s", textfmt.FormatPlainValue(timeframe), textfmt.FormatPlainValue(newValue))
	return nil
}

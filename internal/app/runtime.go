package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/announcements"
	"ccoff-abs-bot/internal/books"
	"ccoff-abs-bot/internal/buildinfo"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/joinverify"
	"ccoff-abs-bot/internal/market"
	"ccoff-abs-bot/internal/metrics"
	"ccoff-abs-bot/internal/operations"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var joinVerification *joinverify.Service

type telegramMessageJob struct {
	msg        *tgbotapi.Message
	enqueuedAt time.Time
}

type telegramHTTPClient struct {
	longPoll *http.Client
	callback *http.Client
	send     *http.Client
	upload   *http.Client
}

func newTelegramHTTPClient() *telegramHTTPClient {
	return &telegramHTTPClient{
		longPoll: &http.Client{Timeout: telegram.TelegramHTTPClientTimeout},
		callback: &http.Client{Timeout: telegram.TelegramCallbackHTTPTimeout},
		send:     &http.Client{Timeout: telegram.TelegramSendHTTPTimeout},
		upload:   &http.Client{Timeout: telegram.TelegramHTTPClientTimeout},
	}
}

func (c *telegramHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if err := guardTestTelegramRequest(req); err != nil {
		return nil, err
	}
	if c == nil {
		return http.DefaultClient.Do(req)
	}

	endpoint := telegramEndpointFromRequest(req)
	start := time.Now()
	client := c.clientForEndpoint(endpoint)
	resp, err := client.Do(req)
	duration := time.Since(start)
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	metrics.RecordTelegramAPICall(endpoint, duration, err, statusCode)
	if duration > telegram.TelegramSendHTTPTimeout && endpoint != "getUpdates" {
		log.Printf("⚠️ Telegram API 调用耗时过长: endpoint=%s duration=%s err=%s", textfmt.FormatPlainValue(endpoint), duration, textfmt.FormatTelegramSendError(err))
	}
	return resp, err
}

func (c *telegramHTTPClient) clientForEndpoint(endpoint string) *http.Client {
	switch endpoint {
	case "getUpdates":
		return c.longPoll
	case "answerCallbackQuery":
		return c.callback
	case "sendDocument", "sendPhoto", "sendVideo", "sendAudio", "sendVoice", "sendAnimation":
		return c.upload
	default:
		return c.send
	}
}

func telegramEndpointFromRequest(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "unknown"
	}
	path := strings.Trim(req.URL.EscapedPath(), "/")
	if path == "" {
		return "unknown"
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if last == "" {
		return "unknown"
	}
	return last
}

func Run() {
	run(config.LoadConfig)
}

// RunTest is the native, foreground entry point for local source development.
func RunTest() {
	run(func() {
		if err := config.LoadTestConfig(".env.test"); err != nil {
			log.Fatal(err)
		}
	})
}

func run(loadConfig func()) {
	time.Local = time.FixedZone("CST", 8*3600)
	logRuntimeIdentity()

	loadConfig()
	if err := operations.WriteHealth(time.Time{}); err != nil {
		log.Fatal(err)
	}
	if err := prepareTestDirectory(); err != nil {
		log.Fatal(err)
	}
	InitDB()
	if err := books.RecoverBookRequestAnnouncementDeliveriesOnStartup(); err != nil {
		log.Fatalf("book announcement delivery recovery failed; refusing startup: %s", textfmt.FormatPlainError(err))
	}

	abs.DefaultClient = abs.NewAbsClient()
	game.StartABSCrossDaySessionBackfill()
	books.StartBookLibraryEventCollector()

	bot, err := tgbotapi.NewBotAPIWithClient(
		config.AppConfig.TgToken,
		tgbotapi.APIEndpoint,
		newTelegramHTTPClient(),
	)
	if err != nil {
		log.Panicf("🤖 Bot 启动失败: %s", textfmt.FormatTelegramSendError(err))
	}

	if config.AppConfig.JoinVerifyEnabled {
		admins := make([]int64, 0, len(config.AppConfig.AdminIDs))
		for id := range config.AppConfig.AdminIDs {
			admins = append(admins, id)
		}
		joinVerification = joinverify.New(storage.DB, bot, config.AppConfig.NoticeGroupID, config.AppConfig.JoinVerifyChannelID, bot.Self.ID, config.AppConfig.JoinVerifyChannelURL, admins)
		if err := joinVerification.Validate(); err != nil {
			log.Fatalf("入群验证配置或权限检查失败: %s", textfmt.FormatTelegramSendError(err))
		}
	}

	if err := announcements.Validate(bot); err != nil {
		log.Fatalf("公告频道权限检查失败: %s", textfmt.FormatTelegramSendError(err))
	}
	if err := announcements.Recover(storage.DB); err != nil {
		log.Fatalf("公告投递恢复失败: %s", textfmt.FormatPlainError(err))
	}
	bot.Debug = false // 建议线上环境关闭上帝视角，避免日志爆炸
	if config.AppConfig.IsTest() {
		if !strings.EqualFold(bot.Self.UserName, config.AppConfig.TestBotUsername) {
			log.Fatal("test Bot identity mismatch: check TEST_BOT_USERNAME and TELEGRAM_BOT_TOKEN")
		}
		log.Printf("🧪 TEST ONLY: bot=@%s version=%s commit=%s ABS=mock", bot.Self.UserName, buildinfo.Version, buildinfo.Commit)
		for id := range config.AppConfig.AdminIDs {
			telegram.SendPlainText(bot, id, "🧪 测试环境 · "+buildinfo.Version+" · "+buildinfo.Commit+"\nABS 使用本地模拟数据。发送 /test 查看测试工具。")
		}
	}
	log.Printf("✅ Bot 已成功启动，当前运行账号: %s", textfmt.FormatPlainValue(bot.Self.UserName))

	if info, err := bot.GetWebhookInfo(); err != nil {
		log.Printf("⚠️ 查询 Telegram webhook 状态失败，将继续尝试清理: %s", textfmt.FormatTelegramSendError(err))
	} else if info.URL != "" || info.PendingUpdateCount > 0 {
		log.Printf("ℹ️ Telegram webhook 状态: url_configured=%t pending_updates=%d", info.URL != "", info.PendingUpdateCount)
	}

	if _, err := bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: telegram.TelegramWebhookDropOnStartup && joinVerification == nil}); err != nil {
		log.Fatalf("❌ 清理 Telegram webhook 失败，长轮询无法可靠接收消息: %s", textfmt.FormatTelegramSendError(err))
	}
	log.Printf("✅ Telegram webhook 已清理，开始使用长轮询接收消息: drop_pending=%t", telegram.TelegramWebhookDropOnStartup && joinVerification == nil)
	writeStartupHealth("webhook_cleared", bot.Self.UserName)
	notifyAdminsOnStartup(bot)

	telegram.SweeperOnce.Do(func() {
		telegram.StartMessageSweeper(bot)
	})

	telegram.StartTelegramDispatcher(bot)
	StartChannelAnnouncementDispatcher(bot)
	if joinVerification != nil {
		go joinVerification.Run(context.Background())
	}

	// 正式启动后台核心任务（生命周期巡检与加密备份）
	startBackgroundJobs(bot)

	// 🚨 新增核心：启动全自动榜单定时投递引擎 (包含日榜、周榜、月榜及热门书单)
	game.StartLeaderboardScheduler(bot)
	game.StartWorldBossScheduler(bot)
	game.StartSectSecretRealmScheduler(bot)
	activity.StartLotteryScheduler(bot)
	game.StartSectLotteryScheduler(bot)
	game.StartSectHornDispatcher(bot)
	StartCompensationDispatcher(bot)
	market.StartMarketplaceExpiryScheduler(bot)
	game.StartGardenMaturityNotifier(bot)
	activity.StartTreasureHuntScheduler(bot)

	// 🚨 修复隐患 2：实现真正的高并发 Worker 工作池
	jobs := make(chan telegramMessageJob, config.AppConfig.QueueCapacity)
	metrics.RecordMessageQueueState(0, cap(jobs))
	for w := 1; w <= config.AppConfig.WorkerCount; w++ {
		go botWorker(bot, jobs)
	}

	pollTelegramUpdates(bot, jobs)
}

func pollTelegramUpdates(bot *tgbotapi.BotAPI, jobs chan<- telegramMessageJob) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = telegram.TelegramLongPollTimeout
	u.AllowedUpdates = []string{"message", "callback_query"}
	if joinVerification != nil {
		u.AllowedUpdates = append(u.AllowedUpdates, "chat_member")
	}

	log.Printf("✅ Telegram 长轮询已启动: timeout=%ds allowed_updates=%s", telegram.TelegramLongPollTimeout, textfmt.FormatPlainValue(strings.Join(u.AllowedUpdates, ",")))
	writeStartupHealth("polling_started", bot.Self.UserName)

	for {
		updates, err := bot.GetUpdates(u)
		if err != nil {
			var apiErr *tgbotapi.Error
			if errors.As(err, &apiErr) && apiErr.Code == telegram.TelegramPollConflictError {
				log.Fatalf("❌ Telegram getUpdates 冲突：同一个 Bot Token 正被其他进程或部署实例接收更新，请先停止旧实例。err=%s", textfmt.FormatTelegramSendError(err))
			}

			log.Printf("⚠️ Telegram getUpdates 失败，%s 后重试: %s", telegram.TelegramPollRetryDelay, textfmt.FormatTelegramSendError(err))
			time.Sleep(telegram.TelegramPollRetryDelay)
			continue
		}

		if err := operations.WriteHealth(time.Now()); err != nil {
			log.Printf("write runtime health failed: %v", err)
		}
		if len(updates) == 0 {
			continue
		}

		firstID := updates[0].UpdateID
		lastID := updates[len(updates)-1].UpdateID
		metrics.RecordMessageQueueState(len(jobs), cap(jobs))
		log.Printf("📥 收到 Telegram updates: count=%d first=%d last=%d queue=%d/%d", len(updates), firstID, lastID, len(jobs), cap(jobs))

		for _, update := range updates {
			if update.UpdateID >= u.Offset {
				u.Offset = update.UpdateID + 1
			}
			dispatchTelegramUpdate(bot, jobs, update)
		}
	}
}

func dispatchTelegramUpdate(bot *tgbotapi.BotAPI, jobs chan<- telegramMessageJob, update tgbotapi.Update) {
	if !testUpdateAllowed(update) {
		return
	}
	if joinVerification != nil && update.ChatMember != nil {
		for {
			if err := joinVerification.Enqueue(update); err == nil {
				break
			} else {
				log.Printf("入群验证事件持久化失败，将重试: %s", textfmt.FormatPlainError(err))
			}
			time.Sleep(telegram.TelegramPollRetryDelay)
		}
		return
	}
	if update.Message != nil {
		logIncomingMessage(update.Message)
		if handlePrivateStartFastPath(bot, update.Message) {
			return
		}
		select {
		case jobs <- telegramMessageJob{msg: update.Message, enqueuedAt: time.Now()}:
			metrics.RecordMessageQueueState(len(jobs), cap(jobs))
		default:
			metrics.RecordMessageQueueDropped()
			fromID := int64(0)
			if update.Message.From != nil {
				fromID = update.Message.From.ID
			}
			log.Printf("⚠️ 消息队列已满，丢弃消息: user=%d chat=%d", fromID, update.Message.Chat.ID)
			telegram.ReplyText(bot, update.Message.Chat.ID, "⚠️ 当前系统繁忙，请稍后再试。")
		}
	}

	if update.CallbackQuery != nil {
		go func(cb *tgbotapi.CallbackQuery) {
			if cb.From == nil {
				return
			}
			defer func() {
				if r := recover(); r != nil {
					log.Printf("⚠️ 处理 callback 时发生 panic，已恢复: user=%d data=%s panic=%s\nstack:\n%s",
						cb.From.ID, textfmt.FormatPlainValue(cb.Data), textfmt.FormatPlainValue(r), string(debug.Stack()))
				}
			}()
			defer logCallbackHandlingDuration(cb, time.Now())
			books.StartDelayedCallbackAck(bot, cb.ID)
			unlock := telegram.LockUser(cb.From.ID)
			defer unlock()
			if strings.HasPrefix(cb.Data, joinverify.CallbackPrefix) {
				if joinVerification == nil {
					telegram.AnswerCallback(bot, cb.ID, "入群验证当前未启用。")
				} else {
					telegram.AnswerCallback(bot, cb.ID, joinVerification.Callback(cb))
				}
				return
			}
			if joinVerification != nil {
				if ok, hint := joinVerification.Check(cb.From.ID); !ok {
					telegram.AnswerCallback(bot, cb.ID, hint)
					return
				}
			}
			// 官方群成员门禁与消息路径（interactive.go）保持一致：
			// 防止已退群用户继续通过历史面板上的按钮触发功能。
			if config.AppConfig != nil && config.AppConfig.NoticeGroupID != 0 && !telegram.IsUserInGroup(bot, cb.From.ID, config.AppConfig.NoticeGroupID) {
				ack := tgbotapi.NewCallback(cb.ID, "⚠️ 访问受限：请先加入官方交流群再使用本功能。")
				ack.ShowAlert = true
				if _, err := bot.Request(ack); err != nil && !telegram.IsOldTelegramCallbackError(err) {
					log.Printf("⚠️ 拒绝非群成员 callback 应答失败: user=%d err=%s", cb.From.ID, textfmt.FormatTelegramSendError(err))
				}
				return
			}
			if activity.HandleLotteryEntryCallback(bot, cb) {
				return
			}
			if game.HandleBreakthroughGuardCallback(bot, cb) {
				return
			}
			if activity.HandleTreasureHuntCallback(bot, cb) {
				return
			}
			if game.HandleGardenCallback(bot, cb) {
				return
			}
			if game.HandleSectMemberPageCallback(bot, cb) {
				return
			}
			if account.HandleWealthLeaderboardCallback(bot, cb) {
				return
			}
			if handleMenuCallback(bot, cb) {
				return
			}
			if handleAdminMenuCallback(bot, cb) {
				return
			}
			books.HandleBookRequestCallback(bot, cb)
		}(update.CallbackQuery)
	}
}

func handlePrivateStartFastPath(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) bool {
	if msg == nil || msg.Chat == nil || msg.From == nil || !msg.Chat.IsPrivate() {
		return false
	}
	if !telegram.IsTelegramCommandText(msg.Text, "/start") {
		return false
	}
	if activity.IsLotteryStart(msg.Text) {
		return false
	}
	if _, ok := account.ParseReferralStartPayload(msg.Text); ok {
		return false
	}

	userID := msg.From.ID
	chatID := msg.Chat.ID

	// 快速路径必须与 worker 持有同一把用户锁后才能清理会话：
	// 否则 worker 正在处理的上一条消息可能在 clearSession 后把旧会话重新写回（会话复活），
	// 下一条用户消息会被复活的中途步骤拦截。
	// 锁被占用说明该用户有消息在途——此时回退到正常队列，由 worker 持锁按序处理 /start。
	unlock, locked := telegram.TryLockUser(userID)
	if !locked {
		return false
	}
	defer unlock()

	telegram.ClearSession(userID)
	log.Printf("⚡ 私聊 /start 快速响应: user=%d chat=%d message_id=%d", userID, chatID, msg.MessageID)

	if err := telegram.SendUserMainMenu(bot, chatID, "👋 欢迎使用【昔风听书助手】："); err != nil {
		msg := tgbotapi.NewMessage(chatID, "欢迎使用【昔风听书助手】。主菜单暂时发送失败，请稍后再试或联系管理员查看 Bot 日志。")
		if _, fallbackErr := telegram.SendNoAutoDelete(bot, msg); fallbackErr != nil {
			log.Printf("发送 /start 纯文本兜底失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(fallbackErr))
		}
	}

	return true
}

func notifyAdminsOnStartup(bot *tgbotapi.BotAPI) {
	if config.AppConfig == nil || !config.AppConfig.StartupNotifyAdmins {
		return
	}

	text := "【昔风听书助手启动自检】\n" +
		"进程已完成 Telegram 登录和 webhook 清理，即将进入长轮询。\n" +
		"如果你能收到这条消息，说明当前运行实例的 Token 和 Telegram 发送链路可用。"

	for adminID := range config.AppConfig.AdminIDs {
		msg := tgbotapi.NewMessage(adminID, text)
		if _, err := telegram.SendNoAutoDelete(bot, msg); err != nil {
			log.Printf("发送启动自检通知失败: admin=%d err=%s", adminID, textfmt.FormatTelegramSendError(err))
			continue
		}
		log.Printf("✅ 已发送启动自检通知: admin=%d", adminID)
	}
}

func writeStartupHealth(stage string, botUsername string) {
	if config.AppConfig == nil {
		return
	}
	dir := storage.SqliteDatabaseDir(config.AppConfig.DbURL)
	if dir == "" {
		return
	}
	content := "stage=" + stage + "\n" +
		"bot=" + textfmt.FormatPlainValue(botUsername) + "\n" +
		"at=" + time.Now().Format(time.RFC3339) + "\n"

	path := filepath.Join(dir, "bot_startup_health.txt")
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		log.Printf("⚠️ 写入启动健康标记失败: path=%s err=%s", textfmt.FormatPlainValue(path), textfmt.FormatPlainError(err))
	}
}

func logRuntimeIdentity() {
	exePath, exeErr := os.Executable()
	cwd, cwdErr := os.Getwd()

	exeModTime := ""
	if exeErr == nil {
		if stat, err := os.Stat(exePath); err == nil {
			exeModTime = stat.ModTime().Format(time.RFC3339)
		}
	}

	if exeErr != nil {
		exePath = "unknown: " + textfmt.FormatPlainError(exeErr)
	}
	if cwdErr != nil {
		cwd = "unknown: " + textfmt.FormatPlainError(cwdErr)
	}

	log.Printf("ℹ️ 运行身份: exe=%s exe_mtime=%s cwd=%s", textfmt.FormatPlainValue(exePath), textfmt.FormatPlainValue(exeModTime), textfmt.FormatPlainValue(cwd))
}

func logIncomingMessage(msg *tgbotapi.Message) {
	if msg == nil || msg.Chat == nil {
		return
	}

	fromID := int64(0)
	if msg.From != nil {
		fromID = msg.From.ID
	}

	text := ""
	if fields := strings.Fields(msg.Text); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		text = strings.ToLower(fields[0])
	}

	if text != "" {
		log.Printf("📨 Telegram 命令入队: command=%s user=%d chat=%d message_id=%d", textfmt.FormatPlainValue(text), fromID, msg.Chat.ID, msg.MessageID)
	}
}

func logMessageHandlingDuration(msg *tgbotapi.Message, startedAt time.Time) {
	if msg == nil || msg.Chat == nil || startedAt.IsZero() {
		return
	}
	duration := time.Since(startedAt)
	if duration <= telegram.MessageHandleSlowThreshold {
		return
	}
	metrics.RecordSlowMessageHandle()
	fromID := int64(0)
	if msg.From != nil {
		fromID = msg.From.ID
	}
	command := ""
	if fields := strings.Fields(msg.Text); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		command = strings.ToLower(fields[0])
	}
	log.Printf("⚠️ Telegram 消息处理耗时过长: user=%d chat=%d message_id=%d command=%s duration=%s", fromID, msg.Chat.ID, msg.MessageID, textfmt.FormatPlainValue(command), duration)
}

func logCallbackHandlingDuration(cb *tgbotapi.CallbackQuery, startedAt time.Time) {
	if cb == nil || cb.From == nil || startedAt.IsZero() {
		return
	}
	duration := time.Since(startedAt)
	if duration <= telegram.CallbackHandleSlowThreshold {
		return
	}
	metrics.RecordSlowCallbackHandle()
	chatID := int64(0)
	messageID := 0
	if cb.Message != nil && cb.Message.Chat != nil {
		chatID = cb.Message.Chat.ID
		messageID = cb.Message.MessageID
	}
	log.Printf("⚠️ Telegram callback 处理耗时过长: user=%d chat=%d message_id=%d data=%s duration=%s", cb.From.ID, chatID, messageID, textfmt.FormatPlainValue(cb.Data), duration)
}

// 高并发消费者
func botWorker(bot *tgbotapi.BotAPI, jobs <-chan telegramMessageJob) {
	for job := range jobs {
		func() {
			startedAt := time.Now()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("⚠️ botWorker 处理消息时发生 panic，已恢复: panic=%s\nstack:\n%s", textfmt.FormatPlainValue(r), string(debug.Stack()))
				}
			}()

			msg := job.msg
			if msg == nil {
				return
			}
			if queueWait := time.Since(job.enqueuedAt); !job.enqueuedAt.IsZero() && queueWait > telegram.MessageQueueSlowWaitThreshold {
				fromID := int64(0)
				if msg.From != nil {
					fromID = msg.From.ID
				}
				log.Printf("⚠️ Telegram 消息队列等待过久: user=%d chat=%d message_id=%d wait=%s queue=%d/%d", fromID, msg.Chat.ID, msg.MessageID, queueWait, len(jobs), cap(jobs))
			}
			metrics.RecordMessageQueueState(len(jobs), cap(jobs))
			defer logMessageHandlingDuration(msg, startedAt)

			// 没有 From 的消息直接忽略，避免 handleInteractiveMessage 空指针 panic
			if msg.From == nil {
				log.Printf("⚠️ 忽略无 From 的消息: chat=%d message_id=%d", msg.Chat.ID, msg.MessageID)
				return
			}
			lockWaitStart := time.Now()
			unlock := telegram.LockUser(msg.From.ID)
			if wait := time.Since(lockWaitStart); wait > telegram.UserLockSlowWaitThreshold {
				metrics.RecordSlowUserLockWait()
				log.Printf("⚠️ 用户锁等待过久: user=%d chat=%d message_id=%d wait=%s", msg.From.ID, msg.Chat.ID, msg.MessageID, wait)
			}
			defer unlock()
			if telegram.IsTelegramCommandText(msg.Text, "/start") {
				log.Printf("ℹ️ worker 处理 /start: user=%d chat=%d message_id=%d", msg.From.ID, msg.Chat.ID, msg.MessageID)
			}
			if handleTestCommand(bot, msg) {
				return
			}
			handleInteractiveMessage(bot, msg)
		}()
	}
}

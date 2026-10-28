package telegram

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/backup"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func NotifyFusionPoolBurst(bot *tgbotapi.BotAPI, fallbackChatID int64, reason string) {
	if bot == nil {
		return
	}

	targetChatID := config.AppConfig.NoticeGroupID
	if targetChatID == 0 {
		targetChatID = fallbackChatID
	}
	if targetChatID == 0 {
		return
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "众道友引动天地异象"
	}

	// 当前 reason 都是代码内固定文案；这里额外转义，防止未来误传用户输入导致 Markdown 格式注入。
	reason = textfmt.EscapeMarkdown(reason)

	announce := fmt.Sprintf(
		"🌈 **【天降甘霖·仙气化雨】** 🌈\n"+
			"%s，天道奖池已蓄满并自动爆开！\n\n"+
			"💰 降下红包: `300` 积分\n"+
			"📦 福泽份数: `30` 份\n\n"+
			"👇 众修士快回复关键字 【`沾仙气`】 汲取天地造化！",
		reason,
	)

	go SendGroupAutoDeleteMessage(bot, targetChatID, announce)
}

func SendEncryptedBackupToTelegram(bot *tgbotapi.BotAPI, source string) (int, error) {
	if config.AppConfig == nil || config.AppConfig.BackupGroupID == 0 {
		return 0, fmt.Errorf("未配置 BACKUP_GROUP_ID，无法发送备份")
	}

	backupPath, cleanup, err := backup.CreateEncryptedDBBackup()
	if err != nil {
		return 0, fmt.Errorf("创建加密数据库备份失败: %w", err)
	}
	defer cleanup()

	sourceText := "手动触发"
	switch source {
	case "daily_auto":
		sourceText = "每日自动"
	case "manual":
		sourceText = "手动触发"
	case "retry_auto":
		sourceText = "自动重试"
	case "startup_catchup":
		sourceText = "启动补跑"
	}

	info, err := os.Stat(backupPath)
	if err != nil {
		return 0, fmt.Errorf("读取加密备份文件信息失败: %w", err)
	}

	partPaths, cleanupParts, err := backup.SplitBackupFileParts(backupPath, backup.TelegramBotUploadMaxBytes)
	if err != nil {
		return 0, fmt.Errorf("准备备份分片失败: %w", err)
	}
	defer cleanupParts()

	now := storage.DailyOperationsLocalTime(time.Now())
	sizeMB := float64(info.Size()) / (1024 * 1024)
	var lastMessageID int
	totalParts := len(partPaths)
	for i, partPath := range partPaths {
		caption := fmt.Sprintf(
			"📦 **%s加密数据库备份**\n⏰ 时间: `%s`\n📏 体积: `%.2f` MB\n🔐 文件已使用 AES-GCM 加密（v3 压缩格式）。\n⚠️ 请妥善保管 BACKUP_ENCRYPT_KEY，丢失后无法解密。",
			sourceText,
			now.Format("2006-01-02 15:04:05"),
			sizeMB,
		)
		if totalParts > 1 {
			caption += fmt.Sprintf("\n📎 分片：`%d/%d`（请按顺序保存后合并再解密）", i+1, totalParts)
		}

		doc := tgbotapi.NewDocument(config.AppConfig.BackupGroupID, tgbotapi.FilePath(partPath))
		doc.Caption = caption
		doc.ParseMode = "Markdown"

		sentMsg, sendErr := SendNoAutoDelete(bot, doc)
		if sendErr != nil {
			return 0, fmt.Errorf("发送 Telegram 备份文件失败: part=%d/%d err=%w", i+1, totalParts, sendErr)
		}
		lastMessageID = sentMsg.MessageID
	}

	if lastMessageID > 0 {
		pinLatestBackupMessage(bot, lastMessageID)
	}

	return lastMessageID, nil
}

func pinLatestBackupMessage(bot *tgbotapi.BotAPI, messageID int) {
	if bot == nil || config.AppConfig == nil || config.AppConfig.BackupGroupID == 0 || messageID == 0 {
		return
	}

	rawOldMsgID, err := storage.GetSystemConfigStringChecked(storage.BackupLastPinnedMessageIDKey)
	if err != nil {
		recordBackupPinStateReadFailure(bot, storage.BackupLastPinnedMessageIDKey, err)
		return
	}

	oldMsgID := 0
	rawOldMsgID = strings.TrimSpace(rawOldMsgID)
	if rawOldMsgID != "" {
		parsedOldMsgID, parseErr := strconv.Atoi(rawOldMsgID)
		if parseErr != nil {
			recordBackupPinStateReadFailure(bot, storage.BackupLastPinnedMessageIDKey, fmt.Errorf("invalid backup pinned message id %s: %w", textfmt.FormatPlainValue(rawOldMsgID), parseErr))
			return
		}
		oldMsgID = parsedOldMsgID
	}
	if oldMsgID > 0 && oldMsgID != messageID {
		unpinCfg := tgbotapi.UnpinChatMessageConfig{
			ChatID:    config.AppConfig.BackupGroupID,
			MessageID: oldMsgID,
		}
		if _, err := bot.Request(unpinCfg); err != nil && !IsTerminalTelegramUnpinError(err) {
			storage.SetSystemConfigError(storage.BackupLastPinErrorKey, err)
			log.Printf("⚠️ 备份消息发送成功，但取消旧备份置顶失败: old_message_id=%d new_message_id=%d err=%s",
				oldMsgID,
				messageID,
				textfmt.FormatTelegramSendError(err),
			)
		}
	}

	pinCfg := tgbotapi.PinChatMessageConfig{
		ChatID:              config.AppConfig.BackupGroupID,
		MessageID:           messageID,
		DisableNotification: true,
	}

	if _, err := bot.Request(pinCfg); err != nil {
		storage.SetSystemConfigError(storage.BackupLastPinErrorKey, err)
		log.Printf("⚠️ 备份消息发送成功，但置顶失败。请检查 Bot 是否拥有置顶消息权限: message_id=%d err=%s", messageID, textfmt.FormatTelegramSendError(err))
		NotifySuperAdminsPlain(bot, fmt.Sprintf(
			"⚠️ 备份文件已发送成功，但置顶失败。\n\nMessageID: %d\n错误: %s\n\n请检查 Bot 是否是备份群管理员，并拥有置顶消息权限。",
			messageID,
			textfmt.FormatPlainError(err),
		))
		return
	}

	if err := storage.SetSystemConfigStringChecked(storage.BackupLastPinnedMessageIDKey, strconv.Itoa(messageID)); err != nil {
		recordBackupPinStateWriteFailure(bot, storage.BackupLastPinnedMessageIDKey, err)
		return
	}
	if err := storage.SetSystemConfigStringChecked(storage.BackupLastPinErrorKey, ""); err != nil {
		recordBackupPinStateWriteFailure(bot, storage.BackupLastPinErrorKey, err)
		return
	}
}

func recordBackupPinStateReadFailure(bot *tgbotapi.BotAPI, key string, err error) {
	storage.SetSystemConfigError(storage.BackupLastPinErrorKey, err)
	log.Printf("备份置顶状态读取失败，已跳过本次置顶交接: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	NotifySuperAdminsPlain(bot, fmt.Sprintf("备份文件已发送成功，但置顶状态读取失败，已跳过本次置顶交接。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func recordBackupPinStateWriteFailure(bot *tgbotapi.BotAPI, key string, err error) {
	if key != storage.BackupLastPinErrorKey {
		storage.SetSystemConfigError(storage.BackupLastPinErrorKey, err)
	}
	log.Printf("备份置顶状态写入失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	NotifySuperAdminsPlain(bot, fmt.Sprintf("备份消息置顶已执行，但置顶状态写入失败，请人工核查。\n\n配置：%s\n错误：%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err)))
}

func NotifySuperAdminsPlain(bot *tgbotapi.BotAPI, text string) {
	if bot == nil || config.AppConfig == nil {
		return
	}

	for adminID := range config.AppConfig.AdminIDs {
		msg := tgbotapi.NewMessage(adminID, text)
		if !EnqueueNoAutoDelete(bot, msg, "super_admin_notice", telegramAsyncPriorityHigh, "") {
			log.Printf("⚠️ 通知管理员入队失败: admin=%d", adminID)
		}
	}
}

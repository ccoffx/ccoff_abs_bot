package books

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func formatBookRequestAdminText(req model.BookRequest) string {
	lastActionText := "暂无"
	if req.LastActionAt != nil {
		lastActionText = req.LastActionAt.Format("2006-01-02 15:04")
	} else if !req.UpdatedAt.IsZero() {
		lastActionText = req.UpdatedAt.Format("2006-01-02 15:04")
	}

	assigneeText := "未接单"
	if strings.TrimSpace(req.AssigneeName) != "" {
		assigneeText = req.AssigneeName
	}

	text := fmt.Sprintf(
		"📚 求书工单 #%d\n\n"+
			"状态：%s\n"+
			"接单人：%s\n"+
			"最近更新：%s\n\n"+
			"用户：%s\n"+
			"用户ID：%d\n\n"+
			"喜马拉雅链接：\n%s\n\n"+
			"用户备注：\n%s\n\n"+
			"管理员备注：\n%s",
		req.ID,
		bookRequestStatusText(req.Status),
		DisplayBookRequestText(assigneeText, "未接单"),
		lastActionText,
		DisplayBookRequestText(req.UserName, "未知用户"),
		req.UserID,
		req.XmlyLink,
		DisplayBookRequestText(req.UserNote, "无"),
		DisplayBookRequestText(req.AdminNote, "无"),
	)

	if isBookRequestClosedStatus(req.Status) {
		text += fmt.Sprintf(
			"\n\n处理人：%s\n处理时间：%s",
			DisplayBookRequestText(req.AdminName, "未知管理员"),
			formatBookRequestTime(req.CompletedAt),
		)
	}

	return text
}

func EditBookRequestAdminMessage(bot *tgbotapi.BotAPI, chatID int64, messageID int, req model.BookRequest, removeButtons bool) {
	if bot == nil || chatID == 0 || messageID == 0 {
		return
	}
	edit := tgbotapi.NewEditMessageText(chatID, messageID, formatBookRequestAdminText(req))
	edit.DisableWebPagePreview = true
	if removeButtons {
		emptyMarkup := tgbotapi.NewInlineKeyboardMarkup()
		edit.ReplyMarkup = &emptyMarkup
	} else {
		var keyboard tgbotapi.InlineKeyboardMarkup

		if req.Status == bookRequestStatusPending {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🤝 接单", fmt.Sprintf("br_claim_%d", req.ID)),
				),
			)
		} else if IsBookRequestOperableStatus(req.Status) {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("✅ 已上传", fmt.Sprintf("br_done_%d", req.ID)),
					tgbotapi.NewInlineKeyboardButtonData("❌ 暂无资源", fmt.Sprintf("br_reject_%d", req.ID)),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("📝 管理员备注", fmt.Sprintf("br_note_%d", req.ID)),
					tgbotapi.NewInlineKeyboardButtonData("❓ 需补充信息", fmt.Sprintf("br_need_info_%d", req.ID)),
				),
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🚫 取消工单", fmt.Sprintf("br_cancel_%d", req.ID)),
				),
			)
		} else if isBookRequestUploadedStatus(req.Status) {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🔄 重新读取候选", fmt.Sprintf("br_ann_refresh_%d", req.ID)),
					tgbotapi.NewInlineKeyboardButtonData("跳过公告", fmt.Sprintf("br_ann_skip_%d", req.ID)),
				),
			)
		}

		if len(keyboard.InlineKeyboard) > 0 {
			edit.ReplyMarkup = &keyboard
		}
	}

	if _, err := bot.Request(edit); err != nil {
		log.Printf("⚠️ 刷新求书工单管理员消息失败: req=%d chat=%d msg=%d err=%s", req.ID, chatID, messageID, textfmt.FormatTelegramSendError(err))
	}
}

func RefreshStoredBookRequestAdminMessage(bot *tgbotapi.BotAPI, req model.BookRequest, removeButtons bool, skipChatID int64, skipMessageID int) {
	if req.AdminChatID == 0 || req.AdminMessageID == 0 {
		return
	}

	if req.AdminChatID == skipChatID && req.AdminMessageID == skipMessageID {
		return
	}

	EditBookRequestAdminMessage(bot, req.AdminChatID, req.AdminMessageID, req, removeButtons)
}

func formatBookRequestUserResultText(req model.BookRequest) string {
	if isBookRequestUploadedStatus(req.Status) {
		return fmt.Sprintf(
			"✅ 你提交的求书已处理完成。\n\n"+
				"喜马拉雅链接：\n%s\n\n"+
				"管理员备注：\n%s\n\n"+
				"请前往 ABS 搜索查看。",
			req.XmlyLink,
			DisplayBookRequestText(req.AdminNote, "无"),
		)
	}

	if req.Status == BookRequestStatusNeedInfo {
		return fmt.Sprintf(
			"❓ 你的求书 #%d 需要补充信息：\n\n%s\n\n请直接回复补充内容。",
			req.ID,
			DisplayBookRequestText(req.AdminNote, "请补充更详细的信息"),
		)
	}

	if req.Status == bookRequestStatusRejected {
		rejectText := fmt.Sprintf(
			"📚 你提交的求书暂时无法处理。\n\n"+
				"喜马拉雅链接：\n%s\n\n"+
				"管理员备注：\n%s",
			req.XmlyLink,
			DisplayBookRequestText(req.AdminNote, "无"),
		)
		if req.RefundedAt != nil && req.CostPaid > 0 {
			rejectText += fmt.Sprintf("\n\n已退还 %d 积分。", req.CostPaid)
		}
		return rejectText
	}

	return fmt.Sprintf(
		"📚 你的求书状态已更新。\n\n"+
			"喜马拉雅链接：\n%s\n\n"+
			"当前状态：%s\n\n"+
			"管理员备注：\n%s",
		req.XmlyLink,
		bookRequestStatusText(req.Status),
		DisplayBookRequestText(req.AdminNote, "无"),
	)
}

func notifyBookRequestAdmins(bot *tgbotapi.BotAPI, req model.BookRequest) {
	adminIDs := collectBookRequestAdminIDs()

	if len(adminIDs) == 0 {
		log.Printf("⚠️ 新求书工单 #%d 创建成功，但没有找到可通知的管理员", req.ID)
		return
	}

	adminText := formatBookRequestAdminText(req)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🤝 接单", fmt.Sprintf("br_claim_%d", req.ID)),
		),
	)

	for adminID := range adminIDs {
		msg := tgbotapi.NewMessage(adminID, adminText)
		msg.DisableWebPagePreview = true
		msg.ReplyMarkup = keyboard

		sentMsg, err := telegram.SendAutoDelete(bot, msg)
		if err != nil {
			log.Printf("⚠️ 求书工单通知管理员失败: req=%d admin=%d err=%s", req.ID, adminID, textfmt.FormatTelegramSendError(err))
			continue
		}

		// 保存第一条成功发送的管理员工单消息
		if req.AdminChatID == 0 && req.AdminMessageID == 0 {
			if err := recordBookRequestAdminMessageID(storage.DB, &req, sentMsg.Chat.ID, sentMsg.MessageID); err != nil {
				log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, sentMsg.Chat.ID, sentMsg.MessageID, textfmt.FormatPlainError(err))
			}
		}
	}
}

func HandleBookRequestStart(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, session *telegram.SessionState) {
	if msg == nil || msg.From == nil {
		return
	}

	if !hasActiveAbsAccount(msg.From.ID) {
		telegram.SendPlainText(bot, msg.Chat.ID,
			"📚 求书功能仅限当前有效的 ABS 用户使用。\n\n"+
				"请先注册 / 绑定 ABS 账号，或完成续期后再提交求书。",
		)
		return
	}

	limitMsg := checkBookRequestLimits(msg.From.ID)
	if limitMsg != "" {
		telegram.SendPlainText(bot, msg.Chat.ID, "⚠️ "+limitMsg)
		return
	}

	session.SetStep("WAITING_BOOK_LINK")
	telegram.UserSessions.Store(msg.From.ID, session)

	planText := "每次提交需消耗积分，具体以确认页为准。"
	if _, plan, err := LoadBookRequestUserPlanWithDB(storage.DB, msg.From.ID, time.Now()); err == nil {
		planText = bookRequestPlanText(plan)
	}

	telegram.SendPlainText(bot, msg.Chat.ID,
		"📚 求书提交\n\n"+
			planText+"\n\n"+
			"请发送喜马拉雅链接。\n\n"+
			"要求："+BookRequestLinkRequirementText+"。\n\n"+
			"发送“取消”可退出。",
	)
}

func ShowMyBookRequests(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	if !hasActiveAbsAccount(userID) {
		telegram.SendPlainText(bot, chatID,
			"📋 我的求书功能仅限当前有效的 ABS 用户使用。\n\n"+
				"请先注册 / 绑定 ABS 账号，或完成续期后再查看求书记录。",
		)
		return
	}

	var reqs []model.BookRequest
	if err := storage.DB.Where("user_id = ?", userID).
		Order("created_at DESC").
		Limit(5).
		Find(&reqs).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 查询求书记录失败，请稍后再试。")
		return
	}

	if len(reqs) == 0 {
		telegram.SendPlainText(bot, chatID, "📋 你还没有提交过求书。")
		return
	}
	var b strings.Builder
	b.WriteString("📋 我的求书记录\n")
	b.WriteString("以下显示最近 5 条：\n\n")

	for _, req := range reqs {
		lastActionText := req.UpdatedAt.Format("2006-01-02 15:04")
		if req.LastActionAt != nil {
			lastActionText = req.LastActionAt.Format("2006-01-02 15:04")
		}

		assigneeText := "未接单"
		if strings.TrimSpace(req.AssigneeName) != "" {
			assigneeText = req.AssigneeName
		}

		b.WriteString(fmt.Sprintf("#%d  %s\n", req.ID, bookRequestStatusText(req.Status)))
		b.WriteString(fmt.Sprintf("接单人：%s\n", assigneeText))
		b.WriteString(fmt.Sprintf("最近更新：%s\n", lastActionText))
		b.WriteString("喜马拉雅链接：\n")
		b.WriteString(req.XmlyLink)
		b.WriteString("\n")
		b.WriteString("用户备注：\n")
		b.WriteString(DisplayBookRequestText(req.UserNote, "无"))
		b.WriteString("\n")
		b.WriteString("管理员备注：\n")
		b.WriteString(DisplayBookRequestText(req.AdminNote, "无"))
		b.WriteString("\n\n")
	}

	msg := tgbotapi.NewMessage(chatID, b.String())

	// 待接单的工单提供取消入口（二次点击确认，确认后退还创建时扣除的积分）。
	if cancelRows := buildBookRequestUserCancelRows(userID); len(cancelRows) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(cancelRows...)
	}

	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 发送我的求书记录失败: user=%d err=%s", userID, textfmt.FormatTelegramSendError(err))
	}
}

func ShowPendingBookRequests(bot *tgbotapi.BotAPI, chatID int64) {
	var reqs []model.BookRequest

	if err := storage.DB.
		Where("status = ?", bookRequestStatusPending).
		Order("created_at ASC").
		Limit(20).
		Find(&reqs).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 查询待处理求书失败。")
		return
	}

	if len(reqs) == 0 {
		telegram.SendPlainText(bot, chatID, "📚 当前没有待处理求书工单。")
		return
	}

	var builder strings.Builder
	builder.WriteString("📚 待接单求书工单\n\n")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)

	for _, req := range reqs {
		builder.WriteString(fmt.Sprintf(
			"#%d\n用户：%s\n提交时间：%s\n\n",
			req.ID,
			DisplayBookRequestText(req.UserName, "未知用户"),
			req.CreatedAt.Format("2006-01-02 15:04"),
		))

		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("查看 / 处理 #%d", req.ID),
				fmt.Sprintf("br_view_%d", req.ID),
			),
		))
	}

	builder.WriteString(fmt.Sprintf("\n共 %d 条待处理工单。", len(reqs)))

	msg := tgbotapi.NewMessage(chatID, builder.String())
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 发送待处理求书列表失败: err=%s", textfmt.FormatTelegramSendError(err))
	}
}

func ShowMyClaimedBookRequests(bot *tgbotapi.BotAPI, chatID int64, adminID int64) {
	var reqs []model.BookRequest

	if err := storage.DB.
		Where("assignee_id = ? AND status IN ?", adminID, BookRequestOperableStatuses()).
		Order("last_action_at DESC").
		Limit(20).
		Find(&reqs).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "❌ 查询我的处理工单失败。")
		return
	}

	if len(reqs) == 0 {
		telegram.SendPlainText(bot, chatID, "📚 你当前没有正在处理的求书工单。")
		return
	}

	var builder strings.Builder
	builder.WriteString("📚 我的处理工单\n\n")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0)

	for _, req := range reqs {
		lastActionText := req.UpdatedAt.Format("2006-01-02 15:04")
		if req.LastActionAt != nil {
			lastActionText = req.LastActionAt.Format("2006-01-02 15:04")
		}

		builder.WriteString(fmt.Sprintf(
			"#%d  %s\n用户：%s\n最近更新：%s\n\n",
			req.ID,
			bookRequestStatusText(req.Status),
			DisplayBookRequestText(req.UserName, "未知用户"),
			lastActionText,
		))

		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("查看 / 处理 #%d", req.ID),
				fmt.Sprintf("br_view_%d", req.ID),
			),
		))
	}

	msg := tgbotapi.NewMessage(chatID, builder.String())
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)

	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 发送我的处理工单失败: admin=%d err=%s", adminID, textfmt.FormatTelegramSendError(err))
	}
}

func sendBookRequestDetail(bot *tgbotapi.BotAPI, chatID int64, req model.BookRequest) {
	msg := tgbotapi.NewMessage(chatID, formatBookRequestAdminText(req))
	msg.DisableWebPagePreview = true
	if req.Status == bookRequestStatusPending {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🤝 接单", fmt.Sprintf("br_claim_%d", req.ID)),
			),
		)
	} else if IsBookRequestOperableStatus(req.Status) {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ 已上传", fmt.Sprintf("br_done_%d", req.ID)),
				tgbotapi.NewInlineKeyboardButtonData("❌ 暂无资源", fmt.Sprintf("br_reject_%d", req.ID)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📝 管理员备注", fmt.Sprintf("br_note_%d", req.ID)),
				tgbotapi.NewInlineKeyboardButtonData("❓ 需补充信息", fmt.Sprintf("br_need_info_%d", req.ID)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🚫 取消工单", fmt.Sprintf("br_cancel_%d", req.ID)),
			),
		)
	} else if isBookRequestUploadedStatus(req.Status) {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 重新读取候选", fmt.Sprintf("br_ann_refresh_%d", req.ID)),
				tgbotapi.NewInlineKeyboardButtonData("跳过公告", fmt.Sprintf("br_ann_skip_%d", req.ID)),
			),
		)
	}

	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 发送求书详情失败: req=%d err=%s", req.ID, textfmt.FormatTelegramSendError(err))
	}
}

func SubmitBookRequest(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, session *telegram.SessionState) {
	if msg == nil || msg.From == nil {
		return
	}

	if !hasActiveAbsAccount(msg.From.ID) {
		telegram.SendPlainText(bot, msg.Chat.ID,
			"📚 求书提交失败。\n\n"+
				"该功能仅限当前有效的 ABS 用户使用，请先注册 / 绑定 ABS 账号，或完成续期后再提交。",
		)
		telegram.ClearSession(msg.From.ID)
		return
	}

	limitMsg := checkBookRequestLimits(msg.From.ID)
	if limitMsg != "" {
		telegram.SendPlainText(bot, msg.Chat.ID, "⚠️ "+limitMsg)
		telegram.ClearSession(msg.From.ID)
		return
	}

	xmlyLink := session.GetTemp("book_xmly_link")
	userNote := session.GetTemp("book_user_note")
	userName := textfmt.GetTelegramDisplayName(msg.From)

	now := time.Now()
	req := model.BookRequest{
		UserID:       msg.From.ID,
		UserName:     userName,
		XmlyLink:     xmlyLink,
		UserNote:     userNote,
		Status:       bookRequestStatusPending,
		LastActionAt: &now,
	}

	limitMsg, err := createBookRequestWithinLimits(&req, now)
	if limitMsg != "" {
		telegram.SendPlainText(bot, msg.Chat.ID, "⚠️ "+limitMsg)
		telegram.ClearSession(msg.From.ID)
		return
	}
	if err != nil {
		if errors.Is(err, storage.ErrInsufficientPoints) {
			telegram.SendPlainText(bot, msg.Chat.ID, "❌ 积分不足，无法提交求书工单。")
			telegram.ClearSession(msg.From.ID)
			return
		}
		log.Printf("❌ 创建求书工单失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
		telegram.SendPlainText(bot, msg.Chat.ID, "❌ 提交失败，请稍后再试。")
		telegram.ClearSession(msg.From.ID)
		return
	}

	notifyBookRequestAdmins(bot, req)

	telegram.SendPlainText(bot, msg.Chat.ID,
		fmt.Sprintf(
			"✅ 求书已提交，工单编号：#%d\n\n管理员处理后，你会收到通知。",
			req.ID,
		),
	)

	telegram.ClearSession(msg.From.ID)
}

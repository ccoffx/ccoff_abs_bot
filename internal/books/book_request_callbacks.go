package books

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/metrics"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func IsBookRequestAdmin(userID int64) bool {
	role := storage.GetUserRole(userID)
	return role == "super_admin" || role == "admin"
}

func parseBookRequestCallbackID(data string, prefix string) (uint, bool) {
	if !strings.HasPrefix(data, prefix) {
		return 0, false
	}

	rawID := strings.TrimPrefix(data, prefix)
	id64, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || id64 == 0 {
		return 0, false
	}
	if id64 > uint64(^uint(0)) {
		return 0, false
	}

	return uint(id64), true
}

func LoadBookRequestByID(db *gorm.DB, reqID uint, context string) (model.BookRequest, bool, error) {
	var req model.BookRequest
	if err := db.Where("id = ?", reqID).First(&req).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return req, false, nil
		}
		log.Printf("⚠️ 求书工单读取失败: context=%s req=%d err=%s", textfmt.FormatPlainValue(context), reqID, textfmt.FormatPlainError(err))
		return req, false, err
	}
	return req, true, nil
}

func StartDelayedCallbackAck(bot *tgbotapi.BotAPI, callbackID string) {
	if bot == nil || strings.TrimSpace(callbackID) == "" {
		return
	}

	stateValue, _ := telegram.CallbackAckStates.LoadOrStore(callbackID, &atomic.Bool{})
	state, ok := stateValue.(*atomic.Bool)
	if !ok {
		telegram.CallbackAckStates.Delete(callbackID)
		return
	}

	go func() {
		time.Sleep(telegram.CallbackFastAckDelay)
		defer telegram.CallbackAckStates.Delete(callbackID)
		if !state.CompareAndSwap(false, true) {
			return
		}
		metrics.RecordCallbackFastAck()
		cb := tgbotapi.NewCallback(callbackID, telegram.FormatCallbackAlertText("操作处理中"))
		if _, err := bot.Request(cb); err != nil && !telegram.IsOldTelegramCallbackError(err) {
			log.Printf("⚠️ 快速确认 callback 失败: err=%s", textfmt.FormatTelegramSendError(err))
		}
	}()
}

// buildBookRequestUserCancelRows 查询用户当前待接单工单，生成取消按钮行（无则返回 nil）。
func buildBookRequestUserCancelRows(userID int64) [][]tgbotapi.InlineKeyboardButton {
	var reqs []model.BookRequest
	if err := storage.DB.Where("user_id = ? AND status = ?", userID, bookRequestStatusPending).
		Order("created_at DESC").
		Limit(5).
		Find(&reqs).Error; err != nil {
		log.Printf("⚠️ 求书取消按钮列表读取失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return nil
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, req := range reqs {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("❌ 取消求书 #%d", req.ID),
				fmt.Sprintf("br_ucancel_%d", req.ID),
			),
		))
	}
	return rows
}

// handleBookRequestUserCancel 用户取消自己的待接单工单（confirmed=false 时先做二次点击确认）。
// 取消后按创建时扣费金额退还积分（CostPaid<=0 或已退还则跳过）。
func handleBookRequestUserCancel(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery, reqID uint, confirmed bool) {
	if bot == nil || cb == nil || cb.From == nil || reqID == 0 {
		return
	}

	req, found, err := LoadBookRequestByID(storage.DB, reqID, "user cancel")
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
		return
	}
	if !found {
		telegram.AnswerCallback(bot, cb.ID, "工单不存在")
		return
	}

	if req.UserID != cb.From.ID {
		telegram.AnswerCallback(bot, cb.ID, "只能取消自己的求书工单")
		return
	}

	if req.Status != bookRequestStatusPending {
		telegram.AnswerCallback(bot, cb.ID, "该工单已被接单或已处理，无法自行取消")
		return
	}

	if !confirmed {
		// 第一次点击：把按钮换成确认按钮，避免误触。
		if cb.Message != nil {
			confirmText := fmt.Sprintf("⚠️ 确认取消求书 #%d", req.ID)
			if req.CostPaid > 0 {
				confirmText = fmt.Sprintf("⚠️ 确认取消求书 #%d（退还 %d 积分）", req.ID, req.CostPaid)
			}
			edit := tgbotapi.NewEditMessageReplyMarkup(cb.Message.Chat.ID, cb.Message.MessageID,
				tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(confirmText, fmt.Sprintf("br_ucancel_ok_%d", req.ID)),
				)))
			if _, err := bot.Request(edit); err != nil {
				log.Printf("⚠️ 求书用户取消确认按钮刷新失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatTelegramSendError(err))
			}
		}
		telegram.AnswerCallback(bot, cb.ID, "请再次点击确认取消")
		return
	}

	now := time.Now()
	cancelled := false
	refunded := 0
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.BookRequest{}).
			Where("id = ? AND user_id = ? AND status = ?", reqID, cb.From.ID, bookRequestStatusPending).
			Updates(map[string]interface{}{
				"status": BookRequestStatusCancelled,
				// admin_name 仅作展示（无逻辑依赖）：无管理员参与的取消记录取消来源，
				// 避免管理端详情显示“未知管理员”；真实操作者仍在 book_request_logs 中。
				"admin_name":     "用户取消",
				"last_action_at": &now,
				"completed_at":   &now,
			})
		if res.Error != nil {
			return fmt.Errorf("book request user cancel update failed: %s", textfmt.FormatPlainError(res.Error))
		}
		if res.RowsAffected == 0 {
			return nil
		}
		cancelled = true

		amount, err := RefundBookRequestInTx(tx, &req, req.UserID, req.UserName, "cancelled by user", now)
		if err != nil {
			return err
		}
		refunded = amount

		if err := CreateBookRequestLogInTx(tx, reqID, req.UserID, req.UserName, "user_cancel", bookRequestStatusPending, BookRequestStatusCancelled, "user cancelled own pending request"); err != nil {
			return err
		}
		return storage.WriteAuditLogInTx(tx, req.UserID, "USER_CANCEL_BOOK_REQUEST", fmt.Sprintf("%d", reqID), 0, "user cancelled own pending book request")
	})
	if err != nil {
		log.Printf("book request user cancel failed: req=%d user=%d err=%s", reqID, cb.From.ID, textfmt.FormatPlainError(err))
		telegram.AnswerCallback(bot, cb.ID, "取消失败，请稍后再试")
		return
	}

	if !cancelled {
		telegram.AnswerCallback(bot, cb.ID, "该工单状态已变化，无法取消")
		return
	}

	var updatedReq model.BookRequest
	if err := storage.DB.Where("id = ?", reqID).First(&updatedReq).Error; err != nil {
		log.Printf("⚠️ 求书用户取消后读取失败: req=%d err=%s", reqID, textfmt.FormatPlainError(err))
		updatedReq = req
		updatedReq.Status = BookRequestStatusCancelled
	}

	// 刷新管理端消息（创建/操作时记录的第一条管理员消息），移除接单按钮。
	RefreshStoredBookRequestAdminMessage(bot, updatedReq, true, 0, 0)

	// 用户列表消息：取消完成后重建剩余待接单工单的取消按钮（无则清空），避免误触已取消工单。
	if cb.Message != nil {
		remainingRows := buildBookRequestUserCancelRows(cb.From.ID)
		markup := tgbotapi.NewInlineKeyboardMarkup(remainingRows...)
		edit := tgbotapi.NewEditMessageReplyMarkup(cb.Message.Chat.ID, cb.Message.MessageID, markup)
		if _, err := bot.Request(edit); err != nil && !telegram.IsTelegramMessageNotModifiedError(err) {
			log.Printf("⚠️ 求书用户取消后按钮刷新失败: req=%d chat=%d msg=%d err=%s", reqID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatTelegramSendError(err))
		}
	}

	callbackText := "已取消"
	if refunded > 0 {
		callbackText = fmt.Sprintf("已取消，退还 %d 积分", refunded)
	}
	telegram.AnswerCallback(bot, cb.ID, callbackText)

	msgText := fmt.Sprintf("📚 你的求书 #%d 已取消。\n", reqID)
	if refunded > 0 {
		msgText += fmt.Sprintf("\n已退还 %d 积分。", refunded)
	}
	msgText += "\n如仍需要该书，可重新提交求书工单。"
	telegram.SendPlainText(bot, cb.From.ID, msgText)
}

func HandleBookRequestCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) {
	if cb == nil || cb.From == nil {
		return
	}

	data := cb.Data
	if !strings.HasPrefix(data, "br_") {
		telegram.AnswerCallback(bot, cb.ID, "未知操作")
		return
	}

	// 用户取消自己的待接单工单：不要求管理员权限，但要校验工单归属。
	if reqID, ok := parseBookRequestCallbackID(data, "br_ucancel_ok_"); ok {
		handleBookRequestUserCancel(bot, cb, reqID, true)
		return
	}
	if reqID, ok := parseBookRequestCallbackID(data, "br_ucancel_"); ok {
		handleBookRequestUserCancel(bot, cb, reqID, false)
		return
	}

	if !IsBookRequestAdmin(cb.From.ID) {
		telegram.AnswerCallback(bot, cb.ID, "无权操作该求书工单")
		return
	}

	if handleBookRequestAnnouncementCallback(bot, cb) {
		return
	}

	if reqID, ok := parseBookRequestCallbackID(data, "br_view_"); ok {
		req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback view")
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
			return
		}
		if !found {
			telegram.AnswerCallback(bot, cb.ID, "工单不存在")
			return
		}

		if cb.Message != nil {
			sendBookRequestDetail(bot, cb.Message.Chat.ID, req)
		}

		telegram.AnswerCallback(bot, cb.ID, "已打开工单详情")
		return
	}

	if reqID, ok := parseBookRequestCallbackID(data, "br_claim_"); ok {
		req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback claim")
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
			return
		}
		if !found {
			telegram.AnswerCallback(bot, cb.ID, "工单不存在")
			return
		}

		if req.Status != bookRequestStatusPending {
			if cb.Message != nil {
				EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, true)
			}
			telegram.AnswerCallback(bot, cb.ID, "该工单已被接单或已处理")
			return
		}

		now := time.Now()
		adminName := textfmt.GetTelegramDisplayName(cb.From)

		claimed := false
		err = storage.DB.Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&model.BookRequest{}).
				Where("id = ? AND status = ?", reqID, bookRequestStatusPending).
				Updates(map[string]interface{}{
					"status":         bookRequestStatusClaimed,
					"assignee_id":    cb.From.ID,
					"assignee_name":  adminName,
					"admin_id":       cb.From.ID,
					"admin_name":     adminName,
					"claimed_at":     &now,
					"last_action_at": &now,
					"remind_count":   0,
				})
			if res.Error != nil {
				return fmt.Errorf("book request claim update failed: %s", textfmt.FormatPlainError(res.Error))
			}
			if res.RowsAffected == 0 {
				return nil
			}
			claimed = true
			if err := CreateBookRequestLogInTx(tx, reqID, cb.From.ID, adminName, "claim", bookRequestStatusPending, bookRequestStatusClaimed, "admin claimed book request"); err != nil {
				return err
			}
			return storage.WriteAuditLogInTx(tx, cb.From.ID, "CLAIM_BOOK_REQUEST", fmt.Sprintf("%d", reqID), 0, "admin claimed book request")
		})
		if err != nil {
			log.Printf("book request claim failed: req=%d admin=%d err=%s", reqID, cb.From.ID, textfmt.FormatPlainError(err))
			telegram.AnswerCallback(bot, cb.ID, "\u63a5\u5355\u5931\u8d25")
			return
		}

		if !claimed {
			telegram.AnswerCallback(bot, cb.ID, "\u8be5\u5de5\u5355\u521a\u521a\u5df2\u88ab\u522b\u4eba\u63a5\u5355")
			return
		}

		if err := reloadBookRequestAfterClaim(storage.DB, &req, reqID, cb.From.ID, adminName, now); err != nil {
			log.Printf("book request claim reload failed: req=%d admin=%d err=%s", reqID, cb.From.ID, textfmt.FormatPlainError(err))
		}

		if cb.Message != nil {
			if req.AdminChatID == 0 || req.AdminMessageID == 0 {
				if err := recordBookRequestAdminMessageID(storage.DB, &req, cb.Message.Chat.ID, cb.Message.MessageID); err != nil {
					log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatPlainError(err))
				}
			}

			EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, false)
		}

		telegram.SendPlainText(bot, req.UserID, fmt.Sprintf(
			"📚 你的求书 #%d 已由管理员接单。\n\n当前状态：处理中",
			req.ID,
		))

		telegram.AnswerCallback(bot, cb.ID, "已接单")
		return
	}

	if reqID, ok := parseBookRequestCallbackID(data, "br_need_info_"); ok {
		req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback need info")
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
			return
		}
		if !found {
			telegram.AnswerCallback(bot, cb.ID, "工单不存在")
			return
		}

		if !IsBookRequestOperableStatus(req.Status) {
			telegram.AnswerCallback(bot, cb.ID, "该工单当前不能要求补充信息")
			return
		}

		if !CanOperateBookRequest(req, cb.From.ID) {
			telegram.AnswerCallback(bot, cb.ID, "只有接单人或超级管理员可以操作")
			return
		}

		session := telegram.GetSession(cb.From.ID)
		session.SetStep("WAITING_BOOK_NEED_INFO_NOTE")
		session.SetTemp("book_need_info_req_id", fmt.Sprintf("%d", reqID))

		if cb.Message != nil {
			session.SetTemp("book_need_info_chat_id", fmt.Sprintf("%d", cb.Message.Chat.ID))
			session.SetTemp("book_need_info_message_id", fmt.Sprintf("%d", cb.Message.MessageID))
		}

		telegram.UserSessions.Store(cb.From.ID, session)

		telegram.AnswerCallback(bot, cb.ID, "请发送需要用户补充的内容")
		telegram.SendPlainText(bot, cb.From.ID, fmt.Sprintf("❓ 请发送求书工单 #%d 需要用户补充的信息。\n\n例如：请说明缺少哪几集 / 想要哪个主播版本。\n发送“"+textfmt.Copyable("取消")+"”可退出。", reqID))
		return
	}

	if reqID, ok := parseBookRequestCallbackID(data, "br_note_"); ok {
		req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback note")
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
			return
		}
		if !found {
			telegram.AnswerCallback(bot, cb.ID, "工单不存在")
			return
		}
		if !IsBookRequestOperableStatus(req.Status) {
			if cb.Message != nil {
				if req.AdminChatID == 0 || req.AdminMessageID == 0 {
					if err := recordBookRequestAdminMessageID(storage.DB, &req, cb.Message.Chat.ID, cb.Message.MessageID); err != nil {
						log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatPlainError(err))
					}
				}

				EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, true)
			}

			telegram.AnswerCallback(bot, cb.ID, "该工单已处理，已刷新状态")
			return
		}
		if !CanOperateBookRequest(req, cb.From.ID) {
			telegram.AnswerCallback(bot, cb.ID, "只有接单人或超级管理员可以备注")
			return
		}
		session := telegram.GetSession(cb.From.ID)
		session.SetStep("WAITING_BOOK_ADMIN_NOTE")
		session.SetTemp("book_admin_note_req_id", fmt.Sprintf("%d", reqID))

		if cb.Message != nil {
			session.SetTemp("book_admin_note_chat_id", fmt.Sprintf("%d", cb.Message.Chat.ID))
			session.SetTemp("book_admin_note_message_id", fmt.Sprintf("%d", cb.Message.MessageID))

			if req.AdminChatID == 0 || req.AdminMessageID == 0 {
				if err := recordBookRequestAdminMessageID(storage.DB, &req, cb.Message.Chat.ID, cb.Message.MessageID); err != nil {
					log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatPlainError(err))
				}
			}
		}

		telegram.UserSessions.Store(cb.From.ID, session)

		telegram.AnswerCallback(bot, cb.ID, "请发送管理员备注")
		telegram.SendPlainText(bot, cb.From.ID, fmt.Sprintf("📝 请发送求书工单 #%d 的管理员备注。\n\n最多 %d 字。\n发送“"+textfmt.Copyable("取消")+"”可退出。", reqID, bookRequestNoteMaxLen))
		return
	}

	if reqID, ok := parseBookRequestCallbackID(data, "br_cancel_"); ok {
		req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback cancel")
		if err != nil {
			telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
			return
		}
		if !found {
			telegram.AnswerCallback(bot, cb.ID, "工单不存在")
			return
		}

		if !IsBookRequestOperableStatus(req.Status) {
			if cb.Message != nil {
				EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, true)
			}
			telegram.AnswerCallback(bot, cb.ID, "该工单已处理，已刷新状态")
			return
		}
		if !CanOperateBookRequest(req, cb.From.ID) {
			telegram.AnswerCallback(bot, cb.ID, "只有接单人或超级管理员可以取消")
			return
		}

		session := telegram.GetSession(cb.From.ID)
		session.SetStep("WAITING_BOOK_CANCEL_REASON")
		session.SetTemp("book_cancel_req_id", fmt.Sprintf("%d", reqID))

		if cb.Message != nil {
			session.SetTemp("book_cancel_chat_id", fmt.Sprintf("%d", cb.Message.Chat.ID))
			session.SetTemp("book_cancel_message_id", fmt.Sprintf("%d", cb.Message.MessageID))
		}

		telegram.UserSessions.Store(cb.From.ID, session)

		refundHint := "本次取消不退还积分（无扣费记录）。"
		if req.CostPaid > 0 {
			refundHint = fmt.Sprintf("取消后将退还用户 %d 积分。", req.CostPaid)
		}
		telegram.AnswerCallback(bot, cb.ID, "请确认取消工单")
		telegram.SendPlainText(bot, cb.From.ID, fmt.Sprintf(
			"🚫 请确认取消求书工单 #%d。\n\n%s\n回复“确认取消求书”继续。\n发送“"+textfmt.Copyable("取消")+"”可退出。",
			reqID,
			refundHint,
		))
		return
	}

	status := ""

	if reqID, ok := parseBookRequestCallbackID(data, "br_done_"); ok {
		status = bookRequestStatusUploaded
		data = fmt.Sprintf("%d", reqID)
	} else if reqID, ok := parseBookRequestCallbackID(data, "br_reject_"); ok {
		status = bookRequestStatusRejected
		data = fmt.Sprintf("%d", reqID)
	} else {
		telegram.AnswerCallback(bot, cb.ID, "未知操作")
		return
	}

	reqID64, parseErr := strconv.ParseUint(data, 10, 64)
	if parseErr != nil || reqID64 == 0 {
		telegram.AnswerCallback(bot, cb.ID, "工单编号异常")
		return
	}
	reqID := uint(reqID64)

	req, found, err := LoadBookRequestByID(storage.DB, reqID, "callback finish")
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, "查询工单失败，请稍后重试")
		return
	}
	if !found {
		telegram.AnswerCallback(bot, cb.ID, "工单不存在")
		return
	}

	if !IsBookRequestOperableStatus(req.Status) {
		if cb.Message != nil {
			if req.AdminChatID == 0 || req.AdminMessageID == 0 {
				if err := recordBookRequestAdminMessageID(storage.DB, &req, cb.Message.Chat.ID, cb.Message.MessageID); err != nil {
					log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatPlainError(err))
				}
			}

			EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, true)
		}

		telegram.AnswerCallback(bot, cb.ID, "该工单已处理，已刷新状态")
		return
	}
	if !CanOperateBookRequest(req, cb.From.ID) {
		telegram.AnswerCallback(bot, cb.ID, "只有接单人或超级管理员可以处理")
		return
	}
	oldStatus := req.Status
	now := time.Now()
	adminName := textfmt.GetTelegramDisplayName(cb.From)

	finished := false
	refunded := 0
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.BookRequest{}).
			Where("id = ? AND status IN ?", reqID, BookRequestOperableStatuses()).
			Updates(map[string]interface{}{
				"status":         status,
				"admin_id":       cb.From.ID,
				"admin_name":     adminName,
				"last_action_at": &now,
				"completed_at":   &now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		finished = true

		// 业务规则：暂无资源（rejected）同样退还创建时扣除的积分；已上传不退。
		if status == bookRequestStatusRejected {
			amount, err := RefundBookRequestInTx(tx, &req, cb.From.ID, adminName, "rejected by admin", now)
			if err != nil {
				return err
			}
			refunded = amount
		}

		note := fmt.Sprintf("admin finished book request; status=%s", status)
		if err := CreateBookRequestLogInTx(tx, reqID, cb.From.ID, adminName, "finish", oldStatus, status, note); err != nil {
			return err
		}
		return storage.WriteAuditLogInTx(tx, cb.From.ID, "HANDLE_BOOK_REQUEST", fmt.Sprintf("%d", reqID), 0, note)
	})
	if err != nil {
		log.Printf("book request finish failed: req=%d err=%s", reqID, textfmt.FormatPlainError(err))
		telegram.AnswerCallback(bot, cb.ID, "\u5904\u7406\u5931\u8d25")
		return
	}

	if !finished {
		telegram.AnswerCallback(bot, cb.ID, "\u8be5\u5de5\u5355\u5df2\u5904\u7406")
		return
	}

	if err := ReloadBookRequestAfterFinish(storage.DB, &req, reqID, status, cb.From.ID, adminName, now); err != nil {
		log.Printf("book request finish reload failed: req=%d err=%s", reqID, textfmt.FormatPlainError(err))
	}

	log.Printf("✅ 求书工单处理完成: req=%d status=%s admin=%d refunded=%d", reqID, textfmt.FormatPlainValue(status), cb.From.ID, refunded)

	telegram.SendPlainText(bot, req.UserID, formatBookRequestUserResultText(req))
	callbackText := "已处理"
	if status == bookRequestStatusUploaded {
		callbackText = maybePromptBookRequestGroupAnnouncement(bot, cb.From.ID, req)
	} else if refunded > 0 {
		callbackText = fmt.Sprintf("已处理，退还 %d 积分", refunded)
	}

	currentChatID := int64(0)
	currentMessageID := 0

	if cb.Message != nil {
		currentChatID = cb.Message.Chat.ID
		currentMessageID = cb.Message.MessageID

		if req.AdminChatID == 0 || req.AdminMessageID == 0 {
			if err := recordBookRequestAdminMessageID(storage.DB, &req, cb.Message.Chat.ID, cb.Message.MessageID); err != nil {
				log.Printf("⚠️ 求书工单管理员消息ID记录失败: req=%d chat=%d msg=%d err=%s", req.ID, cb.Message.Chat.ID, cb.Message.MessageID, textfmt.FormatPlainError(err))
			}
		}

		EditBookRequestAdminMessage(bot, cb.Message.Chat.ID, cb.Message.MessageID, req, true)
	}

	RefreshStoredBookRequestAdminMessage(bot, req, true, currentChatID, currentMessageID)

	telegram.AnswerCallback(bot, cb.ID, callbackText)
}

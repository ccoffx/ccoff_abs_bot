package app

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

const groupReplyPrefix = "groupreply:"
const groupReplyKeywordStep = "WAITING_GROUP_REPLY_KEYWORD"
const groupReplyContentStep = "WAITING_GROUP_REPLY_CONTENT"
const groupReplyPageSize = 8

func validateGroupReply(keyword, content string) error {
	if keyword == "" || utf8.RuneCountInString(keyword) > 100 || textfmt.ContainsDisallowedControl(keyword, false) {
		return errors.New("提示词需为 1–100 个字，不能包含换行或控制字符。")
	}
	if textfmt.IsReservedBotInput(keyword) {
		return errors.New("提示词与现有 Bot 指令或菜单冲突，请换一个。")
	}
	if strings.TrimSpace(content) == "" || len(utf16.Encode([]rune(content))) > 4000 || textfmt.ContainsDisallowedControl(content, true) {
		return errors.New("回复内容不能为空，最多 4000 个字符（表情可能占两个字符），支持多行纯文本。")
	}
	return nil
}

// Mutations and their audit entries commit together. The database unique index
// also prevents simultaneous administrators from creating duplicate keywords.
func saveGroupReply(actor int64, id uint, keyword, content string) error {
	keyword = strings.TrimSpace(keyword)
	if err := validateGroupReply(keyword, content); err != nil {
		return err
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&model.GroupReply{}).Where("keyword = ? AND id <> ?", keyword, id).Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return errors.New("该提示词已存在，请编辑原规则或换一个。")
		}
		r := model.GroupReply{Keyword: keyword, Content: content, Enabled: true}
		action := "GROUP_REPLY_CREATE"
		if id == 0 {
			if err := tx.Create(&r).Error; err != nil {
				return err
			}
		} else {
			if err := tx.First(&r, id).Error; err != nil {
				return err
			}
			r.Keyword, r.Content = keyword, content
			if err := tx.Save(&r).Error; err != nil {
				return err
			}
			action = "GROUP_REPLY_EDIT"
		}
		return storage.WriteAuditLogInTx(tx, actor, action, strconv.FormatUint(uint64(r.ID), 10), 0, "keyword="+keyword)
	})
}

func mutateGroupReply(actor int64, id uint, remove bool) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		var r model.GroupReply
		if err := tx.First(&r, id).Error; err != nil {
			return err
		}
		action := "GROUP_REPLY_TOGGLE"
		if remove {
			if err := tx.Delete(&r).Error; err != nil {
				return err
			}
			action = "GROUP_REPLY_DELETE"
		} else {
			r.Enabled = !r.Enabled
			if err := tx.Save(&r).Error; err != nil {
				return err
			}
		}
		return storage.WriteAuditLogInTx(tx, actor, action, strconv.FormatUint(uint64(id), 10), 0, fmt.Sprintf("keyword=%s enabled=%t", r.Keyword, r.Enabled))
	})
}

func groupReplyPreview(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
func groupReplyButton(label, action string) []tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, groupReplyPrefix+action))
}
func sendGroupReplyPanel(bot *tgbotapi.BotAPI, chatID int64, text string, rows [][]tgbotapi.InlineKeyboardButton) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	if _, err := telegram.SendNoAutoDelete(bot, msg); err != nil {
		log.Printf("group reply panel: %s", textfmt.FormatTelegramSendError(err))
	}
}
func showGroupReplies(bot *tgbotapi.BotAPI, chatID int64, page int) {
	var count int64
	if err := storage.DB.Model(&model.GroupReply{}).Count(&count).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "规则读取失败，请稍后重试。")
		return
	}
	last := int((count - 1) / groupReplyPageSize)
	if page > last {
		page = last
	}
	if page < 0 {
		page = 0
	}
	var rules []model.GroupReply
	if err := storage.DB.Order("id asc").Offset(page * groupReplyPageSize).Limit(groupReplyPageSize).Find(&rules).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "规则读取失败，请稍后重试。")
		return
	}
	rows := [][]tgbotapi.InlineKeyboardButton{groupReplyButton("➕ 新增规则", "add")}
	for _, r := range rules {
		status := "停用"
		if r.Enabled {
			status = "启用"
		}
		rows = append(rows, groupReplyButton(fmt.Sprintf("#%d [%s] %s", r.ID, status, groupReplyPreview(r.Keyword, 24)), fmt.Sprintf("view:%d", r.ID)))
	}
	if page > 0 {
		rows = append(rows, groupReplyButton("上一页", fmt.Sprintf("list:%d", page-1)))
	}
	if page < last {
		rows = append(rows, groupReplyButton("下一页", fmt.Sprintf("list:%d", page+1)))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("返回系统配置", "admin:config")))
	sendGroupReplyPanel(bot, chatID, fmt.Sprintf("群关键词回复（共 %d 条，第 %d/%d 页）\n\n仅在官方群触发；整条消息完全匹配，忽略首尾空格，区分大小写。现有业务指令优先。\n点击规则可修改、启停或删除。", count, page+1, last+1), rows)
}
func showGroupReply(bot *tgbotapi.BotAPI, chatID int64, id uint) {
	var r model.GroupReply
	if err := storage.DB.First(&r, id).Error; err != nil {
		telegram.SendPlainText(bot, chatID, "规则不存在或读取失败，请刷新列表。")
		return
	}
	status := "停用"
	if r.Enabled {
		status = "启用"
	}
	sendGroupReplyPanel(bot, chatID, fmt.Sprintf("规则 #%d [%s]\n提示词：%s\n\n回复内容：\n%s", id, status, r.Keyword, groupReplyPreview(r.Content, 600)), [][]tgbotapi.InlineKeyboardButton{
		groupReplyButton("✏️ 修改提示词", fmt.Sprintf("keyword:%d", id)),
		groupReplyButton("✏️ 修改回复", fmt.Sprintf("content:%d", id)),
		groupReplyButton("启用 / 停用", fmt.Sprintf("toggle:%d", id)),
		groupReplyButton("删除规则", fmt.Sprintf("delete:%d", id)),
		groupReplyButton("返回规则列表", "list:0"),
	})
}

func handleGroupReplyAdminMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.Chat == nil || msg.From == nil || !msg.Chat.IsPrivate() {
		return false
	}
	session := telegram.GetSession(msg.From.ID)
	step := session.GetStep()
	if text != "群关键词回复" && step != groupReplyKeywordStep && step != groupReplyContentStep {
		return false
	}
	if storage.GetUserRole(msg.From.ID) != "super_admin" {
		telegram.ClearSession(msg.From.ID)
		telegram.SendPlainText(bot, msg.Chat.ID, "该功能仅限超级管理员。")
		return true
	}
	if text == "群关键词回复" || text == "取消" {
		telegram.ClearSession(msg.From.ID)
		showGroupReplies(bot, msg.Chat.ID, 0)
		return true
	}
	if text == "" {
		telegram.SendPlainText(bot, msg.Chat.ID, "请发送文本，或发送「取消」退出。")
		return true
	}
	id, _ := strconv.ParseUint(session.GetTemp("group_reply_id"), 10, 64)
	keyword, content := session.GetTemp("group_reply_keyword"), session.GetTemp("group_reply_content")
	if step == groupReplyKeywordStep {
		keyword = strings.TrimSpace(text)
		if err := validateGroupReply(keyword, "placeholder"); err != nil {
			telegram.SendPlainText(bot, msg.Chat.ID, err.Error())
			return true
		}
		if id == 0 {
			session.SetTemp("group_reply_keyword", keyword)
			session.SetStep(groupReplyContentStep)
			telegram.SendPlainText(bot, msg.Chat.ID, "请发送该提示词对应的回复内容（最多 4000 字符，支持多行纯文本）。发送「取消」退出。")
			return true
		}
	} else {
		content = msg.Text
	}
	if err := saveGroupReply(msg.From.ID, uint(id), keyword, content); err != nil {
		log.Printf("save group reply: %s", textfmt.FormatPlainError(err))
		// Validation errors are safe to show; database diagnostics stay in logs.
		hint := "保存失败，请检查提示词是否重复、规则是否已删除，然后重试。"
		if validation := validateGroupReply(keyword, content); validation != nil {
			hint = validation.Error()
		}
		telegram.SendPlainText(bot, msg.Chat.ID, hint)
		return true
	}
	telegram.ClearSession(msg.From.ID)
	telegram.SendPlainText(bot, msg.Chat.ID, "规则已保存，立即生效。")
	showGroupReplies(bot, msg.Chat.ID, 0)
	return true
}

func handleGroupReplyCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, groupReplyPrefix) {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || !cb.Message.Chat.IsPrivate() || cb.Message.Chat.ID != cb.From.ID || storage.GetUserRole(cb.From.ID) != "super_admin" {
		telegram.AnswerCallback(bot, cb.ID, "仅限超级管理员在私聊中设置。")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(cb.Data, groupReplyPrefix), ":")
	var number uint64
	var err error
	if len(parts) == 2 {
		number, err = strconv.ParseUint(parts[1], 10, 32)
	}
	if err != nil || (parts[0] != "add" && len(parts) != 2) {
		telegram.AnswerCallback(bot, cb.ID, "无效操作，请刷新列表。")
		return true
	}
	id, chatID := uint(number), cb.Message.Chat.ID
	telegram.ClearSession(cb.From.ID)
	switch parts[0] {
	case "list":
		telegram.AnswerCallback(bot, cb.ID, "已刷新")
		showGroupReplies(bot, chatID, int(number))
	case "add":
		session := telegram.GetSession(cb.From.ID)
		session.SetStep(groupReplyKeywordStep)
		telegram.AnswerCallback(bot, cb.ID, "请输入提示词")
		telegram.SendPlainText(bot, chatID, "请发送提示词（1–100 个字，不能与 Bot 指令或菜单冲突）。发送「取消」退出。")
	case "view":
		telegram.AnswerCallback(bot, cb.ID, "已打开")
		showGroupReply(bot, chatID, id)
	case "keyword", "content":
		var r model.GroupReply
		if err := storage.DB.First(&r, id).Error; err != nil {
			telegram.AnswerCallback(bot, cb.ID, "规则不存在或读取失败。")
			return true
		}
		session := telegram.GetSession(cb.From.ID)
		session.SetTemp("group_reply_id", strconv.FormatUint(number, 10))
		session.SetTemp("group_reply_keyword", r.Keyword)
		session.SetTemp("group_reply_content", r.Content)
		step, prompt := groupReplyKeywordStep, "请发送新的提示词。发送「取消」退出。"
		if parts[0] == "content" {
			step, prompt = groupReplyContentStep, "请发送新的回复内容（最多 4000 字符，支持多行纯文本）。发送「取消」退出。"
		}
		session.SetStep(step)
		telegram.AnswerCallback(bot, cb.ID, "请输入新内容")
		telegram.SendPlainText(bot, chatID, prompt)
	case "delete":
		telegram.AnswerCallback(bot, cb.ID, "请确认删除")
		sendGroupReplyPanel(bot, chatID, fmt.Sprintf("确认删除规则 #%d？", id), [][]tgbotapi.InlineKeyboardButton{groupReplyButton("确认删除", fmt.Sprintf("remove:%d", id)), groupReplyButton("取消", fmt.Sprintf("view:%d", id))})
	case "toggle", "remove":
		if err := mutateGroupReply(cb.From.ID, id, parts[0] == "remove"); err != nil {
			log.Printf("mutate group reply: %s", textfmt.FormatPlainError(err))
			telegram.AnswerCallback(bot, cb.ID, "操作失败，规则可能已删除，请刷新列表。")
			return true
		}
		telegram.AnswerCallback(bot, cb.ID, "已更新")
		if parts[0] == "remove" {
			showGroupReplies(bot, chatID, 0)
		} else {
			showGroupReply(bot, chatID, id)
		}
	default:
		telegram.AnswerCallback(bot, cb.ID, "未知操作。")
	}
	return true
}

// Called after built-in business handlers so activity keywords retain priority.
func handleGroupKeywordReply(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.Chat == nil || msg.From == nil || msg.From.IsBot || config.AppConfig == nil || config.AppConfig.NoticeGroupID == 0 || msg.Chat.ID != config.AppConfig.NoticeGroupID || (msg.Chat.Type != "group" && msg.Chat.Type != "supergroup") || text == "" || textfmt.IsReservedBotInput(text) || storage.DB == nil {
		return false
	}
	var r model.GroupReply
	err := storage.DB.Where("keyword = ? AND enabled = ?", strings.TrimSpace(text), true).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		log.Printf("group reply lookup: %s", textfmt.FormatPlainError(err))
		return false
	}
	reply := tgbotapi.NewMessage(msg.Chat.ID, r.Content)
	reply.ReplyToMessageID = msg.MessageID
	reply.AllowSendingWithoutReply = true
	if _, err := telegram.SendAutoDelete(bot, reply); err != nil {
		log.Printf("group keyword reply: %s", textfmt.FormatTelegramSendError(err))
	}
	return true
}

// Package announcements delivers durable public posts and separate group reminders.
package announcements

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var dispatchMu sync.Mutex

func Target() int64 {
	if config.AppConfig == nil {
		return 0
	}
	if config.AppConfig.AnnouncementChannelID != 0 {
		return config.AppConfig.AnnouncementChannelID
	}
	return config.AppConfig.NoticeGroupID
}
func TargetLabel(id int64) string {
	if config.AppConfig != nil && id == config.AppConfig.AnnouncementChannelID && id != 0 {
		return fmt.Sprintf("公告频道（%d）", id)
	}
	return fmt.Sprintf("公告目标（%d）", id)
}
func Validate(bot *tgbotapi.BotAPI) error {
	id := config.AppConfig.AnnouncementChannelID
	if id == 0 {
		return nil
	}
	chat, err := bot.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: id}})
	if err != nil {
		return err
	}
	if chat.Type != "channel" {
		return fmt.Errorf("ANNOUNCEMENT_CHANNEL_ID must identify a channel")
	}
	m, err := bot.GetChatMember(tgbotapi.GetChatMemberConfig{ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: id, UserID: bot.Self.ID}})
	if err != nil {
		return err
	}
	if m.Status != "creator" && (m.Status != "administrator" || !m.CanPostMessages) {
		return fmt.Errorf("announcement Bot requires channel administrator and post-message permission")
	}
	return nil
}
func Queue(db *gorm.DB, d model.AnnouncementDelivery) error {
	if d.TargetChatID == 0 {
		return fmt.Errorf("公告目标未配置")
	}
	d.State = "pending"
	d.ReminderState = "pending"
	if d.ReminderGroupID == 0 {
		d.ReminderState = "none"
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&d).Error
}
func QueueEdit(db *gorm.DB, key, parent, kind, text string, actor int64, remind bool) error {
	var root model.AnnouncementDelivery
	if err := db.Where("key = ?", parent).First(&root).Error; err != nil {
		return err
	}
	group := int64(0)
	if remind {
		group = root.ReminderGroupID
	}
	return Queue(db, model.AnnouncementDelivery{Key: key, ParentKey: parent, Kind: kind, TargetChatID: root.TargetChatID, ReminderGroupID: group, ActorID: actor, Text: text, MarkupJSON: root.MarkupJSON})
}
func MessageLink(chatID int64, username string, messageID int) string {
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", strings.TrimPrefix(username, "@"), messageID)
	}
	id := strconv.FormatInt(chatID, 10)
	if !strings.HasPrefix(id, "-100") || messageID <= 0 {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(id, "-100"), messageID)
}
func audit(db *gorm.DB, d model.AnnouncementDelivery, action string) {
	if err := storage.WriteAuditLogInTx(db, d.ActorID, "CHANNEL_ANNOUNCEMENT_"+action, fmt.Sprintf("%d", d.ID), 0, fmt.Sprintf("key=%s target=%d state=%s message=%d", d.Key, d.TargetChatID, d.State, d.MessageID)); err != nil {
		log.Printf("announcement audit failed: %s", textfmt.FormatPlainError(err))
	}
}
func receipt(bot *tgbotapi.BotAPI, d model.AnnouncementDelivery, text string) {
	text = fmt.Sprintf("📣 公告 #%d：%s\n目标：%d", d.ID, text, d.TargetChatID)
	if d.ActorID != 0 {
		telegram.SendPlainText(bot, d.ActorID, text)
	} else {
		telegram.NotifySuperAdminsPlain(bot, text)
	}
}
func Recover(db *gorm.DB) error {
	// A send may already be visible. Edits are safe to retry, new sends are not.
	if err := db.Model(&model.AnnouncementDelivery{}).Where("state = ? AND parent_key = ?", "sending", "").Updates(map[string]any{"state": "uncertain", "last_error": "process restarted while sending; inspect channel before retry"}).Error; err != nil {
		return err
	}
	if err := db.Model(&model.AnnouncementDelivery{}).Where("state = ? AND parent_key <> ?", "sending", "").Update("state", "pending").Error; err != nil {
		return err
	}
	return db.Model(&model.AnnouncementDelivery{}).Where("reminder_state = ?", "sending").Update("reminder_state", "uncertain").Error
}
func definite(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 400 && apiErr.Code < 500
	}
	var apiValue tgbotapi.Error
	if errors.As(err, &apiValue) {
		return apiValue.Code >= 400 && apiValue.Code < 500
	}
	return false
}
func Dispatch(bot *tgbotapi.BotAPI) {
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	var rows []model.AnnouncementDelivery
	if err := storage.DB.Where("state IN ? AND attempts < ? AND retry_at <= ?", []string{"pending", "failed"}, 3, time.Now()).Where("parent_key = ? OR EXISTS (SELECT 1 FROM announcement_deliveries AS parent WHERE parent.key = announcement_deliveries.parent_key AND parent.state = ?)", "", "sent").Order("id asc").Limit(30).Find(&rows).Error; err != nil {
		log.Printf("announcement queue read: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, d := range rows {
		if d.Attempts < 3 {
			deliver(bot, &d)
		}
	}
	var reminders []model.AnnouncementDelivery
	if err := storage.DB.Where("state = ? AND reminder_state IN ? AND reminder_attempts < ? AND retry_at <= ?", "sent", []string{"pending", "failed"}, 3, time.Now()).Order("id asc").Limit(30).Find(&reminders).Error; err != nil {
		return
	}
	for _, d := range reminders {
		remind(bot, &d)
	}
}
func deliver(bot *tgbotapi.BotAPI, d *model.AnnouncementDelivery) {
	if d.Kind == "lottery_intro" {
		parts := strings.Split(d.Key, ":")
		if len(parts) != 3 {
			fail(bot, d, fmt.Errorf("invalid lottery announcement key"), true)
			return
		}
		var a model.LotteryActivity
		if err := storage.DB.First(&a, parts[1]).Error; err != nil {
			return
		}
		if a.Status != "active" && a.Status != "drawing" {
			if err := storage.DB.Model(d).Updates(map[string]any{"state": "superseded", "reminder_state": "none"}).Error; err == nil {
				audit(storage.DB, *d, "SUPERSEDED")
			}
			return
		}
	}

	if d.ParentKey != "" {
		var root model.AnnouncementDelivery
		if err := storage.DB.Where("key = ?", d.ParentKey).First(&root).Error; err != nil {
			return
		}
		if root.State != "sent" || root.MessageID == 0 {
			return
		}
		d.MessageID = root.MessageID
		if d.MarkupJSON == "" {
			d.MarkupJSON = root.MarkupJSON
		}
	}
	res := storage.DB.Model(d).Where("state IN ?", []string{"pending", "failed"}).Updates(map[string]any{"state": "sending", "attempts": gorm.Expr("attempts + 1"), "message_id": d.MessageID})
	if res.Error != nil || res.RowsAffected != 1 {
		return
	}
	d.State = "sending"
	d.Attempts++
	if d.Kind == "registration_closed" {
		d.MarkupJSON = `{"inline_keyboard":[]}`
	}
	if d.MarkupJSON == "" && bot.Self.UserName != "" {
		label, payload := "", ""
		if d.Kind == "lottery_intro" {
			parts := strings.Split(d.Key, ":")
			if len(parts) > 1 {
				label = "参与抽奖"
				payload = "lottery_" + parts[1]
			}
		}
		if d.Kind == "lottery_result" {
			label = "领取奖励"
			payload = "lottery_claim"
		}
		if d.Kind == "registration_open" {
			label = "打开 Bot 注册"
			payload = ""
		}
		if label != "" {
			link := "https://t.me/" + bot.Self.UserName
			if payload != "" {
				link += "?start=" + payload
			}
			data, err := json.Marshal(tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL(label, link))))
			if err != nil {
				fail(bot, d, err, true)
				return
			}
			d.MarkupJSON = string(data)
			if err = storage.DB.Model(d).Update("markup_json", d.MarkupJSON).Error; err != nil {
				return
			}
		}
	}
	var markup tgbotapi.InlineKeyboardMarkup
	if d.MarkupJSON != "" {
		if err := json.Unmarshal([]byte(d.MarkupJSON), &markup); err != nil {
			fail(bot, d, err, true)
			return
		}
	}
	var msg tgbotapi.Message
	var err error
	if d.ParentKey != "" {
		edit := tgbotapi.NewEditMessageText(d.TargetChatID, d.MessageID, d.Text)
		edit.ReplyMarkup = &markup
		msg, err = bot.Send(telegram.PrepareCopyable(edit))
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			err = nil
		}
	} else {
		send := tgbotapi.NewMessage(d.TargetChatID, d.Text)
		if d.MarkupJSON != "" {
			send.ReplyMarkup = markup
		}
		msg, err = telegram.SendNoAutoDelete(bot, send)
	}
	if err != nil {
		fail(bot, d, err, d.ParentKey != "" || definite(err))
		return
	}
	if d.ParentKey == "" {
		d.MessageID = msg.MessageID
	}
	if d.MessageID == 0 {
		fail(bot, d, fmt.Errorf("Telegram returned no message id"), false)
		return
	}
	if err = markSent(storage.DB, d); err != nil {
		// Persist the known message id if possible; never blindly send again.
		_ = storage.DB.Model(d).Updates(map[string]any{"state": "uncertain", "message_id": d.MessageID, "last_error": "sent but finalization failed"}).Error
		receipt(bot, *d, "已发送但登记失败，请人工核查，禁止直接重发。")
		return
	}
	receipt(bot, *d, "发布成功。")
	pinLottery(bot, *d)
}
func fail(bot *tgbotapi.BotAPI, d *model.AnnouncementDelivery, err error, safe bool) {
	d.State = "uncertain"
	if safe {
		d.State = "failed"
	}
	d.LastError = textfmt.FormatTelegramSendError(err)
	d.RetryAt = time.Now().Add(time.Minute)
	if e := storage.DB.Model(d).Updates(map[string]any{"state": d.State, "last_error": d.LastError, "retry_at": d.RetryAt}).Error; e != nil {
		log.Printf("announcement failure state: %s", textfmt.FormatPlainError(e))
	}
	audit(storage.DB, *d, "FAILED")
	receipt(bot, *d, fmt.Sprintf("发布失败（%s）。结果不确定时请人工检查；发送：%s 查看处置入口。", d.State, textfmt.Copyable("频道公告异常")))
}
func markSent(db *gorm.DB, d *model.AnnouncementDelivery) error {
	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(d).Updates(map[string]any{"state": "sent", "message_id": d.MessageID, "last_error": "", "retry_at": time.Time{}})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("announcement finalization missed")
		}
		if strings.HasPrefix(d.Key, "lottery:") {
			parts := strings.Split(d.Key, ":")
			id, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return err
			}
			field := ""
			if d.Kind == "lottery_intro" {
				field = "intro_message_id"
			}
			if d.Kind == "lottery_result" {
				field = "result_message_id"
			}
			if field != "" {
				if err := tx.Model(&model.LotteryActivity{}).Where("id = ?", id).Update(field, d.MessageID).Error; err != nil {
					return err
				}
			}
		}
		return storage.WriteAuditLogInTx(tx, d.ActorID, "CHANNEL_ANNOUNCEMENT_SENT", fmt.Sprint(d.ID), 0, fmt.Sprintf("target=%d message=%d key=%s", d.TargetChatID, d.MessageID, d.Key))
	})
}
func pinLottery(bot *tgbotapi.BotAPI, d model.AnnouncementDelivery) {
	if d.Kind != "lottery_intro" && d.Kind != "lottery_result" {
		return
	}
	field := "intro_pinned"
	if d.Kind == "lottery_result" {
		field = "result_pinned"
	}
	parts := strings.Split(d.Key, ":")
	if len(parts) < 2 {
		return
	}
	// Do not pin an ended activity after its pin cleanup deadline.
	var activity model.LotteryActivity
	if err := storage.DB.First(&activity, parts[1]).Error; err != nil {
		return
	}
	if activity.ClaimDeadlineAt != nil && !time.Now().Before(*activity.ClaimDeadlineAt) {
		return
	}
	_, err := bot.Request(tgbotapi.PinChatMessageConfig{ChatID: d.TargetChatID, MessageID: d.MessageID, DisableNotification: true})
	if err != nil {
		receipt(bot, d, "正文已发布，置顶失败，请检查频道置顶/编辑权限。")
		return
	}
	if err = storage.DB.Model(&activity).Update(field, true).Error; err != nil {
		log.Printf("announcement pin record: %s", textfmt.FormatPlainError(err))
	}
}
func remind(bot *tgbotapi.BotAPI, d *model.AnnouncementDelivery) {
	var newer int64
	if err := storage.DB.Model(&model.AnnouncementDelivery{}).Where("parent_key = ?", d.Key).Count(&newer).Error; err != nil {
		return
	}
	if newer > 0 {
		_ = storage.DB.Model(d).Update("reminder_state", "superseded").Error
		return
	}
	res := storage.DB.Model(d).Where("reminder_state IN ?", []string{"pending", "failed"}).Updates(map[string]any{"reminder_state": "sending", "reminder_attempts": gorm.Expr("reminder_attempts + 1")})
	if res.Error != nil || res.RowsAffected != 1 {
		return
	}
	chat, err := bot.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: d.TargetChatID}})
	if err != nil {
		failReminder(bot, d, err, true)
		return
	}
	link := MessageLink(d.TargetChatID, chat.UserName, d.MessageID)
	if link == "" {
		failReminder(bot, d, fmt.Errorf("cannot create channel message link"), true)
		return
	}
	title := strings.SplitN(d.Text, "\n", 2)[0]
	msg, err := telegram.SendAutoDelete(bot, tgbotapi.NewMessage(d.ReminderGroupID, title+"\n详情见频道："+link))
	if err != nil {
		failReminder(bot, d, err, definite(err))
		return
	}
	if err = storage.DB.Model(d).Updates(map[string]any{"reminder_state": "sent", "reminder_message_id": msg.MessageID, "last_error": ""}).Error; err != nil {
		log.Printf("reminder finalization: %s", textfmt.FormatPlainError(err))
		_ = storage.DB.Model(d).Updates(map[string]any{"reminder_state": "uncertain", "reminder_message_id": msg.MessageID, "last_error": "reminder sent but finalization failed"}).Error
		receipt(bot, *d, "群提醒可能已发送，但登记失败，请核查后通过频道公告异常入口处置，勿直接重发。")
	}
}
func failReminder(bot *tgbotapi.BotAPI, d *model.AnnouncementDelivery, err error, safe bool) {
	state := "uncertain"
	if safe {
		state = "failed"
	}
	if e := storage.DB.Model(d).Updates(map[string]any{"reminder_state": state, "retry_at": time.Now().Add(time.Minute), "last_error": textfmt.FormatTelegramSendError(err)}).Error; e != nil {
		log.Printf("reminder failure persistence: %s", textfmt.FormatPlainError(e))
	}
	audit(storage.DB, *d, "REMINDER_FAILED")
	receipt(bot, *d, "频道正文已发布，群提醒失败，可通过频道公告异常入口单独处置提醒。")
}

// Resolve requires an explicit, separately confirmed administrator decision.
func Resolve(bot *tgbotapi.BotAPI, id uint, action string, messageID int, actor int64) error {
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	var d model.AnnouncementDelivery
	if err := storage.DB.First(&d, id).Error; err != nil {
		return err
	}
	switch action {
	case "频道公告已发":
		if d.State != "uncertain" || messageID <= 0 {
			return fmt.Errorf("仅可确认发送结果不确定的公告，且需有效消息 ID")
		}
		d.MessageID = messageID
		if err := markSent(storage.DB, &d); err != nil {
			return err
		}
		pinLottery(bot, d)
	case "频道公告未发", "频道公告重试":
		if (action == "频道公告未发" && d.State != "uncertain") || (action == "频道公告重试" && d.State != "failed") {
			return fmt.Errorf("公告状态不允许该处置")
		}
		if err := storage.DB.Model(&d).Updates(map[string]any{"state": "pending", "attempts": 0, "retry_at": time.Time{}, "last_error": ""}).Error; err != nil {
			return err
		}
	case "频道提醒已发":
		if d.State != "sent" || (d.ReminderState != "uncertain" && d.ReminderState != "failed") {
			return fmt.Errorf("提醒状态不允许该处置")
		}
		if err := storage.DB.Model(&d).Update("reminder_state", "sent").Error; err != nil {
			return err
		}
	case "频道提醒重试":
		if d.State != "sent" || (d.ReminderState != "uncertain" && d.ReminderState != "failed") {
			return fmt.Errorf("请先确认频道正文已发送及群提醒确实未发")
		}
		if err := storage.DB.Model(&d).Updates(map[string]any{"reminder_state": "pending", "reminder_attempts": 0, "retry_at": time.Time{}}).Error; err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知处置动作")
	}
	return storage.WriteAuditLogInTx(storage.DB, actor, "CHANNEL_ANNOUNCEMENT_RESOLVE", fmt.Sprint(id), 0, fmt.Sprintf("action=%s target=%d message=%d", action, d.TargetChatID, messageID))
}

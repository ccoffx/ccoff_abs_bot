package app

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/announcements"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func StartChannelAnnouncementDispatcher(bot *tgbotapi.BotAPI) {
	if config.AppConfig.IsTest() {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			reconcileChannelAnnouncements(bot)
			announcements.Dispatch(bot)
			<-ticker.C
		}
	}()
}
func registrationAnnouncement(c model.OpenRegistrationCampaign, closed bool, botName string) string {
	title := "🚪 开放注册已开启"
	status := "当前符合条件的用户无需邀请码，可私聊 Bot 注册。"
	if closed {
		title = "🚪 本轮开放注册已结束"
		status = "本轮临时免邀请码入口已结束，请以 Bot 当前注册条件为准。"
	}
	rule := fmt.Sprintf("本轮总名额：%d", c.Quota)
	if c.Mode == openRegistrationModeDuration && c.EndsAt != nil {
		rule = "截止时间：" + c.EndsAt.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05") + "（北京时间）"
	}
	out := title + "\n\n" + rule + "\n" + status
	if botName != "" {
		out += "\nhttps://t.me/" + botName
	}
	return out
}
func compensationAnnouncement(c model.CompensationCampaign, done bool) string {
	status := "正在逐人发放，请留意 Bot 私聊到账通知。"
	if done {
		status = "本轮补偿已处理；个人到账与异常情况请以 Bot 私聊及管理员核实为准。"
	}
	return fmt.Sprintf("🛡 服务补偿通知\n\n%s\n\n补偿标准：每人 %d 积分、%d 天听书有效期（按账号适用条件发放）。\n%s", c.Announcement, c.Points, c.Days, status)
}
func reconcileChannelAnnouncements(bot *tgbotapi.BotAPI) {
	if storage.DB == nil || config.AppConfig.IsTest() {
		return
	}
	if err := closeExpiredOpenRegistrationInTx(storage.DB, time.Now()); err != nil {
		log.Printf("registration expiry: %s", textfmt.FormatPlainError(err))
		return
	}
	var campaigns []model.OpenRegistrationCampaign
	if err := storage.DB.Where("announcement_channel_id <> 0 AND NOT EXISTS (SELECT 1 FROM announcement_deliveries AS delivery WHERE delivery.key = open_registration_campaigns.campaign_id || ?)", ":announcement:closed").Find(&campaigns).Error; err != nil {
		log.Printf("registration announcement scan: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, c := range campaigns {
		root := c.CampaignID + ":announcement"
		if err := announcements.Queue(storage.DB, model.AnnouncementDelivery{Key: root, Kind: "registration_open", ActorID: c.CreatedBy, TargetChatID: c.AnnouncementChannelID, ReminderGroupID: c.AnnouncementGroupID, Text: registrationAnnouncement(c, false, bot.Self.UserName)}); err != nil {
			continue
		}
		closed := c.Status == openRegistrationStatusClosed || (c.EndsAt != nil && !time.Now().Before(*c.EndsAt))
		if !closed && c.Mode == openRegistrationModeQuota {
			var completed int64
			if err := storage.DB.Model(&model.OpenRegistrationReservation{}).Where("campaign_id = ? AND status = ?", c.CampaignID, openRegistrationReservationCompleted).Count(&completed).Error; err != nil {
				continue
			}
			// In-flight reservations may still fail and release slots, so do not
			// announce exhaustion until the full quota has successfully registered.
			closed = completed >= int64(c.Quota)
		}
		if closed {
			// If a round ended while the Bot was offline, publish its final status
			// rather than an opening notification for an already-closed registration.
			if err := storage.DB.Model(&model.AnnouncementDelivery{}).Where("key = ? AND state = ? AND attempts = ?", root, "pending", 0).Updates(map[string]any{"text": registrationAnnouncement(c, true, bot.Self.UserName), "kind": "registration_closed"}).Error; err != nil {
				continue
			}
			if err := announcements.QueueEdit(storage.DB, root+":closed", root, "registration_closed", registrationAnnouncement(c, true, bot.Self.UserName), c.CreatedBy, true); err != nil {
				log.Printf("registration closure announcement: %s", textfmt.FormatPlainError(err))
			}
		}
	}
	var compensations []model.CompensationCampaign
	if err := storage.DB.Where("announcement_channel_id <> 0 AND NOT EXISTS (SELECT 1 FROM announcement_deliveries AS delivery WHERE delivery.key = compensation_campaigns.campaign_id || ?)", ":announcement:done").Find(&compensations).Error; err != nil {
		return
	}
	for _, c := range compensations {
		root := c.CampaignID + ":announcement"
		if err := announcements.Queue(storage.DB, model.AnnouncementDelivery{Key: root, Kind: "compensation", ActorID: c.CreatedBy, TargetChatID: c.AnnouncementChannelID, Text: compensationAnnouncement(c, false)}); err != nil {
			continue
		}
		if c.Status == compensationStatusCompleted {
			if err := storage.DB.Model(&model.AnnouncementDelivery{}).Where("key = ? AND state = ? AND attempts = ?", root, "pending", 0).Update("text", compensationAnnouncement(c, true)).Error; err != nil {
				continue
			}

			_ = announcements.QueueEdit(storage.DB, root+":done", root, "compensation_done", compensationAnnouncement(c, true), c.CreatedBy, false)
		}
	}
}
func HandleChannelAnnouncementCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	session := telegram.GetSession(msg.From.ID)
	step := session.GetStep()
	command := text == "发布服务公告" || text == "频道公告异常" || strings.HasPrefix(text, "频道公告重试 ") || strings.HasPrefix(text, "频道公告已发 ") || strings.HasPrefix(text, "频道公告未发 ") || strings.HasPrefix(text, "频道提醒重试 ") || strings.HasPrefix(text, "频道提醒已发 ")
	if !command && !strings.HasPrefix(step, "CHANNEL_ANN_") {
		return false
	}
	if !msg.Chat.IsPrivate() || !storage.IsSuperAdmin(msg.From.ID) {
		telegram.SendPlainText(bot, msg.Chat.ID, "该功能仅限超级管理员在私聊中使用。")
		return true
	}
	reply := func(s string) { telegram.SendPlainText(bot, msg.Chat.ID, s) }
	if text == "频道公告异常" {
		var rows []model.AnnouncementDelivery
		if err := storage.DB.Where("state IN ? OR reminder_state IN ?", []string{"uncertain", "failed"}, []string{"uncertain", "failed"}).Order("id desc").Limit(20).Find(&rows).Error; err != nil {
			reply("读取公告状态失败。")
			return true
		}
		out := "频道公告异常\n"
		for _, d := range rows {
			out += fmt.Sprintf("\n#%d 目标 %d 正文 %s 提醒 %s 消息 %d\n", d.ID, d.TargetChatID, d.State, d.ReminderState, d.MessageID)
		}
		out += "\n请先人工核查对应目标。\n" + textfmt.Copyable("频道公告已发 ID 消息ID") + "\n" + textfmt.Copyable("频道公告未发 ID") + "\n" + textfmt.Copyable("频道公告重试 ID") + "\n" + textfmt.Copyable("频道提醒已发 ID") + "\n" + textfmt.Copyable("频道提醒重试 ID")
		reply(out)
		return true
	}
	if command && text != "发布服务公告" {
		fields := strings.Fields(text)
		if len(fields) < 2 || len(fields) > 3 {
			reply("格式错误，请查看频道公告异常。")
			return true
		}
		id, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil || id == 0 {
			reply("公告 ID 无效。")
			return true
		}
		message := 0
		if len(fields) == 3 {
			message, err = strconv.Atoi(fields[2])
			if err != nil || message < 1 {
				reply("消息 ID 无效。")
				return true
			}
		}
		if fields[0] == "频道公告已发" && message == 0 {
			reply("请填写频道中实际存在的消息 ID。")
			return true
		}
		session.SetTemp("channel_recovery_id", fmt.Sprint(id))
		session.SetTemp("channel_recovery_action", fields[0])
		session.SetTemp("channel_recovery_message", strconv.Itoa(message))
		session.SetStep("CHANNEL_ANN_RECOVERY")
		reply("请人工核对公告目标及实际发送情况，再回复：" + textfmt.Copyable("确认频道公告处置"))
		return true
	}
	if step == "CHANNEL_ANN_RECOVERY" {
		if text != "确认频道公告处置" {
			reply("请回复确认频道公告处置，或取消。")
			return true
		}
		id, _ := strconv.ParseUint(session.GetTemp("channel_recovery_id"), 10, 32)
		message, _ := strconv.Atoi(session.GetTemp("channel_recovery_message"))
		err := announcements.Resolve(bot, uint(id), session.GetTemp("channel_recovery_action"), message, msg.From.ID)
		if err != nil {
			reply("处置失败：" + textfmt.FormatPlainError(err))
		} else {
			reply("处置已记录。待发送内容将由后台处理。")
		}
		telegram.ClearSession(msg.From.ID)
		return true
	}
	if text == "发布服务公告" {
		if config.AppConfig.AnnouncementChannelID == 0 {
			reply("请先配置 ANNOUNCEMENT_CHANNEL_ID。")
			return true
		}
		session.SetTemp("channel_target", fmt.Sprint(config.AppConfig.AnnouncementChannelID))
		session.SetTemp("channel_group", fmt.Sprint(config.AppConfig.NoticeGroupID))
		session.SetStep("CHANNEL_ANN_KIND")
		reply("请选择公告类型：" + textfmt.Copyable("维护") + " 或 " + textfmt.Copyable("恢复"))
		return true
	}
	switch step {
	case "CHANNEL_ANN_KIND":
		if text != "维护" && text != "恢复" {
			reply("请选择维护或恢复。")
			return true
		}
		session.SetTemp("channel_kind", text)
		session.SetStep("CHANNEL_ANN_BODY")
		reply("请输入公开公告正文（1–2000 字），不要包含账号、卡密或内部凭据。")
		return true
	case "CHANNEL_ANN_BODY":
		if strings.TrimSpace(text) == "" || len([]rune(text)) > 2000 {
			reply("正文须为 1–2000 字。")
			return true
		}
		session.SetTemp("channel_body", "📣 服务"+session.GetTemp("channel_kind")+"通知\n\n"+text)
		session.SetTemp("channel_key", "service:"+security.GenerateRandomCode(24))
		session.SetStep("CHANNEL_ANN_CONFIRM")
		reply("发布预览\n目标：" + session.GetTemp("channel_target") + "\n\n" + session.GetTemp("channel_body") + "\n\n只发布通知，不修改服务状态。\n回复：" + textfmt.Copyable("确认发布服务公告"))
		return true
	case "CHANNEL_ANN_CONFIRM":
		if text != "确认发布服务公告" {
			reply("请回复确认发布服务公告，或取消。")
			return true
		}
		target, _ := strconv.ParseInt(session.GetTemp("channel_target"), 10, 64)
		group, _ := strconv.ParseInt(session.GetTemp("channel_group"), 10, 64)
		d := model.AnnouncementDelivery{Key: session.GetTemp("channel_key"), Kind: "service", TargetChatID: target, ReminderGroupID: group, ActorID: msg.From.ID, Text: session.GetTemp("channel_body")}
		if err := announcements.Queue(storage.DB, d); err != nil {
			reply("公告登记失败，请稍后重试。")
			return true
		}
		reply("公告已进入发布队列，将单独回执发布结果。")
		telegram.ClearSession(msg.From.ID)
		return true
	}
	return true
}

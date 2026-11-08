package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	"ccoff-abs-bot/internal/titles"
)

const groupAdminCommand = "群管理员管理"
const groupAdminPrefix = "groupadmin:"
const groupAdminInputStep = "WAITING_GROUP_ADMIN_ID"
const groupAdminSelectStep = "WAITING_GROUP_ADMIN_RIGHTS"
const groupAdminConfirmStep = "WAITING_GROUP_ADMIN_CONFIRM"

var groupAdminMutationMutex sync.Mutex

func isGroupAdminStep(step string) bool {
	return step == groupAdminInputStep || step == groupAdminSelectStep || step == groupAdminConfirmStep
}

func clearGroupAdminSession(userID int64) {
	if isGroupAdminStep(telegram.GetSession(userID).GetStep()) {
		telegram.ClearSession(userID)
	}
}

func isGroupAdminNavigation(text string) bool {
	for _, menu := range []tgbotapi.ReplyKeyboardMarkup{telegram.SuperAdminMenu, telegram.NormalAdminMenu, telegram.UserMainMenu} {
		for _, row := range menu.Keyboard {
			for _, button := range row {
				if text == button.Text {
					return true
				}
			}
		}
	}
	return text == "聚宝斋"
}

// Use raw Bot API fields: the Telegram SDK predates tags, stories and topics.
var groupAdminPermissions = []struct{ field, label string }{
	{"can_delete_messages", "删除消息"},
	{"can_restrict_members", "封禁与限制成员"},
	{"can_invite_users", "邀请用户"},
	{"can_pin_messages", "置顶消息"},
	{"can_change_info", "修改群资料"},
	{"can_manage_video_chats", "管理视频聊天"},
	{"can_manage_topics", "管理话题"},
	{"can_manage_tags", "管理成员标签"},
	{"can_send_welcome_messages", "管理欢迎消息"},
	{"can_post_stories", "发布动态"},
	{"can_edit_stories", "编辑动态"},
	{"can_delete_stories", "删除动态"},
	{"can_promote_members", "添加管理员"},
	{"is_anonymous", "匿名管理"},
}

type groupAdminMember struct {
	User        *tgbotapi.User  `json:"user"`
	Status      string          `json:"status"`
	CanBeEdited bool            `json:"can_be_edited"`
	Rights      map[string]bool `json:"-"`
}

type groupAdminDraft struct {
	GroupID   int64
	GroupName string
	UserID    int64
	UserName  string
	Snapshot  string
	IntentID  uint
	Rights    map[string]bool
	Available map[string]bool
	Nonce     string
	ExpiresAt time.Time
}

func getGroupAdminMember(bot *tgbotapi.BotAPI, groupID, userID int64) (groupAdminMember, error) {
	var member groupAdminMember
	response, err := bot.MakeRequest("getChatMember", tgbotapi.Params{"chat_id": strconv.FormatInt(groupID, 10), "user_id": strconv.FormatInt(userID, 10)})
	if err != nil {
		return member, err
	}
	if response == nil || !response.Ok {
		return member, errors.New("无法读取群成员状态")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Result, &raw); err != nil {
		return member, err
	}
	if err := json.Unmarshal(response.Result, &member); err != nil {
		return member, err
	}
	if member.User == nil || member.User.ID != userID || member.Status == "" {
		return member, errors.New("群成员状态不完整，请重试")
	}
	member.Rights = make(map[string]bool)
	for field, value := range raw {
		if strings.HasPrefix(field, "can_") && field != "can_be_edited" || field == "is_anonymous" {
			var enabled bool
			if err := json.Unmarshal(value, &enabled); err != nil {
				return member, err
			}
			if enabled {
				member.Rights[field] = true
			}
		}
	}
	return member, nil
}

func groupAdminSnapshot(member groupAdminMember) string {
	// Names, titles and tags may change during selection without affecting rights.
	data, _ := json.Marshal(struct {
		Status   string
		Editable bool
		IsBot    bool
		Rights   map[string]bool
	}{member.Status, member.CanBeEdited, member.User.IsBot, member.Rights})
	return string(data)
}

func checkGroupAdminTarget(member groupAdminMember) error {
	if member.User.IsBot {
		return errors.New("此入口仅任命普通用户，不能修改机器人权限")
	}
	switch member.Status {
	case "member":
		return nil
	case "administrator":
		if member.CanBeEdited {
			return nil
		}
		return errors.New("Bot 无权编辑这位管理员。请群主先手动撤销其管理员身份，再从此入口重新任命")
	case "creator":
		return errors.New("目标是群主，Bot 不能修改群主权限或头衔")
	default:
		return errors.New("目标必须是群内未受限的普通成员，或 Bot 有权编辑的管理员")
	}
}

func groupAdminBotRights(bot *tgbotapi.BotAPI, groupID int64) (map[string]bool, error) {
	member, err := getGroupAdminMember(bot, groupID, bot.Self.ID)
	if err != nil {
		return nil, err
	}
	if member.Status != "administrator" || !member.Rights["can_promote_members"] {
		return nil, errors.New("请群主先给 Bot 开启「添加管理员」权限")
	}
	return member.Rights, nil
}

func groupAdminAllowed(rights map[string]bool, field string) bool {
	// Anonymous presence is a display setting, not a delegable capability.
	return field == "is_anonymous" || rights[field]
}

func groupAdminIntentID(groupID, userID int64) (uint, error) {
	if storage.DB == nil {
		return 0, errors.New("无法读取任命操作记录")
	}
	var intent model.AuditLog
	err := storage.DB.Select("id").Where("target = ? AND action = ?",
		fmt.Sprintf("group:%d/user:%d", groupID, userID), "GROUP_ADMIN_PROMOTE_INTENT").Order("id DESC").Limit(1).Find(&intent).Error
	return intent.ID, err
}

func prepareGroupAdminDraft(bot *tgbotapi.BotAPI, userID int64) (*groupAdminDraft, error) {
	if config.AppConfig == nil || config.AppConfig.NoticeGroupID >= 0 {
		return nil, errors.New("尚未配置交流群")
	}
	groupID := config.AppConfig.NoticeGroupID
	// Wait for any submitted operation before capturing its revision. Any
	// subsequent attempt invalidates this draft even if Telegram's result is
	// uncertain and getChatMember still returns the original permissions.
	groupAdminMutationMutex.Lock()
	intentID, err := groupAdminIntentID(groupID, userID)
	groupAdminMutationMutex.Unlock()
	if err != nil {
		return nil, errors.New("无法读取任命操作记录，请稍后重试")
	}
	response, err := bot.MakeRequest("getChat", tgbotapi.Params{"chat_id": strconv.FormatInt(groupID, 10)})
	if err != nil {
		return nil, err
	}
	var chat tgbotapi.Chat
	if response == nil || !response.Ok {
		return nil, errors.New("无法读取交流群")
	}
	if err := json.Unmarshal(response.Result, &chat); err != nil {
		return nil, err
	}
	if chat.ID != groupID || chat.Type != "supergroup" {
		return nil, errors.New("此功能仅用于配置的超级群，普通群请先升级为超级群")
	}
	available, err := groupAdminBotRights(bot, groupID)
	if err != nil {
		return nil, err
	}
	member, err := getGroupAdminMember(bot, groupID, userID)
	if err != nil {
		return nil, err
	}
	if err := checkGroupAdminTarget(member); err != nil {
		return nil, err
	}
	// Reject unknown enabled capabilities rather than silently stripping them.
	known := map[string]bool{"can_manage_chat": true}
	for _, permission := range groupAdminPermissions {
		known[permission.field] = true
	}
	for field := range member.Rights {
		if member.Status == "administrator" && !known[field] {
			return nil, fmt.Errorf("目标存在暂不支持的权限 %s，请在 Telegram 客户端管理", field)
		}
	}
	rights := map[string]bool{"can_manage_chat": true}
	for _, permission := range groupAdminPermissions {
		rights[permission.field] = member.Status == "administrator" && member.Rights[permission.field]
	}
	return &groupAdminDraft{
		GroupID: groupID, GroupName: chat.Title, UserID: userID,
		UserName: strings.TrimSpace(member.User.FirstName + " " + member.User.LastName),
		Snapshot: groupAdminSnapshot(member), IntentID: intentID, Rights: rights, Available: available,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

func saveGroupAdminDraft(session *telegram.SessionState, draft *groupAdminDraft) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	draft.Nonce = hex.EncodeToString(nonce[:])
	encoded, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	session.SetTemp("group_admin_draft", string(encoded))
	return nil
}

func sendGroupAdminPanel(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string, rows [][]tgbotapi.InlineKeyboardButton) error {
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if messageID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		edit.ReplyMarkup = &markup
		if _, err := bot.Request(edit); err == nil || telegram.IsTelegramMessageNotModifiedError(err) {
			return nil
		} else if !telegram.IsTelegramMessageUnavailableForEdit(err) {
			return err
		}
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = markup
	_, err := telegram.SendAutoDelete(bot, msg)
	return err
}

func showGroupAdminDraft(bot *tgbotapi.BotAPI, actorID int64, messageID int, draft *groupAdminDraft, confirm bool) {
	session := telegram.GetSession(actorID)
	if err := saveGroupAdminDraft(session, draft); err != nil {
		telegram.ClearSession(actorID)
		telegram.SendPlainText(bot, actorID, "无法创建操作凭据，请重新打开群管理员管理。")
		return
	}
	button := func(label, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(label, groupAdminPrefix+draft.Nonce+":"+action)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🛡 群管理员管理\n群：%s（%d）\n用户：%s（TGID %d）\n\n", textfmt.LotteryDisplayText(draft.GroupName, 40, "交流群"), draft.GroupID, textfmt.LotteryDisplayText(draft.UserName, 40, "用户"), draft.UserID)
	b.WriteString("✅ 基础管理（必选，保留管理员身份）\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for index, permission := range groupAdminPermissions {
		mark := "⬜"
		if draft.Rights[permission.field] {
			mark = "✅"
		}
		label := mark + " " + permission.label
		if !groupAdminAllowed(draft.Available, permission.field) {
			label += "（Bot 无此权限）"
		}
		if confirm {
			fmt.Fprintln(&b, label)
		} else {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(button(label, "toggle:"+strconv.Itoa(index))))
		}
	}
	if confirm {
		session.SetStep(groupAdminConfirmStep)
		b.WriteString("\n确认后按以上权限任命或更新。此次操作只管理 Telegram 群权限，不授予机器人后台管理权限。")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(button("确认任命", "apply"), button("返回选择权限", "edit")))
	} else {
		session.SetStep(groupAdminSelectStep)
		b.WriteString("点击权限切换勾选；新任命默认仅开启基础管理，现有可编辑管理员默认沿用当前权限。\n仅开启基础管理也可用于称号自动同步。")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(button("查看任命预览", "preview")))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(button("取消", "cancel")))
	if err := sendGroupAdminPanel(bot, actorID, messageID, b.String(), rows); err != nil {
		// An unseen or uncertain preview must not leave a live confirmation.
		telegram.ClearSession(actorID)
		log.Printf("发送群管理员面板失败，已撤销本次确认: %s", textfmt.FormatTelegramSendError(err))
	}
}

func handleGroupAdminMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return false
	}
	session := telegram.GetSession(msg.From.ID)
	step := session.GetStep()
	if text != groupAdminCommand && !isGroupAdminStep(step) {
		return false
	}
	if !msg.Chat.IsPrivate() || msg.Chat.ID != msg.From.ID || !storage.IsSuperAdmin(msg.From.ID) {
		if msg.Chat.IsPrivate() {
			telegram.ClearSession(msg.From.ID)
		}
		telegram.SendPlainText(bot, msg.Chat.ID, "群管理员管理仅限超级管理员在本人私聊中使用。")
		return true
	}
	if step == groupAdminSelectStep || step == groupAdminConfirmStep {
		var draft groupAdminDraft
		if json.Unmarshal([]byte(session.GetTemp("group_admin_draft")), &draft) != nil || !groupAdminDraftCurrent(&draft) {
			telegram.ClearSession(msg.From.ID)
			if text != groupAdminCommand {
				return false // An expired wizard must not intercept other commands.
			}
		}
	}
	if isGroupAdminStep(step) && isGroupAdminNavigation(text) {
		telegram.ClearSession(msg.From.ID)
		return false // Let the ordinary router apply its gates and open the menu.
	}
	if text == "取消" || strings.Contains(text, "返回") {
		telegram.ClearSession(msg.From.ID)
		telegram.SendPlainText(bot, msg.Chat.ID, "已取消群管理员操作。")
		return true
	}
	if text == groupAdminCommand {
		telegram.ClearSession(msg.From.ID)
		telegram.GetSession(msg.From.ID).SetStep(groupAdminInputStep)
		telegram.SendPlainText(bot, msg.Chat.ID, "请输入要任命或调整群权限的 Telegram 数字 ID（TGID）。目标需已加入交流群。\nBot 需要「添加管理员」权限；群主和 Bot 无权编辑的管理员无法在此修改。发送「取消」退出。")
		return true
	}
	if step != groupAdminInputStep {
		telegram.SendPlainText(bot, msg.Chat.ID, "请使用权限面板按钮操作，或发送「取消」。")
		return true
	}
	userID, err := strconv.ParseInt(text, 10, 64)
	if err != nil || userID <= 0 || userID > (1<<52)-1 {
		telegram.SendPlainText(bot, msg.Chat.ID, "请输入有效的 Telegram 数字 ID。")
		return true
	}
	draft, err := prepareGroupAdminDraft(bot, userID)
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, "无法准备任命："+textfmt.FormatTelegramSendError(err))
		return true
	}
	showGroupAdminDraft(bot, msg.From.ID, 0, draft, false)
	return true
}

func handleGroupAdminCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, groupAdminPrefix) {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || !cb.Message.Chat.IsPrivate() || cb.Message.Chat.ID != cb.From.ID {
		telegram.AnswerCallback(bot, cb.ID, "仅限超级管理员在本人私聊中操作。")
		return true
	}
	if !storage.IsSuperAdmin(cb.From.ID) {
		clearGroupAdminSession(cb.From.ID)
		telegram.AnswerCallback(bot, cb.ID, "仅限超级管理员在本人私聊中操作。")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(cb.Data, groupAdminPrefix), ":")
	session := telegram.GetSession(cb.From.ID)
	step := session.GetStep()
	var draft groupAdminDraft
	if len(parts) < 2 || json.Unmarshal([]byte(session.GetTemp("group_admin_draft")), &draft) != nil || draft.Nonce == "" || parts[0] != draft.Nonce ||
		(step != groupAdminSelectStep && step != groupAdminConfirmStep) {
		telegram.AnswerCallback(bot, cb.ID, "面板已失效，请重新打开群管理员管理。")
		return true
	}
	if !groupAdminDraftCurrent(&draft) {
		// Only a callback matching the current draft may clear it. A stale
		// button from another panel must never cancel a newer operation.
		telegram.ClearSession(cb.From.ID)
		telegram.AnswerCallback(bot, cb.ID, "面板已失效，请重新打开群管理员管理。")
		return true
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		telegram.ClearSession(cb.From.ID)
		telegram.AnswerCallback(bot, cb.ID, "已取消")
		return true
	}
	switch {
	case len(parts) == 3 && parts[1] == "toggle" && step == groupAdminSelectStep:
		index, err := strconv.Atoi(parts[2])
		if err != nil || index < 0 || index >= len(groupAdminPermissions) {
			telegram.AnswerCallback(bot, cb.ID, "无效权限。")
			return true
		}
		field := groupAdminPermissions[index].field
		if !draft.Rights[field] && !groupAdminAllowed(draft.Available, field) {
			telegram.AnswerCallback(bot, cb.ID, "Bot 无此权限，不能授予目标用户。")
			return true
		}
		draft.Rights[field] = !draft.Rights[field]
		telegram.AnswerCallback(bot, cb.ID, "已选择，尚未提交")
		showGroupAdminDraft(bot, cb.From.ID, cb.Message.MessageID, &draft, false)
	case len(parts) == 2 && parts[1] == "preview" && step == groupAdminSelectStep:
		telegram.AnswerCallback(bot, cb.ID, "请核对权限并确认")
		showGroupAdminDraft(bot, cb.From.ID, cb.Message.MessageID, &draft, true)
	case len(parts) == 2 && parts[1] == "edit" && step == groupAdminConfirmStep:
		telegram.AnswerCallback(bot, cb.ID, "请重新选择权限")
		showGroupAdminDraft(bot, cb.From.ID, cb.Message.MessageID, &draft, false)
	case len(parts) == 2 && parts[1] == "apply" && step == groupAdminConfirmStep:
		telegram.AnswerCallback(bot, cb.ID, "正在核对并提交任命")
		// Consume confirmation before external I/O; duplicate clicks and restart
		// cannot replay a request whose result might be uncertain.
		telegram.ClearSession(cb.From.ID)
		result := applyGroupAdminDraft(bot, cb.From.ID, &draft)
		telegram.SendPlainText(bot, cb.From.ID, result)
	default:
		telegram.AnswerCallback(bot, cb.ID, "请按面板步骤操作。")
	}
	return true
}

func applyGroupAdminDraft(bot *tgbotapi.BotAPI, actorID int64, draft *groupAdminDraft) string {
	// Serialize different operators, too. A second preview must observe the
	// first operator's completed update before changing the same user's rights.
	groupAdminMutationMutex.Lock()
	defer groupAdminMutationMutex.Unlock()
	if !groupAdminDraftAuthorized(actorID, draft) {
		return "任命未执行：权限、目标群或操作期限已变化，请重新打开面板。"
	}
	intentID, err := groupAdminIntentID(draft.GroupID, draft.UserID)
	if err != nil {
		return "任命未执行：无法读取操作记录，请稍后重试。"
	}
	if intentID != draft.IntentID {
		return "任命未执行：此预览之后已有其他任命尝试，请先核对群管理员列表，再重新打开面板。"
	}
	available, err := groupAdminBotRights(bot, draft.GroupID)
	if err != nil {
		return "任命未执行：" + textfmt.FormatTelegramSendError(err)
	}
	member, err := getGroupAdminMember(bot, draft.GroupID, draft.UserID)
	if err != nil {
		return "任命未执行，无法重新核对成员：" + textfmt.FormatTelegramSendError(err)
	}
	if err := checkGroupAdminTarget(member); err != nil {
		return "任命未执行：" + err.Error()
	}
	if groupAdminSnapshot(member) != draft.Snapshot {
		return "任命未执行：目标成员身份或权限已变化，请重新输入 TGID 并核对。"
	}
	params := tgbotapi.Params{"chat_id": strconv.FormatInt(draft.GroupID, 10), "user_id": strconv.FormatInt(draft.UserID, 10), "can_manage_chat": "true"}
	for _, permission := range groupAdminPermissions {
		enabled := draft.Rights[permission.field]
		if enabled && !groupAdminAllowed(available, permission.field) {
			return "任命未执行：Bot 当前无权授予「" + permission.label + "」，请重新选择权限。"
		}
		params[permission.field] = strconv.FormatBool(enabled)
	}
	// Explicitly disable channel-only defaults; this workflow never targets a channel.
	params["can_post_messages"], params["can_edit_messages"], params["can_manage_direct_messages"] = "false", "false", "false"
	rightsJSON, _ := json.Marshal(draft.Rights)
	target := fmt.Sprintf("group:%d/user:%d", draft.GroupID, draft.UserID)
	detail := fmt.Sprintf("operation=%s previous=%s requested=%s", draft.Nonce, draft.Snapshot, rightsJSON)
	if err := storage.WriteAuditLogInTx(storage.DB, actorID, "GROUP_ADMIN_PROMOTE_INTENT", target, 0, detail); err != nil {
		log.Printf("群管理员任命审计失败，已阻止提交: %s", textfmt.FormatPlainError(err))
		return "任命未执行：无法保存操作记录，请稍后重试。"
	}
	// Network preflight and the audit write may take time. Check local authority
	// and expiry again at the last point before changing Telegram permissions.
	if !groupAdminDraftAuthorized(actorID, draft) {
		if err := storage.WriteAuditLogInTx(storage.DB, actorID, "GROUP_ADMIN_PROMOTE_ABORTED", target, 0, detail); err != nil {
			log.Printf("记录群管理员任命中止失败: %s", textfmt.FormatPlainError(err))
		}
		return "任命未执行：权限、目标群或操作期限已变化，请重新打开面板。"
	}
	response, requestErr := bot.MakeRequest("promoteChatMember", params)
	var success bool
	if requestErr == nil && response != nil && response.Ok {
		_ = json.Unmarshal(response.Result, &success)
	}
	action, result := "GROUP_ADMIN_PROMOTE_UNCERTAIN", "任命结果尚不确定，请先在 Telegram 群管理员列表核对；本次不会自动重试。"
	if success {
		action = "GROUP_ADMIN_PROMOTE_OK"
		result = fmt.Sprintf("✅ 已按所选权限任命/更新 TGID %d 为群管理员。未授予机器人后台管理权限。", draft.UserID)
	} else if requestErr != nil {
		var apiErr *tgbotapi.Error
		var apiValue tgbotapi.Error
		if errors.As(requestErr, &apiErr) || errors.As(requestErr, &apiValue) {
			if apiErr == nil {
				apiErr = &apiValue
			}
			if apiErr.Code >= 400 && apiErr.Code < 500 {
				action = "GROUP_ADMIN_PROMOTE_FAILED"
				result = "Telegram 拒绝了任命：" + textfmt.FormatTelegramSendError(requestErr) + "。请处理后重新打开面板。"
			}
		}
	}
	if err := storage.WriteAuditLogInTx(storage.DB, actorID, action, target, 0, detail); err != nil {
		log.Printf("群管理员任命结果审计失败: operation=%s err=%s", draft.Nonce, textfmt.FormatPlainError(err))
		result += "\n操作结果记录保存失败，请人工核对群管理员列表。"
	}
	if success {
		// Queue title synchronization even if Telegram's chat_member event is delayed.
		if err := titles.RequestMemberSync(draft.GroupID, draft.UserID, draft.GroupName); err != nil {
			log.Printf("任命后称号重同步入队失败: %s", textfmt.FormatPlainError(err))
			result += fmt.Sprintf("\n称号同步暂未入队，请 TGID %d 对应的用户在「我的称号」重新点击佩戴。", draft.UserID)
		}
	}
	return result
}

func groupAdminDraftAuthorized(actorID int64, draft *groupAdminDraft) bool {
	return storage.IsSuperAdmin(actorID) && groupAdminDraftCurrent(draft)
}

func groupAdminDraftCurrent(draft *groupAdminDraft) bool {
	return config.AppConfig != nil &&
		config.AppConfig.NoticeGroupID == draft.GroupID && !time.Now().After(draft.ExpiresAt)
}

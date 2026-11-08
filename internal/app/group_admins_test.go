package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

const groupAdminTestTarget int64 = 6254916849

type groupAdminFixture struct {
	mu                sync.Mutex
	bot               *tgbotapi.BotAPI
	botMember         map[string]any
	target            map[string]any
	chatType          string
	lookupError       bool
	promoteCode       int
	editCode          int
	editError         string
	afterTargetLookup func()
	requests          []groupReplyRequest
}

func newGroupAdminFixture(t *testing.T) *groupAdminFixture {
	t.Helper()
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.AuditLog{}, &model.TitleMemberSyncRequest{}); err != nil {
		t.Fatal(err)
	}
	previous := joinVerification
	joinVerification = nil
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	config.AppConfig.NoticeGroupID = -100300
	telegram.ClearSession(1)
	telegram.ClearSession(2)
	t.Cleanup(func() {
		joinVerification = previous
		telegram.ClearSession(1)
		telegram.ClearSession(2)
	})
	f := &groupAdminFixture{
		chatType:  "supergroup",
		botMember: map[string]any{"user": map[string]any{"id": 99, "is_bot": true}, "status": "administrator", "can_promote_members": true, "can_manage_chat": true, "can_delete_messages": true, "can_manage_tags": true, "can_post_stories": true},
		target:    map[string]any{"user": map[string]any{"id": groupAdminTestTarget, "first_name": "测试管理员"}, "status": "member"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fields := map[string]string{}
		for field := range r.Form {
			fields[field] = r.Form.Get(field)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, groupReplyRequest{method: method, fields: fields})
		w.Header().Set("Content-Type", "application/json")
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 99, "is_bot": true, "first_name": "test", "username": "test_bot"}
		case "getChat":
			result = map[string]any{"id": -100300, "type": f.chatType, "title": "测试交流群"}
		case "getChatMember":
			if f.lookupError {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "member lookup failed"})
				return
			}
			if fields["user_id"] == "99" {
				result = f.botMember
			} else if fields["user_id"] == strconv.FormatInt(groupAdminTestTarget, 10) {
				result = f.target
				if f.afterTargetLookup != nil {
					f.afterTargetLookup()
				}
			} else {
				id, _ := strconv.ParseInt(fields["user_id"], 10, 64)
				result = map[string]any{"user": map[string]any{"id": id, "first_name": "operator"}, "status": "member"}
			}
		case "promoteChatMember":
			if f.promoteCode == -1 {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			if f.promoteCode != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": f.promoteCode, "description": "promotion failed"})
				return
			}
			f.target["status"], f.target["can_be_edited"] = "administrator", true
			for field, value := range fields {
				if strings.HasPrefix(field, "can_") || field == "is_anonymous" {
					f.target[field] = value == "true"
				}
			}
		case "sendMessage", "editMessageText":
			if method == "editMessageText" && f.editCode != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": f.editCode, "description": f.editError})
				return
			}
			chatID, _ := strconv.ParseInt(fields["chat_id"], 10, 64)
			result = map[string]any{"message_id": 99, "chat": map[string]any{"id": chatID, "type": "private"}, "text": fields["text"]}
		case "answerCallbackQuery":
		default:
			t.Errorf("unexpected Telegram method %s", method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.bot = bot
	return f
}

func (f *groupAdminFixture) calls(method string) []groupReplyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []groupReplyRequest
	for _, request := range f.requests {
		if request.method == method {
			result = append(result, request)
		}
	}
	return result
}

func currentGroupAdminDraft(t *testing.T) groupAdminDraft {
	t.Helper()
	var draft groupAdminDraft
	if err := json.Unmarshal([]byte(telegram.GetSession(1).GetTemp("group_admin_draft")), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

func groupAdminTestCallback(nonce, action string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: nonce + action, From: &tgbotapi.User{ID: 1}, Data: groupAdminPrefix + nonce + ":" + action,
		Message: &tgbotapi.Message{MessageID: 99, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
}

func groupAdminTestOpen(t *testing.T, f *groupAdminFixture) {
	t.Helper()
	cb := groupAdminTestCallback("", "")
	cb.Data = "admin:cmd:group_admins"
	if !handleAdminMenuCallback(f.bot, cb) {
		t.Fatal("menu entry not handled")
	}
	handleInteractiveMessage(f.bot, groupReplyAdminMessage(strconv.FormatInt(groupAdminTestTarget, 10)))
}

func groupAdminTestPreview(t *testing.T, f *groupAdminFixture) *tgbotapi.CallbackQuery {
	t.Helper()
	groupAdminTestOpen(t, f)
	draft := currentGroupAdminDraft(t)
	handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "preview"))
	draft = currentGroupAdminDraft(t)
	return groupAdminTestCallback(draft.Nonce, "apply")
}

func TestGroupAdminPermissionSelectionConfirmationAndTitleResync(t *testing.T) {
	f := newGroupAdminFixture(t)
	command, superOnly, ok := adminMenuCommandText("group_admins")
	_, menu := renderSuperAdminMenu("users")
	encoded, _ := json.Marshal(menu)
	if !ok || !superOnly || command != groupAdminCommand || !strings.Contains(string(encoded), "admin:cmd:group_admins") {
		t.Fatal("missing privileged group administrator menu")
	}
	groupAdminTestOpen(t, f)
	draft := currentGroupAdminDraft(t)
	for field, value := range draft.Rights {
		if value != (field == "can_manage_chat") {
			t.Fatal("new administrator received unexpected default rights", draft.Rights)
		}
	}
	handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "apply"))
	// Pinning is unavailable; forged toggles must not select it.
	handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "toggle:3"))
	if currentGroupAdminDraft(t).Rights["can_pin_messages"] {
		t.Fatal("unavailable privilege selected")
	}
	stale := groupAdminTestCallback(draft.Nonce, "preview")
	handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "toggle:0"))
	handleGroupAdminCallback(f.bot, stale)
	if telegram.GetSession(1).GetStep() != groupAdminSelectStep || !currentGroupAdminDraft(t).Rights["can_delete_messages"] {
		t.Fatal("old panel changed the current selection")
	}
	draft = currentGroupAdminDraft(t)
	handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "preview"))
	if len(f.calls("promoteChatMember")) != 0 {
		t.Fatal("selection or preview changed Telegram privileges")
	}
	draft = currentGroupAdminDraft(t)
	confirm := groupAdminTestCallback(draft.Nonce, "apply")
	handleGroupAdminCallback(f.bot, confirm)
	handleGroupAdminCallback(f.bot, confirm)
	calls := f.calls("promoteChatMember")
	if len(calls) != 1 {
		t.Fatalf("duplicate confirmation caused %d promotions", len(calls))
	}
	fields := calls[0].fields
	if fields["chat_id"] != "-100300" || fields["user_id"] != "6254916849" || fields["can_manage_chat"] != "true" || fields["can_delete_messages"] != "true" {
		t.Fatal("wrong group, target or requested privileges", fields)
	}
	for _, field := range []string{"can_pin_messages", "can_promote_members", "can_manage_tags", "can_post_stories", "is_anonymous", "can_post_messages"} {
		if fields[field] != "false" {
			t.Fatal("unselected permission was granted", field, fields)
		}
	}
	var events []model.TitleMemberSyncRequest
	if err := storage.DB.Find(&events).Error; err != nil || len(events) != 1 || events[0].UserID != groupAdminTestTarget {
		t.Fatal("promotion did not enqueue title resync", events, err)
	}
	var audits []model.AuditLog
	if err := storage.DB.Order("id").Find(&audits).Error; err != nil || len(audits) != 2 || audits[0].Action != "GROUP_ADMIN_PROMOTE_INTENT" || audits[1].Action != "GROUP_ADMIN_PROMOTE_OK" {
		t.Fatal("missing promotion audit trail", audits, err)
	}
	var users int64
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", groupAdminTestTarget).Count(&users).Error; err != nil || users != 0 {
		t.Fatal("group promotion created or authorized a bot backend account", users, err)
	}
}

func TestGroupAdminRejectsInaccessibleTargetsAndBotsWithoutPromotionRights(t *testing.T) {
	for _, name := range []string{"owner", "uneditable admin", "left", "kicked", "restricted", "bot", "bot lacks promotion", "channel", "lookup failure", "unknown privilege", "audit history unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := newGroupAdminFixture(t)
			switch name {
			case "owner":
				f.target["status"] = "creator"
			case "uneditable admin":
				f.target["status"] = "administrator"
			case "left", "kicked", "restricted":
				f.target["status"] = name
			case "bot":
				f.target["user"] = map[string]any{"id": groupAdminTestTarget, "is_bot": true}
			case "bot lacks promotion":
				f.botMember["can_promote_members"] = false
			case "channel":
				f.chatType = "channel"
			case "lookup failure":
				f.lookupError = true
			case "unknown privilege":
				f.target["status"], f.target["can_be_edited"], f.target["can_future_privilege"] = "administrator", true, true
			case "audit history unavailable":
				if err := storage.DB.Migrator().DropTable(&model.AuditLog{}); err != nil {
					t.Fatal(err)
				}
			}
			groupAdminTestOpen(t, f)
			if telegram.GetSession(1).GetStep() != groupAdminInputStep || len(f.calls("promoteChatMember")) != 0 {
				t.Fatal("inaccessible target reached privilege selection or was modified")
			}
		})
	}
}

func TestGroupAdminRechecksAuthorizationMembershipAndAuditBeforeSubmission(t *testing.T) {
	for _, name := range []string{"revoked operator", "changed group", "expired", "target left", "target promoted", "lost bot privilege", "lost selected privilege", "lookup failure", "audit unavailable", "audit history unavailable", "forged private chat", "copied callback", "restart"} {
		t.Run(name, func(t *testing.T) {
			f := newGroupAdminFixture(t)
			confirm := groupAdminTestPreview(t, f)
			if name == "lost selected privilege" {
				draft := currentGroupAdminDraft(t)
				handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "edit"))
				draft = currentGroupAdminDraft(t)
				handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "toggle:0"))
				draft = currentGroupAdminDraft(t)
				handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "preview"))
				draft = currentGroupAdminDraft(t)
				confirm = groupAdminTestCallback(draft.Nonce, "apply")
			}
			switch name {
			case "revoked operator":
				delete(config.AppConfig.AdminIDs, 1)
			case "changed group":
				config.AppConfig.NoticeGroupID = -100999
			case "expired":
				draft := currentGroupAdminDraft(t)
				draft.ExpiresAt = time.Now().Add(-time.Second)
				data, _ := json.Marshal(draft)
				telegram.GetSession(1).SetTemp("group_admin_draft", string(data))
			case "target left":
				f.target["status"] = "left"
			case "target promoted":
				f.target["status"], f.target["can_be_edited"] = "administrator", true
			case "lost bot privilege":
				f.botMember["can_promote_members"] = false
			case "lost selected privilege":
				f.botMember["can_delete_messages"] = false
			case "lookup failure":
				f.lookupError = true
			case "restart":
				telegram.ClearSession(1)
			case "audit unavailable":
				if err := storage.DB.Exec(`CREATE TRIGGER fail_group_admin_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`).Error; err != nil {
					t.Fatal(err)
				}
			case "audit history unavailable":
				if err := storage.DB.Migrator().DropTable(&model.AuditLog{}); err != nil {
					t.Fatal(err)
				}
			case "forged private chat":
				confirm.Message.Chat.ID = 2
			case "copied callback":
				config.AppConfig.AdminIDs[2] = true
				confirm.From.ID, confirm.Message.Chat.ID = 2, 2
			}
			handleGroupAdminCallback(f.bot, confirm)
			if len(f.calls("promoteChatMember")) != 0 {
				t.Fatal("pre-submit guard allowed a promotion")
			}
		})
	}
}

func TestGroupAdminPreservesExistingRightsAndDetectsConcurrentEdits(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent change=%t", changed), func(t *testing.T) {
			f := newGroupAdminFixture(t)
			f.target["status"], f.target["can_be_edited"], f.target["can_manage_chat"] = "administrator", true, true
			f.target["can_manage_tags"], f.target["can_post_stories"] = true, true
			confirm := groupAdminTestPreview(t, f)
			if changed {
				f.target["can_manage_tags"] = false
			}
			handleGroupAdminCallback(f.bot, confirm)
			calls := f.calls("promoteChatMember")
			if changed {
				if len(calls) != 0 {
					t.Fatal("concurrent permission edit was overwritten")
				}
			} else if len(calls) != 1 || calls[0].fields["can_manage_tags"] != "true" || calls[0].fields["can_post_stories"] != "true" {
				t.Fatal("existing modern Bot API privileges were lost", calls)
			}
			if !changed {
				var request model.TitleMemberSyncRequest
				if err := storage.DB.First(&request, "chat_id = ? AND user_id = ?", -100300, groupAdminTestTarget).Error; err != nil {
					t.Fatal("permission edit without a role change did not request title resync", err)
				}
			}
		})
	}
}

func TestGroupAdminFailureAndUncertainResultsNeverReplayConfirmation(t *testing.T) {
	for _, tc := range []struct {
		code   int
		action string
	}{
		{-1, "GROUP_ADMIN_PROMOTE_UNCERTAIN"},
		{400, "GROUP_ADMIN_PROMOTE_FAILED"},
		{429, "GROUP_ADMIN_PROMOTE_FAILED"},
		{500, "GROUP_ADMIN_PROMOTE_UNCERTAIN"},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			f := newGroupAdminFixture(t)
			f.promoteCode = tc.code
			confirm := groupAdminTestPreview(t, f)
			handleGroupAdminCallback(f.bot, confirm)
			handleGroupAdminCallback(f.bot, confirm)
			if len(f.calls("promoteChatMember")) != 1 || telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatal("failed or uncertain operation was replayed")
			}
			var audit model.AuditLog
			if err := storage.DB.Where("action = ?", tc.action).First(&audit).Error; err != nil {
				t.Fatal("incorrect failure outcome", err)
			}
		})
	}
}

func TestGroupAdminEntryDeniesRegularUsersAndPublicChats(t *testing.T) {
	f := newGroupAdminFixture(t)
	for _, msg := range []*tgbotapi.Message{
		{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}},
		{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: -100300, Type: "supergroup"}},
	} {
		if !handleGroupAdminMessage(f.bot, msg, groupAdminCommand) || telegram.GetSession(msg.From.ID).GetStep() != "IDLE" {
			t.Fatal("unauthorized entry opened the administrator wizard")
		}
	}
	if len(f.calls("getChat")) != 0 || len(f.calls("promoteChatMember")) != 0 {
		t.Fatal("unauthorized entry queried or modified group administrators")
	}
}

func TestGroupAdminReportsSuccessWhenResultAuditFailsWithoutReplaying(t *testing.T) {
	f := newGroupAdminFixture(t)
	confirm := groupAdminTestPreview(t, f)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_group_admin_result BEFORE INSERT ON audit_logs WHEN NEW.action = 'GROUP_ADMIN_PROMOTE_OK' BEGIN SELECT RAISE(ABORT,'result audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	handleGroupAdminCallback(f.bot, confirm)
	handleGroupAdminCallback(f.bot, confirm)
	if len(f.calls("promoteChatMember")) != 1 {
		t.Fatal("result audit failure replayed a successful promotion")
	}
	for _, request := range f.calls("sendMessage") {
		if strings.Contains(request.fields["text"], "已按所选权限") && strings.Contains(request.fields["text"], "记录保存失败") {
			return
		}
	}
	t.Fatal("successful external update was hidden by the audit failure")
}

func TestGroupAdminTitleQueueFailurePreservesPromotionAndIdentifiesRecoveryUser(t *testing.T) {
	f := newGroupAdminFixture(t)
	confirm := groupAdminTestPreview(t, f)
	if err := storage.DB.Exec(`CREATE TRIGGER fail_group_admin_title_queue BEFORE INSERT ON title_member_sync_requests BEGIN SELECT RAISE(ABORT,'title queue unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	handleGroupAdminCallback(f.bot, confirm)
	handleGroupAdminCallback(f.bot, confirm)
	if len(f.calls("promoteChatMember")) != 1 {
		t.Fatal("title queue failure replayed the promotion")
	}
	var audit model.AuditLog
	if err := storage.DB.Where("action = ?", "GROUP_ADMIN_PROMOTE_OK").First(&audit).Error; err != nil {
		t.Fatal("title queue failure hid the successful promotion", err)
	}
	for _, request := range f.calls("sendMessage") {
		text := request.fields["text"]
		if strings.Contains(text, "已按所选权限") && strings.Contains(text, "称号同步暂未入队") {
			if !strings.Contains(text, fmt.Sprintf("请 TGID %d 对应的用户", groupAdminTestTarget)) {
				t.Fatal("recovery instruction did not identify the target user; the operator would re-equip their own title", text)
			}
			return
		}
	}
	t.Fatal("title queue failure did not report a recovery instruction")
}

func TestGroupAdminLeavingMenusInvalidatesConfirmation(t *testing.T) {
	for _, navigation := range []string{"admin callback", "user callback", "admin keyboard", "user keyboard"} {
		t.Run(navigation, func(t *testing.T) {
			f := newGroupAdminFixture(t)
			confirm := groupAdminTestPreview(t, f)
			switch navigation {
			case "admin callback":
				cb := groupAdminTestCallback("", "")
				cb.Data = "admin:users"
				handleAdminMenuCallback(f.bot, cb)
			case "user callback":
				cb := groupAdminTestCallback("", "")
				cb.Data = "menu:home"
				handleMenuCallback(f.bot, cb)
			case "admin keyboard":
				handleInteractiveMessage(f.bot, groupReplyAdminMessage("👤 用户管理"))
			case "user keyboard":
				handleInteractiveMessage(f.bot, groupReplyAdminMessage("⚙️ 账号服务"))
			}
			if telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatal("leaving the wizard retained the pending confirmation")
			}
			handleGroupAdminCallback(f.bot, confirm)
			if len(f.calls("promoteChatMember")) != 0 {
				t.Fatal("an old confirmation remained active after menu navigation")
			}
		})
	}
}

func TestGroupAdminRevocationDuringPreflightStopsPromotion(t *testing.T) {
	f := newGroupAdminFixture(t)
	delete(config.AppConfig.AdminIDs, 1)
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("role", "super_admin").Error; err != nil {
		t.Fatal(err)
	}
	confirm := groupAdminTestPreview(t, f)
	f.mu.Lock()
	f.afterTargetLookup = func() {
		if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("role", "user").Error; err != nil {
			t.Error(err)
		}
	}
	f.mu.Unlock()
	handleGroupAdminCallback(f.bot, confirm)
	if len(f.calls("promoteChatMember")) != 0 {
		t.Fatal("operator lost authorization during preflight but still promoted the target")
	}
}

func TestGroupAdminPanelFailuresOnlyReplaceUnavailableMessages(t *testing.T) {
	for _, tc := range []struct {
		code    int
		message string
		replace bool
	}{
		{400, "Bad Request: message to edit not found", true},
		{429, "Too Many Requests", false},
		{500, "Internal Server Error", false},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			f := newGroupAdminFixture(t)
			groupAdminTestOpen(t, f)
			draft := currentGroupAdminDraft(t)
			before := len(f.calls("sendMessage"))
			f.mu.Lock()
			f.editCode, f.editError = tc.code, tc.message
			f.mu.Unlock()
			handleGroupAdminCallback(f.bot, groupAdminTestCallback(draft.Nonce, "preview"))
			after := len(f.calls("sendMessage"))
			if tc.replace {
				if after != before+1 || telegram.GetSession(1).GetStep() != groupAdminConfirmStep {
					t.Fatal("a deleted panel was not replaced")
				}
			} else if after != before || telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatal("transient edit failure sent a replacement or retained an invisible confirmation")
			}
		})
	}
}

func dispatchGroupAdminTestCallback(t *testing.T, f *groupAdminFixture, cb *tgbotapi.CallbackQuery) {
	t.Helper()
	dispatchTelegramUpdate(f.bot, nil, tgbotapi.Update{CallbackQuery: cb})
	// A barrier in the actual per-user update queue waits for all deferred
	// cleanup, too, before the test reads state or replaces global fixtures.
	barrier := reserveUserUpdate(cb.From.ID)
	defer barrier.finish()
	select {
	case <-barrier.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not finish through the actual router")
	}
}

func TestGroupAdminOtherPrivateCallbacksCancelPendingConfirmation(t *testing.T) {
	for _, data := range []string{"titles:rank:-200", "wealth_page:0"} {
		t.Run(data, func(t *testing.T) {
			f := newGroupAdminFixture(t)
			confirm := groupAdminTestPreview(t, f)
			navigate := groupAdminTestCallback("", "")
			navigate.ID, navigate.Data = data, data
			dispatchGroupAdminTestCallback(t, f, navigate)
			if telegram.GetSession(1).GetStep() != "IDLE" {
				t.Fatal("a separate private feature retained the administrator confirmation")
			}
			dispatchGroupAdminTestCallback(t, f, confirm)
			if len(f.calls("promoteChatMember")) != 0 {
				t.Fatal("a confirmation survived navigation through another callback handler")
			}
		})
	}
}

func TestGroupAdminUnrelatedChatCallbacksPreservePrivateDraft(t *testing.T) {
	for _, chat := range []tgbotapi.Chat{{ID: -200, Type: "supergroup"}, {ID: 2, Type: "private"}} {
		t.Run(strconv.FormatInt(chat.ID, 10), func(t *testing.T) {
			f := newGroupAdminFixture(t)
			confirm := groupAdminTestPreview(t, f)
			cb := groupAdminTestCallback("", "")
			cb.Data, cb.Message.Chat = "titles:rank:-200", &chat
			dispatchGroupAdminTestCallback(t, f, cb)
			if telegram.GetSession(1).GetStep() != groupAdminConfirmStep {
				t.Fatal("an unrelated chat cancelled the private draft")
			}
			dispatchGroupAdminTestCallback(t, f, confirm)
			if len(f.calls("promoteChatMember")) != 1 {
				t.Fatal("the current group administrator confirmation failed through the router")
			}
		})
	}
}

func TestGroupAdminConcurrentAttemptsInvalidateOtherOperatorsPreview(t *testing.T) {
	for _, code := range []int{0, 400, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newGroupAdminFixture(t)
			config.AppConfig.AdminIDs[2] = true
			// An unchanged successful edit must also consume every older preview.
			f.target["status"], f.target["can_be_edited"], f.target["can_manage_chat"] = "administrator", true, true
			groupAdminTestPreview(t, f)
			first := currentGroupAdminDraft(t)
			second, err := prepareGroupAdminDraft(f.bot, groupAdminTestTarget)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.promoteCode = code
			f.mu.Unlock()
			start := make(chan struct{})
			results := make(chan string, 2)
			for i, draft := range []*groupAdminDraft{&first, second} {
				go func(actorID int64, draft *groupAdminDraft) {
					<-start
					results <- applyGroupAdminDraft(f.bot, actorID, draft)
				}(int64(i+1), draft)
			}
			close(start)
			stale := 0
			for range 2 {
				select {
				case result := <-results:
					if strings.Contains(result, "此预览之后已有其他任命尝试") {
						stale++
					}
				case <-time.After(3 * time.Second):
					t.Fatal("concurrent administrator attempts did not finish")
				}
			}
			if len(f.calls("promoteChatMember")) != 1 || stale != 1 {
				t.Fatal("a stale preview was not rejected after another operator's attempt")
			}
			// Reopening and explicitly confirming a fresh preview remains possible.
			f.mu.Lock()
			f.promoteCode = 0
			f.mu.Unlock()
			handleGroupAdminCallback(f.bot, groupAdminTestPreview(t, f))
			if len(f.calls("promoteChatMember")) != 2 {
				t.Fatal("a fresh preview was incorrectly blocked by a previous attempt")
			}
		})
	}
}

func TestGroupAdminInvalidatedPanelReleasesWizardWithoutAffectingNewerDraft(t *testing.T) {
	for _, reason := range []string{"expired", "expired message", "changed group", "changed group message", "revoked operator", "stale nonce"} {
		t.Run(reason, func(t *testing.T) {
			f := newGroupAdminFixture(t)
			confirm := groupAdminTestPreview(t, f)
			switch strings.TrimSuffix(reason, " message") {
			case "expired":
				draft := currentGroupAdminDraft(t)
				draft.ExpiresAt = time.Now().Add(-time.Second)
				data, _ := json.Marshal(draft)
				telegram.GetSession(1).SetTemp("group_admin_draft", string(data))
				// Even the current Cancel button must release an expired wizard.
				confirm.Data = groupAdminPrefix + draft.Nonce + ":cancel"
			case "changed group":
				config.AppConfig.NoticeGroupID = -100999
			case "revoked operator":
				delete(config.AppConfig.AdminIDs, 1)
			case "stale nonce":
				groupAdminTestPreview(t, f)
			}
			if strings.HasSuffix(reason, " message") {
				if handleGroupAdminMessage(f.bot, groupReplyAdminMessage("我的称号"), "我的称号") {
					t.Fatal("an invalidated wizard intercepted another command")
				}
			} else {
				handleGroupAdminCallback(f.bot, confirm)
			}
			if len(f.calls("promoteChatMember")) != 0 {
				t.Fatal("an invalidated panel modified group administrators")
			}
			want := "IDLE"
			if reason == "stale nonce" {
				want = groupAdminConfirmStep
			}
			if step := telegram.GetSession(1).GetStep(); step != want {
				t.Fatalf("wizard state = %s, want %s", step, want)
			}
		})
	}
}

package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
)

type groupReplyRequest struct {
	method string
	fields map[string]string
}

func groupReplyFixture(t *testing.T) (*tgbotapi.BotAPI, *[]groupReplyRequest) {
	t.Helper()
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.GroupReply{}, &model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	previous := joinVerification
	joinVerification = nil
	t.Cleanup(func() { joinVerification = previous; telegram.ClearSession(1); telegram.ClearSession(2) })
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	config.AppConfig.NoticeGroupID = -100300
	requests := []groupReplyRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fields := map[string]string{}
		for k := range r.Form {
			fields[k] = r.Form.Get(k)
		}
		requests = append(requests, groupReplyRequest{method, fields})
		var out any = map[string]any{"message_id": 99, "chat": map[string]any{"id": 1, "type": "private"}}
		switch method {
		case "getMe":
			out = map[string]any{"id": 99, "is_bot": true, "first_name": "test", "username": "test_bot"}
		case "answerCallbackQuery":
			out = true
		case "getChatMember":
			out = map[string]any{"status": "member"}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, &requests
}
func groupReplyAdminMessage(text string) *tgbotapi.Message {
	return &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: text}
}
func groupReplyAdminCallback(data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{ID: "test", From: &tgbotapi.User{ID: 1}, Data: groupReplyPrefix + data, Message: &tgbotapi.Message{MessageID: 99, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}}
}
func TestGroupReplyAdminCRUDAndPermissions(t *testing.T) {
	bot, _ := groupReplyFixture(t)
	command, superOnly, ok := adminMenuCommandText("group_replies")
	if !ok || !superOnly || command != "群关键词回复" {
		t.Fatal("missing privileged menu command")
	}
	_, menu := renderSuperAdminMenu("config")
	b, _ := json.Marshal(menu)
	if !strings.Contains(string(b), "admin:cmd:group_replies") {
		t.Fatal("missing config entry")
	}
	if !handleGroupReplyAdminMessage(bot, groupReplyAdminMessage(command), command) {
		t.Fatal("entry not handled")
	}
	for _, keyword := range []string{"使用教程入口", "联系站长入口"} {
		handleGroupReplyCallback(bot, groupReplyAdminCallback("add"))
		handleInteractiveMessage(bot, groupReplyAdminMessage(keyword))
		if telegram.GetSession(1).GetStep() != groupReplyContentStep {
			t.Fatal("keyword not captured")
		}
		// Content containing 返回 must be saved rather than triggering the generic back handler.
		handleInteractiveMessage(bot, groupReplyAdminMessage("第一行 [链接](https://example.com)\n点击返回继续"))
		if telegram.GetSession(1).GetStep() != "IDLE" {
			t.Fatal("save did not finish session")
		}
	}
	var rules []model.GroupReply
	if err := storage.DB.Order("id").Find(&rules).Error; err != nil || len(rules) != 2 {
		t.Fatal("multiple rules not saved", err, rules)
	}
	id := strconv.FormatUint(uint64(rules[0].ID), 10)
	handleGroupReplyCallback(bot, groupReplyAdminCallback("content:"+id))
	handleInteractiveMessage(bot, groupReplyAdminMessage("更新后的回复"))
	handleGroupReplyCallback(bot, groupReplyAdminCallback("keyword:"+id))
	handleInteractiveMessage(bot, groupReplyAdminMessage("新版教程入口"))
	var edited model.GroupReply
	storage.DB.First(&edited, rules[0].ID)
	if edited.Keyword != "新版教程入口" || edited.Content != "更新后的回复" {
		t.Fatal("edit failed", edited)
	}
	handleGroupReplyCallback(bot, groupReplyAdminCallback("toggle:"+id))
	storage.DB.First(&edited, rules[0].ID)
	if edited.Enabled {
		t.Fatal("disable failed")
	}
	// A forged callback and a revoked session must not modify configuration.
	denied := groupReplyAdminCallback("remove:" + id)
	denied.From.ID, denied.Message.Chat.ID = 2, 2
	handleGroupReplyCallback(bot, denied)
	telegram.GetSession(2).SetStep(groupReplyContentStep)
	telegram.GetSession(2).SetTemp("group_reply_id", id)
	telegram.GetSession(2).SetTemp("group_reply_keyword", "非法覆盖")
	msg := groupReplyAdminMessage("非法回复")
	msg.From.ID, msg.Chat.ID = 2, 2
	handleGroupReplyAdminMessage(bot, msg, msg.Text)
	storage.DB.First(&edited, rules[0].ID)
	if edited.Content != "更新后的回复" || telegram.GetSession(2).GetStep() != "IDLE" {
		t.Fatal("unprivileged edit succeeded")
	}
	// The first delete only asks for confirmation.
	handleGroupReplyCallback(bot, groupReplyAdminCallback("delete:"+id))
	var n int64
	storage.DB.Model(&model.GroupReply{}).Count(&n)
	if n != 2 {
		t.Fatal("delete did not wait for confirmation")
	}
	handleGroupReplyCallback(bot, groupReplyAdminCallback("remove:"+id))
	storage.DB.Model(&model.GroupReply{}).Count(&n)
	if n != 1 {
		t.Fatal("confirmed delete failed")
	}
	storage.DB.Model(&model.AuditLog{}).Where("action LIKE ?", "GROUP_REPLY_%").Count(&n)
	if n != 6 {
		t.Fatalf("missing audit entries: %d", n)
	}
}

func TestGroupKeywordReplyMatchingAndIsolation(t *testing.T) {
	bot, requests := groupReplyFixture(t)
	if err := saveGroupReply(1, 0, "帮助文档入口", "多行内容\n<b>原样显示</b>"); err != nil {
		t.Fatal(err)
	}
	if err := saveGroupReply(1, 0, "FAQ", "English"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text, chatType string
		chatID         int64
		isBot, want    bool
	}{
		{"帮助文档入口", "supergroup", -100300, false, true},
		{"  帮助文档入口  ", "supergroup", -100300, false, true},
		{"请看帮助文档入口", "supergroup", -100300, false, false},
		{"faq", "supergroup", -100300, false, false},
		{"FAQ", "supergroup", -100300, false, true},
		{"帮助文档入口", "private", 2, false, false},
		{"帮助文档入口", "supergroup", -200, false, false},
		{"帮助文档入口", "channel", -100300, false, false},
		{"帮助文档入口", "supergroup", -100300, true, false},
		{"", "supergroup", -100300, false, false},
	} {
		t.Run(fmt.Sprintf("%s/%s/%d/%t", tc.text, tc.chatType, tc.chatID, tc.isBot), func(t *testing.T) {
			msg := &tgbotapi.Message{MessageID: 321, From: &tgbotapi.User{ID: 2, IsBot: tc.isBot}, Chat: &tgbotapi.Chat{ID: tc.chatID, Type: tc.chatType}, Text: tc.text}
			before := len(*requests)
			if got := handleGroupKeywordReply(bot, msg, msg.Text); got != tc.want {
				t.Fatalf("got %t want %t", got, tc.want)
			}
			if !tc.want && len(*requests) != before {
				t.Fatal("nonmatching input sent a response")
			}
			if tc.want {
				last := (*requests)[len(*requests)-1]
				if last.method != "sendMessage" || last.fields["reply_to_message_id"] != "321" || last.fields["parse_mode"] != "" {
					t.Fatal("reply not plain text or not threaded", last)
				}
			}
		})
	}
	var r model.GroupReply
	storage.DB.First(&r, "keyword = ?", "帮助文档入口")
	if err := mutateGroupReply(1, r.ID, false); err != nil {
		t.Fatal(err)
	}
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: -100300, Type: "supergroup"}, Text: r.Keyword}
	if handleGroupKeywordReply(bot, msg, msg.Text) {
		t.Fatal("disabled rule fired")
	}
	if err := mutateGroupReply(1, r.ID, false); err != nil {
		t.Fatal(err)
	}
	if !handleGroupKeywordReply(bot, msg, msg.Text) {
		t.Fatal("enabled rule did not fire")
	}
	if err := mutateGroupReply(1, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if handleGroupKeywordReply(bot, msg, msg.Text) {
		t.Fatal("deleted rule fired")
	}
}

func TestGroupReplyValidationAndAtomicAudit(t *testing.T) {
	_, _ = groupReplyFixture(t)
	for _, tc := range []struct{ keyword, content string }{
		{"", "reply"}, {"提示词\n换行", "reply"}, {strings.Repeat("字", 101), "reply"},
		{"/start", "reply"}, {"签到", "reply"}, {"帮助文档入口", " "}, {"帮助文档入口", strings.Repeat("😀", 2001)},
	} {
		if err := saveGroupReply(1, 0, tc.keyword, tc.content); err == nil {
			t.Fatalf("invalid rule accepted: %q", tc.keyword)
		}
	}
	if err := saveGroupReply(1, 0, " 文档入口 ", "reply"); err != nil {
		t.Fatal(err)
	}
	if err := saveGroupReply(1, 0, "文档入口", "duplicate"); err == nil {
		t.Fatal("duplicate keyword accepted")
	}
	if err := storage.DB.Migrator().DropTable(&model.AuditLog{}); err != nil {
		t.Fatal(err)
	}
	if err := saveGroupReply(1, 0, "另一个入口", "reply"); err == nil {
		t.Fatal("missing audit did not fail save")
	}
	var n int64
	storage.DB.Model(&model.GroupReply{}).Count(&n)
	if n != 1 {
		t.Fatal("unaudited mutation committed")
	}
}

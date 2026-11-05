package joinverify

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/model"
)

type fakeAPI struct {
	group      map[int64]member
	channel    map[int64]member
	defaults   permissions
	calls      map[string]int
	fail       map[string]int
	failWith   map[string]error
	lost       map[string]int // Apply mutation, then lose response.
	beforeCall func(string, tgbotapi.Params)
	messageID  int
	deleted    int
	requests   []struct {
		method string
		params tgbotapi.Params
	}
}

func (f *fakeAPI) MakeRequest(method string, p tgbotapi.Params) (*tgbotapi.APIResponse, error) {
	copyParams := tgbotapi.Params{}
	for k, v := range p {
		copyParams[k] = v
	}
	f.requests = append(f.requests, struct {
		method string
		params tgbotapi.Params
	}{method, copyParams})
	if f.beforeCall != nil {
		f.beforeCall(method, p)
	}
	f.calls[method]++
	key := method
	if method == "getChatMember" {
		key += ":" + p["chat_id"]
	}
	if f.fail[key] > 0 {
		f.fail[key]--
		if err := f.failWith[key]; err != nil {
			return nil, err
		}
		return nil, errors.New("simulated API unavailable")
	}
	id, _ := strconv.ParseInt(p["user_id"], 10, 64)
	var out any = true
	switch method {
	case "getChat":
		kind := "supergroup"
		if p["chat_id"] == "-200" {
			kind = "channel"
		}
		out = chat{Type: kind, Permissions: f.defaults}
	case "getChatMember":
		m, ok := f.group[id]
		if p["chat_id"] == "-200" {
			m, ok = f.channel[id]
		}
		if !ok {
			m = member{Status: "left"}
		}
		out = m
	case "restrictChatMember":
		var perm permissions
		if err := json.Unmarshal([]byte(p["permissions"]), &perm); err != nil {
			return nil, err
		}
		until, _ := strconv.ParseInt(p["until_date"], 10, 64)
		f.group[id] = member{Status: "restricted", IsMember: true, permissions: perm, UntilDate: until}
	case "banChatMember":
		f.group[id] = member{Status: "kicked"}
	case "unbanChatMember":
		if p["only_if_banned"] != "true" {
			return nil, errors.New("unsafe unban")
		}
		if f.group[id].Status == "kicked" {
			f.group[id] = member{Status: "left"}
		}
	case "sendMessage":
		f.messageID++
		out = map[string]int{"message_id": f.messageID}
	case "editMessageText":
		out = map[string]int{"message_id": 1}
	case "deleteMessage":
		f.deleted++
	default:
		return nil, errors.New("unexpected method: " + method)
	}
	if f.lost[key] > 0 {
		f.lost[key]--
		return nil, errors.New("response lost after mutation")
	}
	data, err := json.Marshal(out)
	return &tgbotapi.APIResponse{Ok: true, Result: data}, err
}
func fixture(t *testing.T) (*Service, *fakeAPI, *time.Time) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "verify.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sql.Close() })
	if err = db.AutoMigrate(&model.JoinVerification{}, &model.JoinVerificationPrompt{}, &model.JoinVerificationGroupActivity{}, &model.JoinVerificationEvent{}, &model.AuditLog{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{group: map[int64]member{1: {Status: "member"}, 999: {Status: "administrator", CanRestrictMembers: true, CanDeleteMessages: true}}, channel: map[int64]member{999: {Status: "administrator"}}, defaults: permissions{SendMessages: true, SendAudios: true, SendPhotos: true, SendPolls: true, InviteUsers: true}, calls: map[string]int{}, fail: map[string]int{}, lost: map[string]int{}}
	s := New(db, f, -100, -200, 999, "https://t.me/example", nil, "testbot")
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, f, &now
}
func record(t *testing.T, s *Service, id int64) model.JoinVerification {
	t.Helper()
	r, err := s.record(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func sharedPrompt(t *testing.T, s *Service) model.JoinVerificationPrompt {
	t.Helper()
	var p model.JoinVerificationPrompt
	if err := s.db.First(&p, "group_id = ?", s.groupID).Error; err != nil {
		t.Fatal(err)
	}
	return p
}
func callback(s *Service, r model.JoinVerification, id int64) *tgbotapi.CallbackQuery {
	s.Start(&tgbotapi.Message{From: &tgbotapi.User{ID: id}, Chat: &tgbotapi.Chat{ID: id, Type: "private"}, Text: "/start jv_" + r.Round})
	if latest, err := s.record(id); err == nil && latest.Round == r.Round {
		r.PrivateMessageID = latest.PrivateMessageID
	}
	return &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: id}, Data: CallbackPrefix + r.Round, Message: &tgbotapi.Message{MessageID: r.PrivateMessageID, Chat: &tgbotapi.Chat{ID: id, Type: "private"}}}
}
func event(update int, chatID, id, actor int64, old, next string, now time.Time) tgbotapi.Update {
	return tgbotapi.Update{UpdateID: update, ChatMember: &tgbotapi.ChatMemberUpdated{Chat: tgbotapi.Chat{ID: chatID}, From: tgbotapi.User{ID: actor}, Date: int(now.Unix()), OldChatMember: tgbotapi.ChatMember{Status: old, IsMember: old == "restricted", User: &tgbotapi.User{ID: id}}, NewChatMember: tgbotapi.ChatMember{Status: next, IsMember: next == "restricted", User: &tgbotapi.User{ID: id}}}}
}
func enqueue(t *testing.T, s *Service, u tgbotapi.Update) {
	t.Helper()
	if err := s.Enqueue(u); err != nil {
		t.Fatal(err)
	}
}

func TestJoinVerificationAndButtonOwnership(t *testing.T) {
	s, f, now := fixture(t)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	r := record(t, s, 1)
	if r.State != "pending" || r.Deadline.Sub(*now) != Timeout || f.group[1].permissions.SendMessages || sharedPrompt(t, s).MessageID == 0 {
		t.Fatalf("bad initial verification: %+v", r)
	}
	*now = now.Add(time.Minute)
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	if !record(t, s, 1).Deadline.Equal(r.Deadline) {
		t.Fatal("duplicate event extended deadline")
	}
	if got := s.Callback(callback(s, r, 2)); !strings.Contains(got, "本人") {
		t.Fatal(got)
	}
	if got := s.Callback(callback(s, r, 1)); !strings.Contains(got, "尚未") {
		t.Fatal(got)
	}
	f.channel[1] = member{Status: "member"}
	if got := s.Callback(callback(s, r, 1)); !strings.Contains(got, "验证通过") {
		t.Fatal(got)
	}
	s.Sweep()
	if sharedPrompt(t, s).MessageID == 0 || f.deleted != 0 {
		t.Fatal("shared prompt was removed before the idle grace elapsed")
	}
	*now = now.Add(sharedPromptIdleGrace)
	s.Sweep()
	if record(t, s, 1).State != "passed" || !f.group[1].SendPhotos || f.deleted != 1 {
		t.Fatal("did not restore permissions and clean prompt")
	}
	before := f.calls["restrictChatMember"]
	s.Callback(callback(s, r, 1))
	if f.calls["restrictChatMember"] != before {
		t.Fatal("repeated callback restored twice")
	}
}
func TestFollowUnfollowTimeoutAndRejoin(t *testing.T) {
	s, f, now := fixture(t)
	ok, _ := s.Check(1)
	if ok {
		t.Fatal("unfollowed member admitted")
	}
	f.channel[1] = member{Status: "member"}
	enqueue(t, s, event(1, -200, 1, 1, "left", "member", *now))
	s.Sweep()
	if record(t, s, 1).State != "pending" {
		t.Fatal("follow event bypassed confirmation")
	}
	s.Callback(callback(s, record(t, s, 1), 1))
	if record(t, s, 1).State != "passed" {
		t.Fatal("private confirmation did not pass")
	}
	old := record(t, s, 1)
	f.channel[1] = member{Status: "left"}
	enqueue(t, s, event(2, -200, 1, 1, "member", "left", *now))
	s.Sweep()
	r := record(t, s, 1)
	if r.Round == old.Round || r.Reason != "unsubscribe" {
		t.Fatal("no new unsubscribe round")
	}
	cb := callback(s, r, 1)
	*now = now.Add(Timeout)
	if got := s.Callback(cb); !strings.Contains(got, "超时") {
		t.Fatal(got)
	}
	s.Sweep()
	if record(t, s, 1).State != "expired" || f.group[1].Status != "left" || f.calls["unbanChatMember"] != 1 {
		t.Fatal("timeout must kick and allow rejoin")
	}
	f.group[1] = member{Status: "member"}
	enqueue(t, s, event(3, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	next := record(t, s, 1)
	if next.State != "pending" || next.Round == r.Round || !next.Deadline.Equal(now.Add(Timeout)) {
		t.Fatal("rejoin not fresh")
	}
	if got := s.Callback(callback(s, r, 1)); !strings.Contains(got, "失效") {
		t.Fatal(got)
	}
}
func TestRestartAndUncertainAPISideEffects(t *testing.T) {
	for _, failure := range []string{"restrictChatMember", "banChatMember", "unbanChatMember"} {
		t.Run(failure, func(t *testing.T) {
			s, f, now := fixture(t)
			if failure == "restrictChatMember" {
				f.lost[failure] = 1
			}
			s.Check(1)
			r := record(t, s, 1)
			if failure == "restrictChatMember" {
				f.channel[1] = member{Status: "member"}
			} else {
				*now = now.Add(Timeout)
				f.lost[failure] = 1
				s.Sweep()
			}
			// Recreate service using durable state, with no in-memory round data.
			restarted := New(s.db, f, -100, -200, 999, s.channelURL, nil, s.botUsername)
			restarted.now = s.now
			restarted.Sweep()
			latest := record(t, restarted, 1)
			if failure == "restrictChatMember" {
				if latest.State != "pending" || f.group[1].SendMessages {
					t.Fatal("restart bypassed confirmation")
				}
				restarted.Callback(callback(restarted, latest, 1))
				if record(t, restarted, 1).State != "passed" {
					t.Fatal("lost restrict response stranded member")
				}
			} else if latest.State != "expired" || f.group[1].Status != "left" {
				t.Fatalf("restart failed: before=%+v after=%+v", r, latest)
			}
		})
	}
}
func TestQueryFailureDelaysKickButSubscriptionAloneDoesNotPassAtExpiry(t *testing.T) {
	s, f, now := fixture(t)
	f.fail["getChatMember:-200"] = 1
	if ok, _ := s.Check(1); ok {
		t.Fatal("API failure admitted user")
	}
	if f.calls["restrictChatMember"] != 0 {
		t.Fatal("API failure treated as unsubscribe")
	}
	s.Check(1)
	*now = now.Add(Timeout)
	f.fail["getChatMember:-200"] = 1
	s.Sweep()
	if f.calls["banChatMember"] != 0 || record(t, s, 1).State != "pending" {
		t.Fatal("API failure kicked member")
	}
	f.channel[1] = member{Status: "member"}
	s.Sweep()
	if record(t, s, 1).State != "expired" || f.calls["banChatMember"] != 1 {
		t.Fatal("subscribed member bypassed confirmation at expiry")
	}
}
func TestRestoreRetryAndExistingRestrictions(t *testing.T) {
	s, f, now := fixture(t)
	original := permissions{SendMessages: true, SendPhotos: false, InviteUsers: true}
	until := now.Add(time.Hour).Unix()
	f.group[1] = member{Status: "restricted", IsMember: true, permissions: original, UntilDate: until}
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	f.fail["restrictChatMember"] = 1
	s.Callback(callback(s, r, 1))
	if record(t, s, 1).State != "restoring" {
		t.Fatal("missing restore intent")
	}
	s.Sweep()
	if record(t, s, 1).State != "passed" || f.group[1].SendPhotos || f.group[1].UntilDate != until || !f.group[1].SendMessages {
		t.Fatal("original restrictions lost")
	}
}
func TestRestoreLostResponseDoesNotOverrideModerator(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	f.lost["restrictChatMember"] = 1
	s.Callback(callback(s, r, 1))
	f.group[1] = member{Status: "restricted", IsMember: true, permissions: permissions{SendMessages: true}, UntilDate: 1234567890}
	before := f.calls["restrictChatMember"]
	s.Sweep()
	if record(t, s, 1).State != "manual" || f.calls["restrictChatMember"] != before {
		t.Fatal("moderator restriction overwritten during restore retry")
	}
}
func TestAdminInterventionLeaveAndExemptions(t *testing.T) {
	s, f, now := fixture(t)
	f.group[2] = member{Status: "administrator"}
	if ok, _ := s.Check(2); !ok {
		t.Fatal("admin not exempt")
	}
	f.channel[3] = member{Status: "restricted", IsMember: false}
	f.group[3] = member{Status: "member"}
	if ok, _ := s.Check(3); ok {
		t.Fatal("restricted nonmember counted as following")
	}
	s.Check(1)
	enqueue(t, s, event(1, -100, 1, 99, "restricted", "restricted", *now))
	s.Sweep()
	before := f.calls["restrictChatMember"]
	bans := f.calls["banChatMember"]
	// Keep the independent nonmember-status test round from timing out here.
	f.channel[3] = member{Status: "member"}
	s.Callback(callback(s, record(t, s, 3), 3))
	*now = now.Add(Timeout)
	s.Sweep()
	if record(t, s, 1).State != "manual" || f.calls["banChatMember"] != bans || f.calls["restrictChatMember"] != before+1 {
		t.Fatal("human permission edit not respected")
	}
	f.group[1] = member{Status: "member"}
	enqueue(t, s, event(2, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	if record(t, s, 1).State != "pending" {
		t.Fatal("manual intervention incorrectly exempted future membership")
	}
	f.group[1] = member{Status: "left"}
	enqueue(t, s, event(3, -100, 1, 1, "restricted", "left", *now))
	s.Sweep()
	if record(t, s, 1).State != "left" {
		t.Fatal("leave did not close round")
	}
}
func TestConcurrentCallbackAndTimeout(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	*now = now.Add(Timeout - time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Callback(callback(s, r, 1)) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.Sweep() }()
	wg.Wait()
	if record(t, s, 1).State != "passed" || f.calls["restrictChatMember"] != 2 || f.calls["banChatMember"] != 0 {
		t.Fatal("concurrent completion not idempotent")
	}
}
func TestPermissionChecksAndEventIsolation(t *testing.T) {
	s, f, now := fixture(t)
	f.group[999] = member{Status: "administrator", CanRestrictMembers: true}
	if s.Validate() == nil {
		t.Fatal("missing delete permission accepted")
	}
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	// Failed events survive restart, rather than disappearing on poll acknowledgement.
	f.fail["getChatMember:-200"] = 1
	s.Sweep()
	var count int64
	s.db.Model(&model.JoinVerificationEvent{}).Where("done = ?", false).Count(&count)
	if count != 1 {
		t.Fatal("failed event not durable")
	}
	s.Sweep()
	s.db.Model(&model.JoinVerificationEvent{}).Where("done = ?", false).Count(&count)
	if count != 0 || record(t, s, 1).State != "pending" {
		t.Fatal("durable event did not recover")
	}
}

func TestCleanupFailureDoesNotBlockReverification(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	r := record(t, s, 1)
	messageID := sharedPrompt(t, s).MessageID
	f.channel[1] = member{Status: "member"}
	if !strings.Contains(s.Callback(callback(s, r, 1)), "验证通过") {
		t.Fatal("cleanup failure blocked success")
	}
	s.Sweep()
	*now = now.Add(sharedPromptIdleGrace)
	f.fail["deleteMessage"] = 1
	s.Sweep()
	f.channel[1] = member{Status: "left"}
	s.Check(1)
	next := record(t, s, 1)
	s.Sweep()
	if next.State != "pending" || next.Round == r.Round || sharedPrompt(t, s).MessageID != messageID || f.calls["sendMessage"] != 2 {
		t.Fatal("old prompt blocked new round")
	}
}

func TestSharedPromptCoalescesBurstAndUsesCurrentUser(t *testing.T) {
	s, f, now := fixture(t)
	for id := int64(1); id <= 40; id++ {
		f.group[id] = member{Status: "member"}
		enqueue(t, s, event(int(id), -100, id, id, "left", "member", *now))
	}
	s.Sweep()
	prompt := sharedPrompt(t, s)
	if prompt.MessageID == 0 || prompt.PendingCount != 40 || f.calls["sendMessage"] != 1 {
		t.Fatalf("burst created multiple group prompts: prompt=%+v sends=%d", prompt, f.calls["sendMessage"])
	}
	if got := f.requests[len(f.requests)-1].params["reply_markup"]; !strings.Contains(got, "?start=jv_continue") {
		t.Fatalf("group entry is not reusable: %s", got)
	}
	before := f.calls["sendMessage"]
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	restarted.Sweep()
	if f.calls["sendMessage"] != before || sharedPrompt(t, restarted).MessageID != prompt.MessageID {
		t.Fatal("restart duplicated shared group prompt")
	}
	*now = now.Add(5 * time.Second)
	for id := int64(41); id <= 42; id++ {
		f.group[id] = member{Status: "member"}
		enqueue(t, restarted, event(int(id), -100, id, id, "left", "member", *now))
	}
	restarted.Sweep()
	if f.calls["sendMessage"] != before || sharedPrompt(t, restarted).MessageID != prompt.MessageID {
		t.Fatal("new arrivals created another group prompt")
	}
	for _, req := range f.requests {
		if req.method == "editMessageText" && req.params["chat_id"] == "-100" {
			t.Fatal("shared group prompt was edited before the interval elapsed")
		}
	}
	entry := &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "/start jv_continue"}
	if got := restarted.Start(entry); got != "" {
		t.Fatal(got)
	}
	second := record(t, restarted, 2)
	if second.PrivateMessageID == 0 || second.Confirmed || second.State != "pending" {
		t.Fatal("shared entry did not open the second member's own panel")
	}
	if got := restarted.Callback(callback(restarted, record(t, restarted, 1), 2)); !strings.Contains(got, "本人") {
		t.Fatal("another member's private callback was accepted", got)
	}
	*now = now.Add(sharedPromptEditInterval)
	f.channel[1] = member{Status: "member"}
	if got := restarted.Callback(callback(restarted, record(t, restarted, 1), 1)); !strings.Contains(got, "验证通过") {
		t.Fatal(got)
	}
	restarted.Sweep()
	var groupEdits int
	for _, req := range f.requests {
		if req.method == "editMessageText" && req.params["chat_id"] == "-100" {
			groupEdits++
		}
	}
	if sharedPrompt(t, restarted).PendingCount != 41 || groupEdits != 1 || f.deleted != 0 {
		t.Fatal("shared prompt was not edited after one member passed")
	}
}

func TestSharedPromptReusesEntryDuringIdleGrace(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	if got := s.Callback(callback(s, r, 1)); !strings.Contains(got, "验证通过") {
		t.Fatal(got)
	}
	s.Sweep()
	idle := sharedPrompt(t, s)
	if idle.MessageID != first.MessageID || idle.PendingCount != 0 || idle.IdleSince.IsZero() || f.deleted != 0 {
		t.Fatalf("idle entry was not retained for a short burst: %+v", idle)
	}
	*now = now.Add(sharedPromptIdleGrace / 2)
	f.group[2] = member{Status: "member"}
	s.Check(2)
	s.Sweep()
	reused := sharedPrompt(t, s)
	if reused.MessageID != first.MessageID || reused.PendingCount != 1 || !reused.IdleSince.IsZero() {
		t.Fatalf("the next burst did not reuse the entry: %+v", reused)
	}
	*now = now.Add(Timeout)
	s.Sweep()
	*now = now.Add(sharedPromptIdleGrace)
	s.Sweep()
	if sharedPrompt(t, s).MessageID != 0 || f.deleted != 1 {
		t.Fatal("idle entry was not deleted after the grace period")
	}
}

func TestSharedPromptRotatesDuringContinuousArrivals(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	*now = now.Add(sharedPromptMaxAge)
	f.group[2] = member{Status: "member"}
	s.Check(2)
	s.Sweep()
	second := sharedPrompt(t, s)
	if second.MessageID == first.MessageID || second.MessageID == 0 || f.deleted != 1 || f.calls["sendMessage"] != 2 {
		t.Fatalf("long-lived entry was not rotated: old=%+v new=%+v", first, second)
	}
	if !second.MessageCreatedAt.Equal(*now) || second.PendingCount != 1 {
		t.Fatalf("replacement entry has the wrong lifetime or count: %+v", second)
	}
}

func TestSharedPromptRefreshesOnlyOnJoinWhenBuried(t *testing.T) {
	for _, subscribed := range []bool{false, true} {
		t.Run(strconv.FormatBool(subscribed), func(t *testing.T) {
			s, f, now := fixture(t)
			s.Check(1)
			s.Sweep()
			first := sharedPrompt(t, s)
			deadline := record(t, s, 1).Deadline
			// A later chat or photo buries the entry, but cannot trigger a resend.
			f.messageID += 10
			if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
				t.Fatal(err)
			}
			s.Sweep()
			if f.calls["sendMessage"] != 1 || f.deleted != 0 {
				t.Fatal("ordinary traffic refreshed the reminder without a join")
			}
			*now = now.Add(5 * time.Second)
			for id := int64(2); id <= 5; id++ {
				f.group[id] = member{Status: "member"}
				if subscribed {
					f.channel[id] = member{Status: "member"}
				}
				enqueue(t, s, event(int(id), s.groupID, id, id, "left", "member", *now))
			}
			s.Sweep()
			next := sharedPrompt(t, s)
			if next.MessageID <= first.MessageID || next.RefreshRequested || f.deleted != 1 || f.calls["sendMessage"] != 2 {
				t.Fatalf("join burst did not replace the buried entry once: %+v", next)
			}
			if !record(t, s, 1).Deadline.Equal(deadline) {
				t.Fatal("refresh extended an existing verification deadline")
			}
			// The request has been consumed: later chat alone cannot refresh again.
			f.messageID++
			if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
				t.Fatal(err)
			}
			s.Sweep()
			if f.calls["sendMessage"] != 2 || f.deleted != 1 {
				t.Fatal("a handled join caused repeated refreshes")
			}
		})
	}
}

func TestSharedPromptBottomCheckSurvivesRestartAndIgnoresServiceMessages(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	for _, msg := range []*tgbotapi.Message{
		nil,
		{MessageID: first.MessageID + 10, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}},
		{MessageID: first.MessageID + 10, Chat: &tgbotapi.Chat{ID: -300, Type: "supergroup"}},
		{MessageID: first.MessageID + 10, Chat: &tgbotapi.Chat{ID: s.groupID}, NewChatMembers: []tgbotapi.User{{ID: 2}}},
		{MessageID: first.MessageID + 10, Chat: &tgbotapi.Chat{ID: s.groupID}, LeftChatMember: &tgbotapi.User{ID: 3}},
		{MessageID: first.MessageID + 10, Chat: &tgbotapi.Chat{ID: s.groupID}, PinnedMessage: &tgbotapi.Message{MessageID: 123}},
	} {
		if err := s.ObserveGroupMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	f.group[2] = member{Status: "member"}
	enqueue(t, restarted, event(2, s.groupID, 2, 2, "left", "member", *now))
	restarted.Sweep()
	if f.calls["sendMessage"] != 1 || f.deleted != 0 || sharedPrompt(t, s).MessageID != first.MessageID || sharedPrompt(t, s).RefreshRequested {
		t.Fatal("bottommost entry was unnecessarily replaced after restart or service messages")
	}
	// Once consumed, a join must not cause a delayed refresh on ordinary traffic.
	f.messageID += 20
	if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
		t.Fatal(err)
	}
	restarted.Sweep()
	if f.calls["sendMessage"] != 1 {
		t.Fatal("bottom check was not consumed on the join")
	}
}

func TestSharedPromptRefreshRetriesAfterRestartWithoutAnotherJoin(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	f.messageID += 10
	if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
		t.Fatal(err)
	}
	f.group[2] = member{Status: "member"}
	enqueue(t, s, event(2, s.groupID, 2, 2, "left", "member", *now))
	f.fail["deleteMessage"] = 1
	s.Sweep()
	if !sharedPrompt(t, s).RefreshRequested || sharedPrompt(t, s).MessageID != first.MessageID {
		t.Fatal("failed refresh lost its durable intent or old reminder")
	}
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	restarted.Sweep()
	if f.calls["deleteMessage"] != 1 || f.calls["sendMessage"] != 1 {
		t.Fatal("restart bypassed cleanup retry backoff")
	}
	*now = now.Add(sharedPromptRetireRetryInterval)
	restarted.Sweep()
	if sharedPrompt(t, s).MessageID == first.MessageID || sharedPrompt(t, s).RefreshRequested || f.calls["sendMessage"] != 2 {
		t.Fatal("failed refresh did not resume after restart")
	}
}

func TestGroupMessageOrderDoesNotRegressOnDelayedObservation(t *testing.T) {
	s, _, _ := fixture(t)
	for _, id := range []int{50, 20, 40} {
		if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: id, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
			t.Fatal(err)
		}
	}
	var activity model.JoinVerificationGroupActivity
	if err := s.db.First(&activity, "group_id = ?", s.groupID).Error; err != nil || activity.LastMessageID != 50 {
		t.Fatalf("delayed update overwrote newer message order: %+v err=%v", activity, err)
	}
}

func TestSharedPromptRefreshLostSendResponseDoesNotDuplicate(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	f.messageID += 10
	if err := s.ObserveGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}}); err != nil {
		t.Fatal(err)
	}
	f.group[2] = member{Status: "member"}
	enqueue(t, s, event(2, s.groupID, 2, 2, "left", "member", *now))
	f.lost["sendMessage"] = 1
	s.Sweep()
	if sharedPrompt(t, s).SendState != "uncertain" || f.deleted != 1 {
		t.Fatal("replacement send outcome was not recorded as uncertain")
	}
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	f.group[3] = member{Status: "member"}
	enqueue(t, restarted, event(3, s.groupID, 3, 3, "left", "member", *now))
	restarted.Sweep()
	if f.calls["sendMessage"] != 2 {
		t.Fatal("another join repeated an uncertain replacement send")
	}
}

func TestSharedPromptRetireFailureRetriesAndTerminalErrorStops(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	first.MessageCreatedAt = now.Add(-sharedPromptMaxAge)
	if err := s.db.Save(&first).Error; err != nil {
		t.Fatal(err)
	}
	f.fail["deleteMessage"] = 1
	if err := s.syncSharedPrompt(); err == nil {
		t.Fatal("transient delete failure was ignored")
	}
	if err := s.syncSharedPrompt(); err != nil || f.calls["deleteMessage"] != 1 || sharedPrompt(t, s).MessageID != first.MessageID {
		t.Fatal("retirement retried too soon or lost the usable entry")
	}
	*now = now.Add(sharedPromptRetireRetryInterval)
	f.fail["deleteMessage"] = 1
	f.failWith = map[string]error{"deleteMessage": errors.New("Bad Request: message can't be deleted")}
	if err := s.syncSharedPrompt(); err != nil {
		t.Fatal(err)
	}
	second := sharedPrompt(t, s)
	if second.MessageID == first.MessageID || second.MessageID == 0 || f.calls["deleteMessage"] != 2 || f.calls["sendMessage"] != 2 {
		t.Fatalf("terminal delete failure blocked replacement: %+v", second)
	}
	var closed bool
	for _, req := range f.requests {
		if req.method == "editMessageText" && req.params["message_id"] == strconv.Itoa(first.MessageID) && req.params["reply_markup"] == `{"inline_keyboard":[]}` {
			closed = true
		}
	}
	if !closed {
		t.Fatal("old reminder buttons were not removed")
	}
	s.Sweep()
	if f.calls["deleteMessage"] != 2 {
		t.Fatal("terminal delete failure was retried")
	}
}

func TestSharedPromptKeepsOldIDUntilButtonsAreClosed(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	s.Sweep()
	first := sharedPrompt(t, s)
	first.MessageCreatedAt = now.Add(-sharedPromptMaxAge)
	if err := s.db.Save(&first).Error; err != nil {
		t.Fatal(err)
	}
	f.fail["deleteMessage"] = 1
	f.failWith = map[string]error{"deleteMessage": errors.New("Bad Request: message can't be deleted")}
	f.fail["editMessageText"] = 1
	if err := s.syncSharedPrompt(); err == nil {
		t.Fatal("failed button removal was reported as success")
	}
	closing := sharedPrompt(t, s)
	if closing.MessageID != first.MessageID || !closing.CloseByEdit || f.calls["sendMessage"] != 1 {
		t.Fatalf("failed closure lost the old message or sent another: %+v", closing)
	}
	if err := s.syncSharedPrompt(); err != nil || f.calls["editMessageText"] != 1 {
		t.Fatal("failed closure retried before the backoff")
	}
	*now = now.Add(sharedPromptRetireRetryInterval)
	if err := s.syncSharedPrompt(); err != nil {
		t.Fatal(err)
	}
	second := sharedPrompt(t, s)
	if second.MessageID == first.MessageID || second.MessageID == 0 || second.CloseByEdit || f.calls["sendMessage"] != 2 || f.calls["deleteMessage"] != 1 {
		t.Fatalf("closed prompt was not replaced exactly once: %+v", second)
	}
}

func TestSharedPromptLostSendResponseIsNotRetriedAfterRestart(t *testing.T) {
	s, f, _ := fixture(t)
	f.lost["sendMessage"] = 1
	s.Check(1)
	s.Sweep()
	unknown := sharedPrompt(t, s)
	if unknown.MessageID != 0 || unknown.SendState != "uncertain" || f.calls["sendMessage"] != 1 {
		t.Fatalf("lost send response was not recorded as uncertain: %+v", unknown)
	}
	s.Sweep()
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	restarted.Sweep()
	if f.calls["sendMessage"] != 1 || sharedPrompt(t, restarted).SendState != "uncertain" {
		t.Fatal("uncertain send was repeated after restart")
	}
}

func TestSharedPromptRecoversKnownMessageIDAfterSaveFailure(t *testing.T) {
	s, f, _ := fixture(t)
	if err := s.db.Exec(`CREATE TRIGGER fail_prompt_confirmation BEFORE UPDATE ON join_verification_prompts
		WHEN NEW.message_id <> 0 BEGIN SELECT RAISE(FAIL, 'simulated prompt save failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	s.Check(1)
	s.Sweep()
	unsaved := sharedPrompt(t, s)
	if unsaved.MessageID != 0 || unsaved.SendState != "sending" || f.calls["sendMessage"] != 1 {
		t.Fatalf("send intent was not durable before the failed save: %+v", unsaved)
	}
	if err := s.db.Exec("DROP TRIGGER fail_prompt_confirmation").Error; err != nil {
		t.Fatal(err)
	}
	s.Sweep()
	recovered := sharedPrompt(t, s)
	if recovered.MessageID == 0 || recovered.SendState != "" || f.calls["sendMessage"] != 1 {
		t.Fatalf("known Telegram message was not recovered without resending: %+v", recovered)
	}
}

func TestSharedPromptRecoveredReplacementConsumesOnlyItsRefresh(t *testing.T) {
	for _, anotherJoin := range []bool{false, true} {
		t.Run(strconv.FormatBool(anotherJoin), func(t *testing.T) {
			s, f, now := fixture(t)
			s.Check(1)
			s.Sweep()
			f.messageID += 10
			s.QueueGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}})
			f.group[2] = member{Status: "member"}
			enqueue(t, s, event(2, s.groupID, 2, 2, "left", "member", *now))
			f.beforeCall = func(method string, _ tgbotapi.Params) {
				if method == "sendMessage" {
					if err := s.db.Exec(`CREATE TRIGGER fail_replacement_save BEFORE UPDATE ON join_verification_prompts
						WHEN NEW.message_id <> 0 BEGIN SELECT RAISE(FAIL, 'simulated prompt save failure'); END`).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			s.Sweep()
			if f.calls["sendMessage"] != 2 || s.unpersistedPromptID == 0 {
				t.Fatal("replacement did not reach the failed confirmation write")
			}
			f.beforeCall = nil
			if err := s.db.Exec("DROP TRIGGER fail_replacement_save").Error; err != nil {
				t.Fatal(err)
			}
			f.messageID += 10
			s.QueueGroupMessage(&tgbotapi.Message{MessageID: f.messageID, Chat: &tgbotapi.Chat{ID: s.groupID}})
			if anotherJoin {
				f.group[3] = member{Status: "member"}
				enqueue(t, s, event(3, s.groupID, 3, 3, "left", "member", *now))
			}
			s.Sweep()
			wantSends := 2
			if anotherJoin {
				wantSends++
			}
			if p := sharedPrompt(t, s); p.RefreshRequested || p.SendState != "" || f.calls["sendMessage"] != wantSends || f.deleted != wantSends-1 {
				t.Fatalf("recovery repeated a consumed refresh or lost a new join: prompt=%+v sends=%d deletes=%d", p, f.calls["sendMessage"], f.deleted)
			}
		})
	}
}

func TestQueuedGroupActivityRetriesWithoutLosingNewerMessages(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	s.Sweep()
	if err := s.db.Exec(`CREATE TRIGGER fail_activity_write BEFORE INSERT ON join_verification_group_activities
		BEGIN SELECT RAISE(FAIL, 'simulated activity write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{50, 20, 40} {
		s.QueueGroupMessage(&tgbotapi.Message{MessageID: id, Chat: &tgbotapi.Chat{ID: s.groupID}})
	}
	// Queue a newer message during the failed write; retrying 50 must retain 70.
	if err := s.db.Callback().Create().Before("gorm:create").Register("test:observe_during_write", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.JoinVerificationGroupActivity); ok {
			s.QueueGroupMessage(&tgbotapi.Message{MessageID: 70, Chat: &tgbotapi.Chat{ID: s.groupID}})
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSharedPrompt(); err == nil {
		t.Fatal("activity write unexpectedly succeeded")
	}
	if err := s.db.Callback().Create().Remove("test:observe_during_write"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DROP TRIGGER fail_activity_write").Error; err != nil {
		t.Fatal(err)
	}
	s.Sweep()
	var activity model.JoinVerificationGroupActivity
	if err := s.db.First(&activity, "group_id = ?", s.groupID).Error; err != nil || activity.LastMessageID != 70 {
		t.Fatalf("failed observation was lost or overwrote newer traffic: %+v err=%v", activity, err)
	}
	if f.calls["sendMessage"] != 1 || f.deleted != 0 {
		t.Fatal("retrying activity triggered a refresh without a join")
	}
	// With no further traffic, the successful observation is not written again.
	if err := s.db.Exec(`CREATE TRIGGER reject_redundant_activity BEFORE INSERT ON join_verification_group_activities
		BEGIN SELECT RAISE(FAIL, 'unexpected redundant activity write'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.syncSharedPrompt(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedPromptSaveFailureBeforeRestartDoesNotResend(t *testing.T) {
	s, f, _ := fixture(t)
	if err := s.db.Exec(`CREATE TRIGGER fail_prompt_confirmation BEFORE UPDATE ON join_verification_prompts
		WHEN NEW.message_id <> 0 BEGIN SELECT RAISE(FAIL, 'simulated prompt save failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	s.Check(1)
	s.Sweep()
	if err := s.db.Exec("DROP TRIGGER fail_prompt_confirmation").Error; err != nil {
		t.Fatal(err)
	}
	restarted := New(s.db, f, s.groupID, s.channelID, s.botID, s.channelURL, nil, s.botUsername)
	restarted.now = s.now
	restarted.Sweep()
	restarted.Sweep()
	unknown := sharedPrompt(t, restarted)
	if unknown.MessageID != 0 || unknown.SendState != "uncertain" || f.calls["sendMessage"] != 1 {
		t.Fatalf("restart repeated an unpersisted send: %+v", unknown)
	}
}

func TestShortOriginalRestrictionDoesNotBecomePermanent(t *testing.T) {
	s, f, now := fixture(t)
	f.group[1] = member{Status: "restricted", IsMember: true, permissions: permissions{SendMessages: true}, UntilDate: now.Add(20 * time.Second).Unix()}
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	s.Callback(callback(s, r, 1))
	if record(t, s, 1).State == "passed" {
		t.Fatal("short restriction must not be sent as an indefinite Telegram restriction")
	}
	*now = now.Add(21 * time.Second)
	s.Sweep()
	if record(t, s, 1).State != "passed" || !f.group[1].SendPhotos || f.group[1].UntilDate != 0 {
		t.Fatal("expired original restriction not cleared")
	}
}

func TestRejoinWhileUnbanResponseWasLost(t *testing.T) {
	s, f, now := fixture(t)
	s.Check(1)
	old := record(t, s, 1)
	*now = now.Add(Timeout)
	f.lost["unbanChatMember"] = 1
	s.Sweep()
	if record(t, s, 1).State != "unbanning" {
		t.Fatal("test did not leave pending unban")
	}
	f.group[1] = member{Status: "member"}
	enqueue(t, s, event(10, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	r := record(t, s, 1)
	if r.State != "pending" || r.Round == old.Round || r.Deadline.Sub(*now) != Timeout {
		t.Fatal("rejoin during uncertain unban escaped verification")
	}
}
func TestModeratorChangesBeforeRestrictRetry(t *testing.T) {
	s, f, _ := fixture(t)
	f.fail["restrictChatMember"] = 1
	s.Check(1)
	f.group[1] = member{Status: "restricted", IsMember: true, permissions: permissions{SendMessages: true}}
	before := f.calls["restrictChatMember"]
	s.Sweep()
	if record(t, s, 1).State != "manual" || f.calls["restrictChatMember"] != before {
		t.Fatal("restrict retry overwrote moderator")
	}
}

func startMessage(r model.JoinVerification, id int64) *tgbotapi.Message {
	return &tgbotapi.Message{From: &tgbotapi.User{ID: id}, Chat: &tgbotapi.Chat{ID: id, Type: "private"}, Text: "/start jv_" + r.Round}
}

func TestPrivateEntryOwnershipAndNoAutomaticPass(t *testing.T) {
	s, f, now := fixture(t)
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	r := record(t, s, 1)
	var last struct {
		method string
		params tgbotapi.Params
	}
	for _, req := range f.requests {
		if req.method == "sendMessage" {
			last = req
		}
	}
	if last.method != "sendMessage" || last.params["chat_id"] != "-100" || !strings.Contains(last.params["reply_markup"], "https://t.me/testbot?start=jv_continue") || strings.Contains(last.params["reply_markup"], "callback_data") {
		t.Fatalf("incorrect group entry: %+v", last)
	}
	if hint := s.Start(startMessage(r, 2)); !strings.Contains(hint, "本人") {
		t.Fatal("forwarded link admitted another user", hint)
	}
	f.channel[1] = member{Status: "member"}
	enqueue(t, s, event(2, -200, 1, 1, "left", "member", *now))
	s.Sweep()
	if ok, _ := s.Check(1); ok {
		t.Fatal("business access bypassed private confirmation")
	}
	if hint := s.Start(startMessage(r, 1)); hint != "" {
		t.Fatal(hint)
	}
	panel := record(t, s, 1)
	if panel.State != "pending" || panel.Confirmed || panel.PrivateMessageID == 0 || f.group[1].SendMessages {
		t.Fatal("opening panel admitted user")
	}
	*now = now.Add(31 * time.Second)
	before := f.calls["sendMessage"]
	if hint := s.Start(startMessage(r, 1)); hint != "" {
		t.Fatal(hint)
	}
	last = f.requests[len(f.requests)-1]
	if f.calls["sendMessage"] != before || !record(t, s, 1).Deadline.Equal(r.Deadline) || !strings.Contains(last.params["text"], "89 秒") {
		t.Fatal("reopening extended deadline or duplicated panel")
	}
	groupCallback := &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: 1}, Data: CallbackPrefix + r.Round, Message: &tgbotapi.Message{MessageID: sharedPrompt(t, s).MessageID, Chat: &tgbotapi.Chat{ID: s.groupID}}}
	if hint := s.Callback(groupCallback); !strings.Contains(hint, "无效") {
		t.Fatal(hint)
	}
	wrongPanel := callback(s, r, 1)
	wrongPanel.Message.MessageID++
	if hint := s.Callback(wrongPanel); !strings.Contains(hint, "失效") {
		t.Fatal(hint)
	}
	if !strings.Contains(s.Callback(callback(s, r, 1)), "验证通过") {
		t.Fatal("private confirmation failed")
	}
	latest := record(t, s, 1)
	s.Sweep()
	if latest.State != "passed" || sharedPrompt(t, s).MessageID == 0 || latest.PrivateMessageID != 0 {
		t.Fatal("successful verification did not clean prompts")
	}
	*now = now.Add(sharedPromptIdleGrace)
	s.Sweep()
	if sharedPrompt(t, s).MessageID != 0 {
		t.Fatal("idle shared prompt was not removed")
	}
	var cleaned bool
	for _, req := range f.requests {
		if req.method == "editMessageText" && req.params["chat_id"] == "1" && strings.Contains(req.params["text"], "验证通过，已恢复验证前的群权限") && req.params["reply_markup"] == `{"inline_keyboard":[]}` {
			cleaned = true
		}
	}
	if !cleaned {
		t.Fatal("private success panel still has buttons")
	}
}

func TestConfirmationSurvivesRestartButUnsubscribeRevokesIt(t *testing.T) {
	for _, unsubscribe := range []bool{false, true} {
		t.Run(strconv.FormatBool(unsubscribe), func(t *testing.T) {
			s, f, now := fixture(t)
			s.Check(1)
			r := record(t, s, 1)
			f.channel[1] = member{Status: "member"}
			f.fail["restrictChatMember"] = 1
			s.Callback(callback(s, r, 1))
			if !record(t, s, 1).Confirmed || record(t, s, 1).State != "restoring" {
				t.Fatal("confirmation intent was not durable")
			}
			restarted := New(s.db, f, -100, -200, 999, s.channelURL, nil, s.botUsername)
			restarted.now = s.now
			if unsubscribe {
				f.channel[1] = member{Status: "left"}
				enqueue(t, restarted, event(10, -200, 1, 1, "member", "left", *now))
				restarted.Sweep()
				f.channel[1] = member{Status: "member"}
			}
			restarted.Sweep()
			latest := record(t, restarted, 1)
			if unsubscribe {
				if latest.Confirmed || latest.State != "pending" || f.group[1].SendMessages || !latest.Deadline.Equal(r.Deadline) {
					t.Fatal("unsubscribe did not revoke confirmation")
				}
				if !strings.Contains(restarted.Callback(callback(restarted, latest, 1)), "验证通过") {
					t.Fatal("reconfirmation failed")
				}
			} else if latest.State != "passed" || !f.group[1].SendMessages {
				t.Fatal("restart lost valid confirmation")
			}
		})
	}
}

func TestPromptReasonsAndAlreadySubscribedExemption(t *testing.T) {
	for _, reason := range []string{"join", "unsubscribe", "business"} {
		t.Run(reason, func(t *testing.T) {
			s, f, _ := fixture(t)
			if _, err := s.ensure(1, reason); err != nil {
				t.Fatal(err)
			}
			r := record(t, s, 1)
			s.Sweep()
			groupText := f.requests[len(f.requests)-1].params["text"]
			if !strings.Contains(groupText, "待完成入群验证") || strings.Contains(groupText, s.reason(&r)) {
				t.Fatal(groupText)
			}
			if hint := s.Start(startMessage(r, 1)); hint != "" {
				t.Fatal(hint)
			}
			if !strings.Contains(f.requests[len(f.requests)-1].params["text"], s.reason(&r)) {
				t.Fatal("private reason missing")
			}
		})
	}
	s, f, now := fixture(t)
	f.channel[1] = member{Status: "member"}
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	if f.calls["restrictChatMember"] != 0 || f.calls["sendMessage"] != 0 {
		t.Fatal("already subscribed newcomer should be exempt")
	}
}

func TestPrivatePanelCleanupRetryDoesNotBlockNewRound(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	cb := callback(s, r, 1)
	f.fail["editMessageText"] = 1
	if hint := s.Callback(cb); !strings.Contains(hint, "验证通过") {
		t.Fatal(hint)
	}
	if record(t, s, 1).PrivateMessageID == 0 {
		t.Fatal("failed cleanup was not retained")
	}
	f.channel[1] = member{Status: "left"}
	s.Check(1)
	latest := record(t, s, 1)
	if latest.State != "pending" || latest.Round == r.Round {
		t.Fatal("cleanup blocked new round")
	}
	if hint := s.Callback(cb); !strings.Contains(hint, "失效") {
		t.Fatal("old private button admitted user", hint)
	}
}

func TestPrivateCallbackQueryFailureAndDeadlineBoundary(t *testing.T) {
	for _, crossDeadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(crossDeadline), func(t *testing.T) {
			s, f, now := fixture(t)
			s.Check(1)
			r := record(t, s, 1)
			cb := callback(s, r, 1)
			f.channel[1] = member{Status: "member"}
			if crossDeadline {
				*now = r.Deadline.Add(-time.Second)
				f.beforeCall = func(method string, p tgbotapi.Params) {
					if method == "getChatMember" && p["chat_id"] == "-200" {
						*now = r.Deadline
					}
				}
			} else {
				f.fail["getChatMember:-200"] = 1
			}
			hint := s.Callback(cb)
			if crossDeadline && !strings.Contains(hint, "超时") {
				t.Fatal(hint)
			}
			if !crossDeadline && !strings.Contains(hint, "无法确认") {
				t.Fatal(hint)
			}
			latest := record(t, s, 1)
			if latest.Confirmed || latest.State != "pending" || f.group[1].SendMessages {
				t.Fatal("failed/late query authorized restore")
			}
		})
	}
}

func TestPrivateCleanupRetriedAfterSuccess(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	cb := callback(s, r, 1)
	f.fail["editMessageText"] = 1
	s.Callback(cb)
	if record(t, s, 1).PrivateMessageID == 0 {
		t.Fatal("missing cleanup retry intent")
	}
	s.Sweep()
	if record(t, s, 1).PrivateMessageID != 0 {
		t.Fatal("private cleanup not retried")
	}
}

func TestAccessMessageProvidesCurrentRoundWithoutExtendingDeadline(t *testing.T) {
	s, _, now := fixture(t)
	ok, hint := s.Check(1)
	if ok {
		t.Fatal("unverified user admitted")
	}
	r := record(t, s, 1)
	*now = now.Add(30 * time.Second)
	msg := s.AccessMessage(1, 1, hint)
	keyboard, ok := msg.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	if !ok || len(keyboard.InlineKeyboard) != 1 {
		t.Fatalf("missing verification keyboard: %+v", msg)
	}
	button := keyboard.InlineKeyboard[0][0]
	if button.URL == nil || *button.URL != "https://t.me/testbot?start=jv_"+r.Round || button.CallbackData != nil {
		t.Fatalf("must open owner verification panel before confirmation: %+v", button)
	}
	if !strings.Contains(msg.Text, "90 秒") {
		t.Fatalf("missing current countdown: %s", msg.Text)
	}
	if got := record(t, s, 1); got.Round != r.Round || !got.Deadline.Equal(r.Deadline) {
		t.Fatalf("access message changed verification round: %+v", got)
	}
	*now = r.Deadline
	msg = s.AccessMessage(1, 1, hint)
	if msg.ReplyMarkup != nil || !strings.Contains(msg.Text, "超时") {
		t.Fatalf("expired round must not offer a verification button: %+v", msg)
	}
}

func TestAccessMessageDoesNotCreateRoundForNonmemberOrReadError(t *testing.T) {
	s, f, _ := fixture(t)
	delete(f.group, 1)
	ok, hint := s.Check(1)
	if ok {
		t.Fatal("nonmember admitted")
	}
	msg := s.AccessMessage(1, 1, hint)
	data, err := json.Marshal(msg.ReplyMarkup)
	if err != nil || !strings.Contains(string(data), "?start=jv_continue") || !strings.Contains(string(data), "https://t.me/example") || !strings.Contains(msg.Text, "联系管理员") {
		t.Fatalf("nonmember must get join guidance, not a nonexistent verification round: %+v %v", msg, err)
	}
	var count int64
	if err := s.db.Model(&model.JoinVerification{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("nonmember access created a round: %d %v", count, err)
	}
	f.fail["getChatMember:-100"] = 1
	_, hint = s.Check(1)
	msg = s.AccessMessage(1, 1, hint)
	if msg.ReplyMarkup != nil || !strings.Contains(msg.Text, "稍后重试") {
		t.Fatalf("read errors must remain retryable: %+v", msg)
	}
}

func privateEntry(text string) *tgbotapi.Message {
	return &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, Text: text}
}

func TestReusableEntryCompletesVerificationWithoutGroupPrompt(t *testing.T) {
	s, f, now := fixture(t)
	delete(f.group, 1)
	if got := s.Start(privateEntry("/start jv_continue")); !strings.Contains(got, "请先加入") {
		t.Fatal(got)
	}
	var count int64
	s.db.Model(&model.JoinVerification{}).Count(&count)
	if count != 0 || f.calls["restrictChatMember"] != 0 {
		t.Fatal("outside entry created a punitive round")
	}
	f.group[1] = member{Status: "member"}
	if got := s.Start(privateEntry("/start jv_continue")); got != "" {
		t.Fatal(got)
	}
	r := record(t, s, 1)
	if r.State != "pending" || r.PrivateMessageID == 0 || !r.Muted {
		t.Fatalf("no private panel: %+v", r)
	}
	*now = now.Add(30 * time.Second)
	f.channel[1] = member{Status: "member"}
	if got := s.Start(privateEntry("入群验证")); got != "" {
		t.Fatal(got)
	}
	latest := record(t, s, 1)
	if latest.Round != r.Round || !latest.Deadline.Equal(r.Deadline) || latest.Confirmed || latest.State != "pending" {
		t.Fatalf("reopen bypassed confirmation or extended deadline: %+v", latest)
	}
	if got := s.Callback(callback(s, latest, 1)); !strings.Contains(got, "验证通过") {
		t.Fatal(got)
	}
	if !f.group[1].SendMessages {
		t.Fatal("member stayed muted")
	}
}

func TestRejoinRetainedBotMuteRestoresOriginalPermissions(t *testing.T) {
	for _, following := range []bool{false, true} {
		for _, restricted := range []bool{false, true} {
			t.Run(strconv.FormatBool(following)+"/originalRestriction="+strconv.FormatBool(restricted), func(t *testing.T) {
				s, f, now := fixture(t)
				original := permissions{SendMessages: true}
				if restricted {
					f.group[1] = member{Status: "restricted", IsMember: true, permissions: original, UntilDate: now.Add(time.Hour).Unix()}
				}
				s.Check(1)
				first := record(t, s, 1)
				f.group[1] = member{Status: "left"}
				enqueue(t, s, event(1, -100, 1, 1, "restricted", "left", *now))
				s.Sweep()
				if got := record(t, s, 1); got.State != "left" || !got.Muted {
					t.Fatalf("lost mute evidence: %+v", got)
				}
				f.group[1] = member{Status: "restricted", IsMember: true}
				if following {
					f.channel[1] = member{Status: "member"}
				}
				enqueue(t, s, event(2, -100, 1, 1, "left", "restricted", *now))
				s.Sweep()
				r := record(t, s, 1)
				if r.OriginalPermissions != first.OriginalPermissions || r.OriginalUntil != first.OriginalUntil {
					t.Fatal("Bot mute replaced original snapshot")
				}
				if !following {
					if r.State != "pending" {
						t.Fatalf("missing new round: %+v", r)
					}
					f.channel[1] = member{Status: "member"}
					if got := s.Callback(callback(s, r, 1)); !strings.Contains(got, "验证通过") {
						t.Fatal(got)
					}
				}
				if got := record(t, s, 1); got.State != "passed" {
					t.Fatalf("not restored: %+v", got)
				}
				want := f.defaults
				if restricted {
					want = original
				}
				if f.group[1].permissions != want {
					t.Fatalf("wrong permissions: %+v want %+v", f.group[1], want)
				}
				if ok, _ := s.Check(1); !ok {
					t.Fatal("verified member blocked")
				}
			})
		}
	}
}

func TestReusableEntryPreservesUnknownAndManualMutes(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(strconv.FormatBool(manual), func(t *testing.T) {
			s, f, now := fixture(t)
			if manual {
				s.Check(1)
				enqueue(t, s, event(1, -100, 1, 42, "restricted", "restricted", *now))
				s.Sweep()
				if got := record(t, s, 1); got.State != "manual" || got.Muted {
					t.Fatalf("manual restriction retains Bot ownership: %+v", got)
				}
				f.group[1] = member{Status: "left"}
				enqueue(t, s, event(2, -100, 1, 1, "restricted", "left", *now))
				s.Sweep()
			}
			f.group[1] = member{Status: "restricted", IsMember: true}
			f.channel[1] = member{Status: "member"}
			before := f.calls["restrictChatMember"]
			if manual {
				enqueue(t, s, event(3, -100, 1, 1, "left", "restricted", *now))
				s.Sweep()
			}
			s.Start(privateEntry("入群验证"))
			if f.calls["restrictChatMember"] != before || f.group[1].SendMessages {
				t.Fatal("lifted a moderator or unknown restriction")
			}
		})
	}
}

func TestReusableEntryHandlesAbsentActiveMemberAndQueryErrors(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	f.group[1] = member{Status: "left"}
	if got := s.Start(privateEntry("入群验证")); !strings.Contains(got, "请先加入") {
		t.Fatal(got)
	}
	if record(t, s, 1).State != "left" {
		t.Fatal("absent member kept active panel")
	}
	f.fail["getChatMember:-100"] = 1
	if got := s.Start(privateEntry("入群验证")); !strings.Contains(got, "稍后重试") {
		t.Fatal(got)
	}
}

func TestReusableEntryPreservesPendingUnban(t *testing.T) {
	s, f, _ := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	r.State = "unbanning"
	if err := s.save(&r); err != nil {
		t.Fatal(err)
	}
	f.group[1] = member{Status: "kicked"}
	s.Start(privateEntry("入群验证"))
	if got := record(t, s, 1); got.State != "unbanning" {
		t.Fatalf("private entry discarded pending unban: %+v", got)
	}
	s.Sweep()
	if f.group[1].Status != "left" || record(t, s, 1).State != "expired" {
		t.Fatal("pending unban did not settle")
	}
}

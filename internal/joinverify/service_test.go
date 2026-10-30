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
	group     map[int64]member
	channel   map[int64]member
	defaults  permissions
	calls     map[string]int
	fail      map[string]int
	lost      map[string]int // Apply mutation, then lose response.
	messageID int
	deleted   int
}

func (f *fakeAPI) MakeRequest(method string, p tgbotapi.Params) (*tgbotapi.APIResponse, error) {
	f.calls[method]++
	key := method
	if method == "getChatMember" {
		key += ":" + p["chat_id"]
	}
	if f.fail[key] > 0 {
		f.fail[key]--
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
	if err = db.AutoMigrate(&model.JoinVerification{}, &model.JoinVerificationEvent{}, &model.AuditLog{}, &model.AutoDeleteMsg{}); err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{group: map[int64]member{1: {Status: "member"}, 999: {Status: "administrator", CanRestrictMembers: true, CanDeleteMessages: true}}, channel: map[int64]member{999: {Status: "administrator"}}, defaults: permissions{SendMessages: true, SendAudios: true, SendPhotos: true, SendPolls: true, InviteUsers: true}, calls: map[string]int{}, fail: map[string]int{}, lost: map[string]int{}}
	s := New(db, f, -100, -200, 999, "https://t.me/example", nil)
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
func callback(r model.JoinVerification, id int64) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{From: &tgbotapi.User{ID: id}, Data: CallbackPrefix + r.Round, Message: &tgbotapi.Message{MessageID: r.MessageID, Chat: &tgbotapi.Chat{ID: r.GroupID}}}
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
	if r.State != "pending" || r.Deadline.Sub(*now) != Timeout || f.group[1].permissions.SendMessages || r.MessageID == 0 {
		t.Fatalf("bad initial verification: %+v", r)
	}
	*now = now.Add(time.Minute)
	enqueue(t, s, event(1, -100, 1, 1, "left", "member", *now))
	s.Sweep()
	if !record(t, s, 1).Deadline.Equal(r.Deadline) {
		t.Fatal("duplicate event extended deadline")
	}
	if got := s.Callback(callback(r, 2)); !strings.Contains(got, "本人") {
		t.Fatal(got)
	}
	if got := s.Callback(callback(r, 1)); !strings.Contains(got, "尚未") {
		t.Fatal(got)
	}
	f.channel[1] = member{Status: "member"}
	if got := s.Callback(callback(r, 1)); !strings.Contains(got, "验证成功") {
		t.Fatal(got)
	}
	if record(t, s, 1).State != "passed" || !f.group[1].SendPhotos || f.deleted != 1 {
		t.Fatal("did not restore permissions and clean prompt")
	}
	before := f.calls["restrictChatMember"]
	s.Callback(callback(r, 1))
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
	if record(t, s, 1).State != "passed" {
		t.Fatal("follow event did not pass")
	}
	old := record(t, s, 1)
	f.channel[1] = member{Status: "left"}
	enqueue(t, s, event(2, -200, 1, 1, "member", "left", *now))
	s.Sweep()
	r := record(t, s, 1)
	if r.Round == old.Round || r.Reason != "unsubscribe" {
		t.Fatal("no new unsubscribe round")
	}
	*now = now.Add(Timeout)
	if got := s.Callback(callback(r, 1)); !strings.Contains(got, "超时") {
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
	if got := s.Callback(callback(r, 1)); !strings.Contains(got, "失效") {
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
			restarted := New(s.db, f, -100, -200, 999, s.channelURL, nil)
			restarted.now = s.now
			restarted.Sweep()
			latest := record(t, restarted, 1)
			if failure == "restrictChatMember" {
				if latest.State != "passed" || !f.group[1].SendMessages {
					t.Fatal("lost restrict response stranded member")
				}
			} else if latest.State != "expired" || f.group[1].Status != "left" {
				t.Fatalf("restart failed: before=%+v after=%+v", r, latest)
			}
		})
	}
}
func TestQueryFailureNeverKicksAndRestoresSubscribedAtExpiry(t *testing.T) {
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
	if record(t, s, 1).State != "passed" || f.calls["banChatMember"] != 0 {
		t.Fatal("expiry did not recheck subscription")
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
	s.Callback(callback(r, 1))
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
	s.Callback(callback(r, 1))
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
		go func() { defer wg.Done(); s.Callback(callback(r, 1)) }()
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
	s, f, _ := fixture(t)
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	f.fail["deleteMessage"] = 2
	if !strings.Contains(s.Callback(callback(r, 1)), "成功") {
		t.Fatal("cleanup failure blocked success")
	}
	f.channel[1] = member{Status: "left"}
	s.Check(1)
	next := record(t, s, 1)
	if next.State != "pending" || next.Round == r.Round || next.MessageID == r.MessageID {
		t.Fatal("old prompt blocked new round")
	}
	var n int64
	s.db.Model(&model.AutoDeleteMsg{}).Count(&n)
	if n != 1 {
		t.Fatal("old prompt not queued for cleanup")
	}
}
func TestShortOriginalRestrictionDoesNotBecomePermanent(t *testing.T) {
	s, f, now := fixture(t)
	f.group[1] = member{Status: "restricted", IsMember: true, permissions: permissions{SendMessages: true}, UntilDate: now.Add(20 * time.Second).Unix()}
	s.Check(1)
	r := record(t, s, 1)
	f.channel[1] = member{Status: "member"}
	s.Callback(callback(r, 1))
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

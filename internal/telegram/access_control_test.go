package telegram

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestIsUserInGroupFreshDoesNotUseStaleRegularCache(t *testing.T) {
	const userID int64 = 91345
	const groupID int64 = -10091345
	key := fmt.Sprintf("%d:%d", groupID, userID)
	groupMemberCache.Delete(key)
	t.Cleanup(func() { groupMemberCache.Delete(key) })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottest/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"test_bot"}}`))
		case "/bottest/getChatMember":
			calls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"user":{"id":91345,"is_bot":false,"first_name":"test"},"status":"left"}}`))
		default:
			http.Error(w, "unexpected API call", http.StatusNotFound)
		}
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	// This represents a positive regular lookup made 31 seconds ago. Its
	// five-minute cache entry is still valid, but a fresh check must query Telegram.
	groupMemberCache.Store(key, &groupMemberCacheEntry{
		inGroup:   true,
		checkedAt: time.Now().Add(-31 * time.Second),
		expireAt:  time.Now().Add(groupMemberPositiveTTL - 31*time.Second),
	})
	if IsUserInGroupFresh(bot, userID, groupID) {
		t.Fatal("fresh group check accepted a user who has left")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fresh group check made %d API calls, want 1", got)
	}
	if IsUserInGroupFresh(bot, userID, groupID) {
		t.Fatal("cached fresh group check accepted a user who has left")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("live membership was not checked again: %d API calls, want 2", got)
	}
}

func TestIsUserInGroupFreshRejectsRecentLeaver(t *testing.T) {
	const userID int64 = 91346
	const groupID int64 = -10091346
	key := fmt.Sprintf("%d:%d", groupID, userID)
	groupMemberCache.Delete(key)
	t.Cleanup(func() { groupMemberCache.Delete(key) })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bottest/getMe" {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"test_bot"}}`))
			return
		}
		if r.URL.Path != "/bottest/getChatMember" {
			http.Error(w, "unexpected API call", http.StatusNotFound)
			return
		}
		status := "member"
		if calls.Add(1) > 1 {
			status = "left"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"user":{"id":91346,"is_bot":false,"first_name":"test"},"status":%q}}`, status)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if !IsUserInGroupFresh(bot, userID, groupID) || IsUserInGroupFresh(bot, userID, groupID) {
		t.Fatal("recently cached member retained access after leaving")
	}
}

package activity

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPrivatePacketClaimsRejectedBeforeWalletAccess(t *testing.T) {
	for _, command := range []string{"抢", "沾仙气"} {
		t.Run(command, func(t *testing.T) {
			replies := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
					return
				}
				r.ParseForm()
				if !strings.Contains(r.Form.Get("text"), "请到群里") {
					t.Error("missing group-only explanation")
				}
				replies++
				fmt.Fprint(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":1,"type":"private"}}}`)
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			HandleGrabRedPacket(bot, &tgbotapi.Message{Text: command, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}, From: &tgbotapi.User{ID: 1}})
			if replies != 1 {
				t.Fatalf("expected rejection, got %d replies", replies)
			}
		})
	}
}

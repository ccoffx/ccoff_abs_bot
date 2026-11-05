package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
)

type messageObserverTransport func(*http.Request) (*http.Response, error)

func (f messageObserverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTelegramHTTPClientObservesSendsWithoutChangingSDKResponse(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = previous })
	client := newTelegramHTTPClient()
	var observed []int
	client.observeMessage = func(msg *tgbotapi.Message) error {
		observed = append(observed, msg.MessageID)
		return errors.New("tracking unavailable") // Do not turn a successful send into a duplicate retry.
	}
	responses := map[string]string{
		"getMe":           `{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"test"}}`,
		"sendMessage":     `{"ok":true,"result":{"message_id":10,"chat":{"id":-100},"text":"hello"}}`,
		"sendPhoto":       `{"ok":true,"result":{"message_id":11,"chat":{"id":-100}}}`,
		"sendMediaGroup":  `{"ok":true,"result":[{"message_id":12,"chat":{"id":-100}},{"message_id":13,"chat":{"id":-100}}]}`,
		"editMessageText": `{"ok":true,"result":{"message_id":1,"chat":{"id":-100}}}`,
		"sendChatAction":  `{"ok":true,"result":true}`,
		"sendVideo":       `{"ok":false,"error_code":400,"description":"rejected"}`,
	}
	transport := messageObserverTransport(func(req *http.Request) (*http.Response, error) {
		body, ok := responses[telegramEndpointFromRequest(req)]
		if !ok {
			t.Fatalf("unexpected request: %s", telegramEndpointFromRequest(req))
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	client.send.Transport, client.upload.Transport = transport, transport
	bot, err := tgbotapi.NewBotAPIWithClient("test", "https://telegram.invalid/bot%s/%s", client)
	if err != nil {
		t.Fatal(err)
	}
	// Use the normal SDK streaming decoder: it may stop before reading EOF.
	for _, method := range []string{"sendMessage", "sendPhoto", "sendMediaGroup", "editMessageText", "sendChatAction", "sendVideo"} {
		response, err := bot.MakeRequest(method, tgbotapi.Params{"chat_id": "-100"})
		if method == "sendVideo" {
			if err == nil {
				t.Fatal("observer hid the Telegram error")
			}
			continue
		}
		var expected tgbotapi.APIResponse
		if json.Unmarshal([]byte(responses[method]), &expected) != nil || err != nil || string(response.Result) != string(expected.Result) {
			t.Fatalf("observer changed the SDK response for %s: %+v err=%v", method, response, err)
		}
	}
	if !reflect.DeepEqual(observed, []int{10, 11, 12, 13}) {
		t.Fatalf("sends were lost, or edits/rejected sends were observed: %v", observed)
	}
}

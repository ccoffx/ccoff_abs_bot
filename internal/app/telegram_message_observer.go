package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/textfmt"
)

// Observe successful sends at the HTTP boundary, including sends outside the
// common Telegram helpers. Preserve the original SDK response and read errors.
type telegramMessageObservingBody struct {
	io.ReadCloser
	data       bytes.Buffer
	readFailed bool
	observe    func(*tgbotapi.Message) error
}

func (b *telegramMessageObservingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.data.Write(p[:n])
	if err != nil && err != io.EOF {
		b.readFailed = true
	}
	return n, err
}

func (b *telegramMessageObservingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.readFailed {
		return err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(b.data.Bytes(), &response) != nil || !response.OK {
		return err
	}
	var messages []tgbotapi.Message
	var message tgbotapi.Message
	if json.Unmarshal(response.Result, &message) == nil {
		messages = append(messages, message)
	} else if json.Unmarshal(response.Result, &messages) != nil {
		return err
	}
	for i := range messages {
		if observeErr := b.observe(&messages[i]); observeErr != nil {
			// A tracking failure must not turn a successful send into a retry and
			// accidentally duplicate a message whose ID Telegram already returned.
			log.Printf("入群验证 Bot 消息位置记录失败: %s", textfmt.FormatPlainError(observeErr))
		}
	}
	return err
}

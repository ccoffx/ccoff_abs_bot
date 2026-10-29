package telegram

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func entityValue(text string, e tgbotapi.MessageEntity) string {
	units := utf16.Encode([]rune(text))
	return string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
}

func TestCopyablePreservesExactTextAndUTF16(t *testing.T) {
	values := []string{"秋日好运🎉", "卡密 a`b\\c_*[]", "积分 100 3\n续期 30 2"}
	var input strings.Builder
	for _, v := range values {
		input.WriteString("🎲 发送：" + textfmt.Copyable(v) + "\n")
	}
	text, entities := formatCopyable(input.String(), "")
	if len(entities) != len(values) {
		t.Fatalf("entities: %+v", entities)
	}
	for i, e := range entities {
		if got := entityValue(text, e); got != values[i] {
			t.Fatalf("copied %q, want %q", got, values[i])
		}
	}
	if entities[2].Type != "pre" || strings.ContainsAny(text, "\x02\x03") {
		t.Fatal("configuration must be a clean pre block")
	}
}

func TestCopyableTransportPreservesMessageOptions(t *testing.T) {
	msg := tgbotapi.NewMessage(-42, "发送："+textfmt.Copyable("参加抽奖 1"))
	msg.ReplyToMessageID = 12
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("返回", "home")))
	got := PrepareCopyable(msg).(tgbotapi.MessageConfig)
	if got.ReplyToMessageID != 12 || !reflect.DeepEqual(msg.ReplyMarkup, got.ReplyMarkup) || len(got.Entities) != 1 {
		t.Fatalf("message options lost: %+v", got)
	}
	edit := tgbotapi.NewEditMessageText(-42, 9, msg.Text)
	edited := PrepareCopyable(edit).(tgbotapi.EditMessageTextConfig)
	if edited.MessageID != 9 || edited.Text != got.Text || !reflect.DeepEqual(edited.Entities, got.Entities) {
		t.Fatalf("edit differs: %+v", edited)
	}
	again := PrepareCopyable(got).(tgbotapi.MessageConfig)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("formatting is not idempotent")
	}
}

func TestCopyableRetainsMarkdownAndLegacyPlainCode(t *testing.T) {
	input := "**提示**：" + textfmt.Copyable("确认创建抽奖") + "，或 `取消`"
	got, entities := formatCopyable(input, "Markdown")
	if got != "**提示**：`确认创建抽奖`，或 `取消`" || len(entities) != 0 {
		t.Fatalf("Markdown: %q %+v", got, entities)
	}
	got, entities = formatCopyable("发送 `1` 或 `2`。", "")
	if got != "发送 1 或 2。" || len(entities) != 2 {
		t.Fatalf("plain code: %q %+v", got, entities)
	}
	got, entities = formatCopyable("说明含单个 ` 字符", "")
	if got != "说明含单个 ` 字符" || len(entities) != 0 {
		t.Fatalf("unmatched backtick changed: %q", got)
	}
}

func TestPlainTextDoesNotInterpretLiteralSecrets(t *testing.T) {
	value := "卡密：a`b\\c`d"
	got, entities := formatCopyable(value, "")
	if got != value || len(entities) != 0 {
		t.Fatalf("literal value changed: %q", got)
	}
}

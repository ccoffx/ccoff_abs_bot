package telegram

import (
	"ccoff-abs-bot/internal/textfmt"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSplitMessageTextPreservesUnicodeWithinTelegramLimit(t *testing.T) {
	for _, text := range []string{"普通短消息", strings.Repeat("汉", 9000), strings.Repeat("😀汉字", 4000)} {
		parts := SplitMessageText(text)
		if strings.Join(parts, "") != text {
			t.Fatal("message splitting lost or changed text")
		}
		for _, part := range parts {
			if units := len(utf16.Encode([]rune(part))); units > 3800 || units == 0 {
				t.Fatalf("invalid message length: %d", units)
			}
		}
	}
}

func TestSplitMessageTextKeepsCopyableCodesIntact(t *testing.T) {
	codes := make([]string, 50)
	for i := range codes {
		codes[i] = strings.Repeat("😀卡密`", 40)
	}
	text := "购买成功\n卡密：\n" + textfmt.Copyable(strings.Join(codes, "\n")) + "\n请妥善保存。"
	want, _ := formatCopyable(text, "")
	var actual strings.Builder
	var copied []string
	for _, part := range SplitMessageText(text) {
		plain, entities := formatCopyable(part, "")
		if utf16Length(plain) > 3800 || strings.ContainsAny(plain, "\x02\x03") {
			t.Fatal("invalid transport message")
		}
		actual.WriteString(plain)
		units := utf16.Encode([]rune(plain))
		for _, entity := range entities {
			value := string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
			for _, line := range strings.Split(value, "\n") {
				if line != "" {
					copied = append(copied, line)
				}
			}
		}
	}
	if actual.String() != want || strings.Join(copied, "\n") != strings.Join(codes, "\n") {
		t.Fatal("split messages lost text or fragmented copyable card codes")
	}
}

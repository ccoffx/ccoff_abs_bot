package textfmt

import (
	"strings"
)

func EscapeMarkdown(text string) string {
	text = strings.ReplaceAll(text, "_", "\\_")
	text = strings.ReplaceAll(text, "*", "\\*")
	text = strings.ReplaceAll(text, "[", "\\[")
	text = strings.ReplaceAll(text, "`", "\\`")
	return text
}

func EscapeMarkdownPreservingEscapes(text string) string {
	var b strings.Builder
	prevBackslash := false
	for _, r := range text {
		if (r == '_' || r == '*' || r == '[' || r == '`') && !prevBackslash {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
		prevBackslash = r == '\\'
	}
	return b.String()
}

func TelegramUsernameMentionMarkdown(username string) string {
	username = strings.TrimSpace(username)
	username = strings.TrimPrefix(username, "@")
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	return "@" + EscapeMarkdown(username)
}

func InventoryItemMarkdownName(name string) string {
	name = strings.Map(func(r rune) rune {
		if ContainsDisallowedControl(string(r), false) {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "-"
	}
	return EscapeMarkdown(name)
}

func TruncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "..."
}

package telegram

import (
	"html"
	"strings"
	"unicode/utf16"

	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

// formatCopyable consumes explicit presentation markers, or existing code
// notation in plain messages. It never interprets user values inside markers.
func formatCopyable(text, mode string) (string, []tgbotapi.MessageEntity) {
	var out strings.Builder
	var entities []tgbotapi.MessageEntity
	offset := 0
	write := func(s string) { out.WriteString(s); offset += utf16Length(s) }
	for len(text) > 0 {
		start := strings.IndexByte(text, '\x02')
		delimiter := "\x03"
		opening := 1
		// Existing plain-text prompts already use Markdown code notation in places.
		if mode == "" {
			if i := strings.IndexByte(text, '`'); i >= 0 && (start < 0 || i < start) {
				lineStart := strings.LastIndexByte(text[:i], '\n') + 1
				prefix := text[lineStart:i]
				if lineStart == 0 {
					previous := out.String()
					prefix = previous[strings.LastIndexByte(previous, '\n')+1:] + prefix
				}
				hint := false
				for _, cue := range []string{"发送", "回复", "输入", "用法", "示例", "例如"} {
					if strings.Contains(prefix, cue) {
						hint = true
						break
					}
				}
				if !hint {
					write(text[:i+1])
					text = text[i+1:]
					continue
				}
				start = i
				delimiter = "`"
				opening = 1
				if strings.HasPrefix(text[i:], "```") {
					delimiter = "```"
					opening = 3
				}
				rest := text[i+opening:]
				end := strings.Index(rest, delimiter)
				marker := strings.IndexByte(rest, '\x02')
				if end < 0 || (marker >= 0 && marker < end) {
					write(text[:i+opening])
					text = text[i+opening:]
					continue
				}
			}
		}
		if start < 0 {
			write(text)
			break
		}
		write(text[:start])
		text = text[start+opening:]
		end := strings.Index(text, delimiter)
		if end < 0 {
			write(text)
			break
		}
		value := text[:end]
		text = text[end+len(delimiter):]
		switch mode {
		case "Markdown":
			if strings.Contains(value, "\n") && !strings.Contains(value, "`") {
				write("```\n" + value + "\n```")
			} else {
				write(textfmt.MarkdownCode(value))
			}
		case "MarkdownV2":
			escaped := strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(value)
			if strings.Contains(value, "\n") {
				write("```\n" + escaped + "\n```")
			} else {
				write("`" + escaped + "`")
			}
		case "HTML":
			if strings.Contains(value, "\n") {
				write("<pre>" + html.EscapeString(value) + "</pre>")
			} else {
				write("<code>" + html.EscapeString(value) + "</code>")
			}
		default:
			if value != "" {
				kind := "code"
				if strings.Contains(value, "\n") {
					kind = "pre"
				}
				entities = append(entities, tgbotapi.MessageEntity{Type: kind, Offset: offset, Length: utf16Length(value)})
				write(value)
			}
		}
	}
	return out.String(), entities
}

// PrepareCopyable is shared by direct edits, persistent announcements, queued
// sends and auto-deleted replies. Explicit caller entities are already formatted.
func PrepareCopyable(chattable tgbotapi.Chattable) tgbotapi.Chattable {
	switch msg := chattable.(type) {
	case tgbotapi.MessageConfig:
		if len(msg.Entities) == 0 {
			msg.Text, msg.Entities = formatCopyable(msg.Text, msg.ParseMode)
		}
		return msg
	case tgbotapi.EditMessageTextConfig:
		if len(msg.Entities) == 0 {
			msg.Text, msg.Entities = formatCopyable(msg.Text, msg.ParseMode)
		}
		return msg
	case *tgbotapi.MessageConfig:
		copy := *msg
		return PrepareCopyable(copy)
	case *tgbotapi.EditMessageTextConfig:
		copy := *msg
		return PrepareCopyable(copy)
	default:
		return chattable
	}
}

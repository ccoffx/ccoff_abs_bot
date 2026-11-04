package textfmt

import "strings"

// Copyable marks an exact value for the Telegram transport. Markers never reach
// Telegram; plain messages use entities, Markdown messages use code spans.
// Keep this at the presentation boundary, after sanitizing display text.
func Copyable(value string) string {
	return "\x02" + strings.ReplaceAll(strings.ReplaceAll(value, "\x02", ""), "\x03", "") + "\x03"
}

// MarkdownCode escapes by closing the legacy Markdown entity before a literal
// backtick (legacy Markdown does not support escaping inside entities).
func MarkdownCode(value string) string {
	parts := strings.Split(value, "`")
	for i, part := range parts {
		if part != "" {
			parts[i] = "`" + part + "`"
		}
	}
	return strings.Join(parts, "\\`")
}

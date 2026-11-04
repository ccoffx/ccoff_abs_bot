package activity

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// Public announcements keep one tracked message and its action button. Full
// prize lists are sent in private previews; winners can query their own prize.
func lotterySummaryText(header string, lines []string, footer string) string {
	var b strings.Builder
	b.WriteString(header)
	for i, line := range lines {
		omitted := fmt.Sprintf("…其余 %d 项未在公告中展示。\n", len(lines)-i)
		if len(utf16.Encode([]rune(b.String()+line+omitted+footer))) > 3800 {
			b.WriteString(omitted)
			break
		}
		b.WriteString(line)
	}
	b.WriteString(footer)
	return b.String()
}

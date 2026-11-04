package activity

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestRedPacketFullRankingFitsTelegramMessages(t *testing.T) {
	var full strings.Builder
	full.WriteString("🎉 最后一份红包已领完\n\n📊 气运榜：\n")
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&full, "%d. @%s：`%d` 积分\n", i, strings.Repeat("😀", 32), i)
	}
	full.WriteString("\n👑 手气最佳\n")
	parts := splitRedPacketMarkdownLines(full.String())
	if len(parts) < 2 || strings.Join(parts, "") != full.String() {
		t.Fatalf("full ranking was lost or not split: %d parts", len(parts))
	}
	for i, part := range parts {
		if units := len(utf16.Encode([]rune(part))); units == 0 || units > 3800 {
			t.Fatalf("part %d has invalid UTF-16 length %d", i, units)
		}
		if strings.Count(part, "`")%2 != 0 {
			t.Fatalf("part %d broke a ranking entry's Markdown", i)
		}
	}
}

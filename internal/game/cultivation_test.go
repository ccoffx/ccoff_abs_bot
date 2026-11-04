package game

import (
	"testing"

	"ccoff-abs-bot/internal/model"
)

func TestDouQiRealmDefaultsAndFallbackDisplay(t *testing.T) {
	rules := buildDefaultCultivationRuleSet("test")
	if err := ValidateCultivationRuleSet(rules); err != nil {
		t.Fatal(err)
	}
	if len(rules.Realms) != 14 || len(rules.MinorRealms) != 13 || len(rules.Breakthroughs) != 13 {
		t.Fatal("realm or breakthrough counts changed")
	}
	cultivationRuleCache.mu.Lock()
	previous := cultivationRuleCache.rules
	cultivationRuleCache.rules = &CultivationRuleSet{}
	cultivationRuleCache.mu.Unlock()
	t.Cleanup(func() {
		cultivationRuleCache.mu.Lock()
		cultivationRuleCache.rules = previous
		cultivationRuleCache.mu.Unlock()
	})
	wantNames := []string{"斗之气", "斗者", "斗师", "大斗师", "斗灵", "斗王", "斗皇", "斗宗", "斗尊", "九转斗尊", "半圣", "斗圣", "半帝", "斗帝"}
	for major, name := range wantNames {
		want := "【" + name + "圆满】"
		if major == 0 {
			want = "【斗之气】"
		}
		if got := GetRealmName(&model.Cultivation{MajorRealm: major, MinorRealm: 4}); got != want {
			t.Fatalf("realm %d fallback: got %q, want %q", major, got, want)
		}
		if major > 0 && len(rules.MinorRealms[major]) != 4 {
			t.Fatalf("realm %d lost minor stages", major)
		}
	}
	if got := GetRealmName(&model.Cultivation{MajorRealm: -1}); got != "【震古烁今大能】" {
		t.Fatalf("invalid realm: %q", got)
	}
}

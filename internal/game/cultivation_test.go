package game

import (
	"errors"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/realm"
	"ccoff-abs-bot/internal/storage"
)

func TestDouQiRealmDefaultsAndFallbackDisplay(t *testing.T) {
	rules := buildDefaultCultivationRuleSet("test")
	if err := ValidateCultivationRuleSet(rules); err != nil {
		t.Fatal(err)
	}
	if len(rules.Realms) != 13 || len(rules.MinorRealms) != 13 || len(rules.Breakthroughs) != 12 {
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
	wantNames := []string{"斗之气九段", "九星斗者", "九星斗师", "九星大斗师", "九星斗灵", "九星斗王", "九星斗皇", "九星斗宗", "九星斗尊", "斗尊巅峰十转", "巅峰半圣", "九星斗圣", "斗帝"}
	for major, name := range wantNames {
		count := realm.StageCount(major)
		if got := GetRealmName(&model.Cultivation{MajorRealm: major, MinorRealm: count}); got != "【"+name+"】" {
			t.Fatalf("major %d fallback: %s", major, got)
		}
		if len(rules.MinorRealms[major]) != count {
			t.Fatalf("major %d lost stages", major)
		}
	}
	if got := GetRealmName(&model.Cultivation{MajorRealm: -1}); got != "【震古烁今大能】" {
		t.Fatalf("invalid realm: %q", got)
	}
}

func TestCultivationStarsTurnsAndSaintPeakBoundaries(t *testing.T) {
	setupGuardTestDB(t)
	for _, c := range []struct {
		major                int
		hours                float64
		wantMajor, wantMinor int
	}{
		{0, 0, 0, 1}, {0, 8.9, 0, 9}, {0, 10, 1, 1},
		{1, 49.999, 1, 9}, {1, 5000, 1, 9},
		{9, 5500, 9, 1}, {9, 8099.999, 9, 10},
		{10, 8100, 10, 1}, {10, 11499.999, 10, 4},
		{11, 22299.999, 11, 9}, {12, 99999, 12, 1},
	} {
		if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]any{"major_realm": c.major, "minor_realm": 1, "total_audio_time": c.hours, "pill_audio_time": 0}).Error; err != nil {
			t.Fatal(err)
		}
		var cul model.Cultivation
		if err := storage.DB.Where("user_id = ?", 1).First(&cul).Error; err != nil {
			t.Fatal(err)
		}
		SyncCultivationRealm(&cul)
		if cul.MajorRealm != c.wantMajor || cul.MinorRealm != c.wantMinor {
			t.Fatalf("%+v got %d/%d", c, cul.MajorRealm, cul.MinorRealm)
		}
	}
	if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]any{"major_realm": 5, "minor_realm": 8, "total_audio_time": 1500}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", 0, time.Now(), func() (int, error) { return 1, nil })
	if !errors.Is(err, storage.ErrBreakthroughNotReady) {
		t.Fatal("eight stars could break through", err)
	}
	if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Update("minor_realm", 9).Error; err != nil {
		t.Fatal(err)
	}
	result, err := resolveBreakthroughWithRoll(1, "USE_INVENTORY", 0, time.Now(), func() (int, error) { return 1, nil })
	if err != nil || result == nil {
		t.Fatal("nine stars could not break through", err)
	}
}

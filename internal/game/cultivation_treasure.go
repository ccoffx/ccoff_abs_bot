package game

import (
	"strings"
)

// cultivationTreasureByFromRealm 由“当前大境界（突破前）”索引对应至宝名。
// 键 5~12 对应“斗王→斗皇 … 半帝→斗帝”；斗帝(13)封顶无下一境，故无至宝。
var cultivationTreasureByFromRealm = map[int]string{
	5:  "地心熔岩髓", // 斗王 → 斗皇
	6:  "破宗源晶",  // 斗皇 → 斗宗
	7:  "空间结晶",  // 斗宗 → 斗尊
	8:  "九转金髓",  // 斗尊 → 九转斗尊
	9:  "古龙真血",  // 九转斗尊 → 半圣
	10: "菩提心",   // 半圣 → 斗圣
	11: "帝境残魂",  // 斗圣 → 半帝
	12: "古帝源气",  // 半帝 → 斗帝
}

// cultivationTreasureDropRatePercent 世界 Boss 至宝掉落概率（%）。
func cultivationTreasureDropRatePercent() int { return 10 }

// cultivationTreasureNameForRealm 返回“从该大境界突破到下一境”所需的至宝名；无则返回空串。
func CultivationTreasureNameForRealm(major int) string {
	return cultivationTreasureByFromRealm[major]
}

// isCultivationTreasureItemName 判断某 Inventory 物品名是否为至宝。
func IsCultivationTreasureItemName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, v := range cultivationTreasureByFromRealm {
		if v == name {
			return true
		}
	}
	return false
}

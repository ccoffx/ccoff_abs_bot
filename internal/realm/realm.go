// Package realm defines the shared Dou Qi stages used by cultivation and titles.
package realm

import (
	"fmt"
	"strings"

	"ccoff-abs-bot/internal/model"
)

const Emperor = 12

var numerals = []string{"", "一", "二", "三", "四", "五", "六", "七", "八", "九", "十"}

func StageCount(major int) int {
	switch {
	case major == 9:
		return 10
	case major == 10:
		return 4
	case major == Emperor:
		return 1
	case major >= 0 && major < Emperor:
		return 9
	default:
		return 0
	}
}

func MinorName(major, minor int) string {
	count := StageCount(major)
	if count == 0 {
		return ""
	}
	if minor < 1 {
		minor = 1
	}
	if minor > count {
		minor = count
	}
	switch major {
	case 0:
		return numerals[minor] + "段"
	case 9:
		return numerals[minor] + "转"
	case 10:
		return []string{"低阶", "中阶", "高阶", "巅峰"}[minor-1]
	case Emperor:
		return ""
	default:
		return numerals[minor] + "星"
	}
}

func Format(major, minor int, majorName, minorName string) string {
	if StageCount(major) == 0 {
		return "震古烁今大能"
	}
	if majorName == "" {
		majorName = Defaults()[major].TitleName
	}
	// 小境界编号已采用段/星/转/阶语义，旧配置中的阶段文案不能
	// 覆盖当前称呼。称号页直读数据库与斗气页读取缓存均走此处。
	switch strings.TrimSpace(minorName) {
	case "", "前期", "初期", "中期", "后期", "圆满", "大圆满":
		minorName = MinorName(major, minor)
	}
	switch major {
	case 0, 9:
		return majorName + minorName
	case Emperor:
		return majorName
	default:
		return minorName + majorName
	}
}

func Defaults() []model.CultivationRealmConfig {
	names := []string{"斗之气", "斗者", "斗师", "大斗师", "斗灵", "斗王", "斗皇", "斗宗", "斗尊", "斗尊巅峰", "半圣", "斗圣", "斗帝"}
	// 保留现有各大境界入境门槛；取消半帝后，其区间并入斗圣。
	starts := []float64{0, 10, 50, 150, 350, 750, 1500, 2400, 3700, 5500, 8100, 11500, 22300}
	out := make([]model.CultivationRealmConfig, 0, len(names))
	for major, name := range names {
		max := 0.0
		if major+1 < len(names) {
			max = starts[major+1]
		}
		out = append(out, model.CultivationRealmConfig{MajorRealm: major, Name: name, TitleName: name, MinTotalHours: starts[major], MaxTotalHours: max, IsMaxRealm: major == Emperor, Enabled: true})
	}
	return out
}

func DefaultMinors() []model.CultivationMinorRealmConfig {
	var out []model.CultivationMinorRealmConfig
	for _, r := range Defaults() {
		count := StageCount(r.MajorRealm)
		for minor := 1; minor <= count; minor++ {
			hours := r.MinTotalHours
			if !r.IsMaxRealm {
				hours += (r.MaxTotalHours - r.MinTotalHours) * float64(minor-1) / float64(count)
			}
			name := MinorName(r.MajorRealm, minor)
			if name == "" {
				name = "至高境"
			}
			out = append(out, model.CultivationMinorRealmConfig{MajorRealm: r.MajorRealm, MinorRealm: minor, Name: name, RequiredHours: hours, Enabled: true})
		}
	}
	return out
}

func ValidateStage(major, minor int) error {
	count := StageCount(major)
	if count == 0 {
		return fmt.Errorf("大境界编号只能是 0-%d", Emperor)
	}
	if minor < 1 || minor > count {
		return fmt.Errorf("该境界的小境界编号只能是 1-%d", count)
	}
	return nil
}

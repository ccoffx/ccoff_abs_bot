package realm

import "testing"

func TestFormatPreservesCurrentConfiguredNames(t *testing.T) {
	for _, tc := range []struct {
		major, minor               int
		majorName, minorName, want string
	}{
		{2, 3, "斗师", "三星", "三星斗师"},
		{9, 10, "斗尊巅峰", "十转", "斗尊巅峰十转"},
		{10, 4, "半圣", "巅峰", "巅峰半圣"},
		{2, 3, "自定义大境界", "自定义阶段", "自定义阶段自定义大境界"},
	} {
		if got := Format(tc.major, tc.minor, tc.majorName, tc.minorName); got != tc.want {
			t.Fatalf("Format(%d, %d, %q, %q) = %q, want %q", tc.major, tc.minor, tc.majorName, tc.minorName, got, tc.want)
		}
	}
}

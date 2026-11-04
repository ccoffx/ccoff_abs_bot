package game

import (
	"testing"
	"time"
)

func TestWorldBossStartWindowAllowsLateRecovery(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before opening", time.Date(2026, 10, 31, 20, 59, 0, 0, loc), false},
		{"on time", time.Date(2026, 10, 31, 21, 0, 0, 0, loc), true},
		{"restart after opening", time.Date(2026, 10, 31, 21, 17, 0, 0, loc), true},
		{"last joinable moment", time.Date(2026, 10, 31, 21, 44, 59, 0, loc), true},
		{"join closed", time.Date(2026, 10, 31, 21, 45, 0, 0, loc), false},
		{"after closing", time.Date(2026, 10, 31, 22, 0, 0, 0, loc), false},
		{"weekday", time.Date(2026, 11, 2, 21, 17, 0, 0, loc), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := worldBossShouldStartAt(tc.at); got != tc.want {
				t.Fatalf("should start at %s = %t, want %t", tc.at, got, tc.want)
			}
		})
	}
}

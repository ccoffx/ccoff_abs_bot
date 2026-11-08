package app

import (
	"strings"
	"testing"

	"ccoff-abs-bot/internal/activity"
	"ccoff-abs-bot/internal/textfmt"
)

func TestTribulationMenuEntriesAndReservedCommands(t *testing.T) {
	_, profile := renderFeatureMenu("profile", 1)
	found := false
	for _, row := range profile.InlineKeyboard {
		for _, button := range row {
			if button.CallbackData != nil && *button.CallbackData == "menu:cmd:titles" {
				command, ok := menuCommandText("titles")
				if !ok || command != "我的称号" {
					t.Fatal("profile title button does not dispatch title command")
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("profile has no title entry")
	}
	_, activities := renderFeatureMenu("activity", 1)
	found = false
	for _, row := range activities.InlineKeyboard {
		for _, button := range row {
			if button.CallbackData != nil && *button.CallbackData == "menu:tip:tribulation" && strings.Contains(button.Text, "天雷猜阵") && strings.Contains(button.Text, "5积分") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("activities have no tribulation entry or wrong price")
	}
	for _, command := range []string{"猜阵", "天雷猜阵", "发起猜阵", "猜阵状态", "我的称号", "称号榜"} {
		if !textfmt.IsReservedBotInput(command) {
			t.Fatalf("group keyword replies may shadow %s", command)
		}
	}
	for _, command := range []string{"猜阵", "天雷猜阵", "发起猜阵", "猜阵状态"} {
		if !activity.HandleTribulationCommand(nil, nil, command) {
			t.Fatalf("game command not recognized: %s", command)
		}
	}
	for _, command := range []string{"发起渡劫", "灵气渡劫", "渡劫", "渡劫状态", "突破", "确认渡劫", "阴阳避雷", "发起避雷", "避雷", "避雷状态"} {
		if activity.HandleTribulationCommand(nil, nil, command) {
			t.Fatalf("game still handles cultivation-like command: %s", command)
		}
	}
}

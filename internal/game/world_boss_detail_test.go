package game

import (
	"strings"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestWorldBossDetailShowsParticipantOutsideTopTen(t *testing.T) {
	for _, status := range []string{"active", "settled"} {
		t.Run(status, func(t *testing.T) {
			event := worldBossLiveFixture(t, 12)
			contributionMust(t, storage.DB.Model(&event).Update("status", status).Error)
			contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).
				Updates(map[string]any{"damage": 30, "delta_hours": .5}).Error)
			contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", event.BossID, 12).
				Updates(map[string]any{"user_name": "榜外道友", "damage": 9, "delta_hours": .25,
					"base_damage": 7.5, "multiplier": 1.2, "reward_points": 5, "reward_contribution": 2,
					"reward_prestige": 1, "is_rewarded": status == "settled"}).Error)
			top, err := worldBossTopParticipants(event.BossID, 10)
			contributionMust(t, err)
			for _, p := range top {
				if p.UserID == 12 {
					t.Fatal("fixture player must be outside the public top ten")
				}
			}
			bot, replies := secretRealmQueryBot(t)
			msg := &tgbotapi.Message{MessageID: 42, From: &tgbotapi.User{ID: 12}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}}
			if !HandleWorldBossCommand(bot, msg, "Boss明细") {
				t.Fatal("detail command was not handled")
			}
			reply := replies()
			for _, want := range []string{"世界Boss明细", "道友：`榜外道友`", "有效听书：`15.00` 分钟", "伤害：`9.00`", "基础伤害：`7.50`", "伤害倍率：`1.20`"} {
				if !strings.Contains(reply, want) {
					t.Fatalf("missing %q in personal detail: %s", want, reply)
				}
			}
			if status == "settled" && (!strings.Contains(reply, "最终结算有效听书") || !strings.Contains(reply, "结算奖励：积分 +`5`，贡献 +`2`，声望 +`1`")) {
				t.Fatalf("missing final personal result: %s", reply)
			}
		})
	}
}

func TestWorldBossDetailMissingRecords(t *testing.T) {
	for _, missing := range []string{"participant", "event"} {
		t.Run(missing, func(t *testing.T) {
			event := worldBossLiveFixture(t, 1)
			want := "未找到你在本期世界Boss中的参与记录"
			if missing == "event" {
				contributionMust(t, storage.DB.Delete(&event).Error)
				want = "暂无世界Boss记录"
			}
			bot, replies := secretRealmQueryBot(t)
			HandleWorldBossCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 99}, Chat: &tgbotapi.Chat{ID: 99, Type: "private"}}, "Boss明细")
			if reply := replies(); !strings.Contains(reply, want) || strings.Contains(reply, "伤害：") {
				t.Fatalf("missing record shown as a personal result: %s", reply)
			}
		})
	}
}

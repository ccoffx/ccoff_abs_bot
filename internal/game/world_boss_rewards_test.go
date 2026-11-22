package game

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
)

func TestWorldBossDamagePoolAllocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage []float64
		pool   int
		want   []int
	}{
		{"proportional", []float64{30, 20, 10, 2}, 18, []int{9, 6, 3, 0}},
		{"remainders", []float64{3, 3, 3}, 2, []int{1, 1, 0}},
		{"no eligible", []float64{2, 0}, 12, []int{0, 0}},
		{"single", []float64{3}, 13, []int{13}},
		{"decimal remainder tie", []float64{3.05, 9.15}, 6, []int{2, 4}},
		{"decimal points tie", []float64{3.1, 9.3}, 14, []int{4, 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := make([]model.WorldBossParticipant, len(tc.damage))
			for i, d := range tc.damage {
				p[i] = model.WorldBossParticipant{UserID: int64(i + 1), Damage: d}
			}
			if got := allocateWorldBossDamagePool(p, tc.pool); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// Reordering the leaderboard must not change which user wins a rounding tie.
	p := []model.WorldBossParticipant{{UserID: 3, Damage: 3}, {UserID: 1, Damage: 3}, {UserID: 2, Damage: 3}}
	if got := allocateWorldBossDamagePool(p, 2); !reflect.DeepEqual(got, []int{0, 1, 1}) {
		t.Fatal(got)
	}
}

func TestWorldBossRedPacketScalesWithEffectiveParticipants(t *testing.T) {
	for _, n := range []int{1, 2, 10, 11, 101} {
		p := newWorldBossRedPacket("test", model.WorldBossEvent{}, n, time.Now())
		if p.Count != n || p.LeftCount != n || p.TotalPoints != n*3 || p.LeftPoints != n*3 || p.ClaimRuleVersion != storage.RedPacketWorldBossEffectiveClaimVersion {
			t.Fatalf("n=%d packet=%+v", n, p)
		}
	}
}

func TestWorldBossProportionalSettlement(t *testing.T) {
	event := worldBossLiveFixture(t, 3)
	event.MaxHP, event.Status = 100, "settling"
	contributionMust(t, storage.DB.Save(&event).Error)
	contributionMust(t, storage.DB.AutoMigrate(&model.PointTransaction{}, &model.SectMember{}, &model.Sect{}, &model.SectContributionLog{}, &model.RedPacket{}, &model.SpiritFireEvent{}, &model.SpiritFireRules{}, &model.SpiritFireState{}, &model.SpiritFire{}, &model.SpiritFireWallet{}, &model.SpiritFireNotice{}))
	contributionMust(t, spiritfire.PrepareEvent(storage.DB, "boss:"+event.BossID, event.StartAt))
	var participants []model.WorldBossParticipant
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Order("user_id").Find(&participants).Error)
	for i := range participants {
		participants[i].Damage = []float64{90, 30, 2}[i]
		contributionMust(t, storage.DB.Save(&participants[i]).Error)
	}
	_, err := grantWorldBossRewards(&event, participants)
	contributionMust(t, err)
	contributionMust(t, storage.DB.Where("boss_id = ?", event.BossID).Order("user_id").Find(&participants).Error)
	for i, want := range []struct {
		points, contribution, prestige int
		rewarded                       bool
	}{{11, 37, 5, true}, {3, 12, 1, true}, {0, 0, 0, false}} {
		p := participants[i]
		var user model.User
		contributionMust(t, storage.DB.First(&user, "telegram_id = ?", p.UserID).Error)
		if user.Points != want.points {
			t.Fatalf("user %d balance=%d want=%d", p.UserID, user.Points, want.points)
		}
		if p.RewardPoints != want.points || p.RewardContribution != want.contribution || p.RewardPrestige != want.prestige || p.IsRewarded != want.rewarded {
			t.Fatalf("participant %d: %+v", i, p)
		}
	}
	var packet model.RedPacket
	contributionMust(t, storage.DB.First(&packet, "ref_id = ?", event.BossID).Error)
	if packet.Count != 2 || packet.TotalPoints != 6 || packet.ClaimRuleVersion != storage.RedPacketWorldBossEffectiveClaimVersion {
		t.Fatalf("packet: %+v", packet)
	}
}

func TestWorldBossKilledWithoutEffectiveParticipantsNotice(t *testing.T) {
	event := worldBossLiveFixture(t, 151)
	event.ChatID, event.Status, event.IsKilled, event.CurrentHP = -100, "settled", true, 0
	contributionMust(t, storage.DB.Save(&event).Error)
	contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Update("damage", 2.99).Error)
	totalDamage := roundWorldBossDamage(151 * 2.99)
	if totalDamage < float64(event.MaxHP) {
		t.Fatal("fixture must kill Boss without any effective participant")
	}
	bot, replies := secretRealmQueryBot(t)
	sendWorldBossSettlement(bot, event, true, totalDamage)
	reply := replies()
	for _, want := range []string{"击杀成功", "本期无人达到有效奖励门槛", "不发放个人积分、宗门贡献和声望", "不生成 Boss 红包", "天道奖池 `+10`"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("missing %q in notice: %s", want, reply)
		}
	}
	for _, unwanted := range []string{"积分池", "贡献池", "声望池", "发送 `抢`", "积分 /"} {
		if strings.Contains(reply, unwanted) {
			t.Fatalf("misleading %q in notice: %s", unwanted, reply)
		}
	}
}

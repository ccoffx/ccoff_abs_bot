package game

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
)

func worldBossLiveRefreshDue(event model.WorldBossEvent, now time.Time) bool {
	return event.BossID != "" && event.Status == "active" && !event.IsKilled && event.ListeningCutoffAt == nil &&
		!now.Before(event.StartAt) && now.Before(event.EndAt) &&
		(event.LiveRefreshedAt == nil || !listeningTimestampFresh(*event.LiveRefreshedAt, now, worldBossLiveRefreshInterval))
}

func worldBossLiveRefreshNotice(event model.WorldBossEvent) string {
	if event.Status == "settled" {
		return ""
	}
	if event.Status == "settling" || event.IsKilled || event.ListeningCutoffAt != nil || !time.Now().Before(event.EndAt) {
		return "\n⏳ Boss 结算中，当前为已保存战况，伤害和奖励以最终结算为准。"
	}
	notice := "\n约每 2 分钟后台刷新，当前为已保存战况。"
	if event.LiveRefreshedAt == nil {
		notice += "\n⏳ 首次刷新尚未完成，伤害数据待更新。"
	} else {
		label := "最近刷新"
		if event.LiveRefreshIncomplete {
			label = "最近刷新尝试"
		}
		notice += fmt.Sprintf("\n%s：`%s`（北京时间）", label, event.LiveRefreshedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"))
	}
	if event.LiveRefreshIncomplete {
		notice += "\n⚠️ 部分道友同步失败，保留其上次战况，其余战况已更新，后台将重试。"
	} else if worldBossLiveRefreshDue(event, time.Now()) && event.LiveRefreshedAt != nil {
		notice += "\n⏳ 战况待更新，请稍后查看。"
	}
	return notice
}

func refreshWorldBossLiveParticipant(p *model.WorldBossParticipant, event model.WorldBossEvent, refreshAt time.Time) (processed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			processed = false
			err = fmt.Errorf("world boss participant refresh panic: %s", textfmt.FormatPlainValue(r))
		}
	}()
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", p.UserID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if user.Status != "active" || strings.TrimSpace(p.AbsUserID) == "" || user.AbsUserID != p.AbsUserID {
		return false, nil // Clear excluded participants when publishing the complete round.
	}
	hours, _, err := worldBossListeningHours(p, event, user.AbsUserID, refreshAt)
	if err != nil {
		return false, err
	}
	maxHours := calculateWorldBossDamageWindowHours(*p, event, refreshAt)
	_, baseDamage, _ := calculateWorldBossDamage(hours, maxHours)
	damage, multiplier, err := ApplyWorldBossDamageBonusesChecked(p.UserID, baseDamage)
	if err != nil {
		return false, err
	}
	p.FinalHours = p.BaseHours + hours
	p.DeltaHours = hours
	if p.DeltaHours > maxHours {
		p.DeltaHours = maxHours
	}
	p.BaseDamage, p.Multiplier, p.Damage = baseDamage, multiplier, damage
	return true, nil
}

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

func sectSecretRealmLiveRefreshDue(event model.SectSecretRealmEvent, now time.Time) bool {
	return canJoinSectSecretRealmAt(event, now) && (event.LiveRefreshedAt == nil ||
		!listeningTimestampFresh(*event.LiveRefreshedAt, now, sectSecretRealmLiveRefreshInterval))
}

// The caller owns the shared realm barrier and event turn. Recheck freshness so
// overlapping scheduler passes cannot repeat a just-completed refresh.
func refreshSectSecretRealmLiveProgressIfDue(event model.SectSecretRealmEvent) (model.SectSecretRealmEvent, bool, error) {
	var current model.SectSecretRealmEvent
	if err := storage.DB.Where("realm_id = ?", event.RealmID).First(&current).Error; err != nil {
		return event, false, err
	}
	if !sectSecretRealmLiveRefreshDue(current, time.Now()) {
		return current, false, nil
	}
	refreshed, err := refreshSectSecretRealmLiveProgress(current)
	return refreshed, true, err
}

func tryRefreshSectSecretRealmLiveProgress(event model.SectSecretRealmEvent) (model.SectSecretRealmEvent, bool, error) {
	if !sectSecretRealmLiveRefreshMu.TryRLock() {
		return event, false, nil
	}
	defer sectSecretRealmLiveRefreshMu.RUnlock()
	release, ok := tryActivityEventTurn("realm:" + event.RealmID)
	if !ok {
		return event, false, nil
	}
	defer release()
	return refreshSectSecretRealmLiveProgressIfDue(event)
}

func sectSecretRealmLiveRefreshNotice(event model.SectSecretRealmEvent) string {
	if event.Status == "settled" {
		return ""
	}
	if event.Status == "settling" || !time.Now().Before(event.EndAt) {
		return "\n⏳ 秘境结算中，当前为已保存榜单，奖励以最终结算为准。"
	}
	notice := "\n约每 2 分钟后台刷新，当前为已保存榜单。"
	if event.LiveRefreshedAt == nil {
		notice += "\n⏳ 首次刷新尚未完成，参与者数据待更新。"
	} else {
		label := "最近刷新"
		if event.LiveRefreshIncomplete {
			label = "最近刷新尝试"
		}
		notice += fmt.Sprintf("\n%s：`%s`（北京时间）", label, event.LiveRefreshedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"))
	}
	if event.LiveRefreshIncomplete {
		notice += "\n⚠️ 部分道友同步失败，保留其上次数据，其余榜单已更新，后台将重试。"
	} else if sectSecretRealmLiveRefreshDue(event, time.Now()) && event.LiveRefreshedAt != nil {
		notice += "\n⏳ 榜单待更新，请稍后查看。"
	}
	return notice
}

func refreshSectSecretRealmLiveParticipant(p *model.SectSecretRealmParticipant, event model.SectSecretRealmEvent, refreshAt time.Time) (processed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			processed = false
			err = fmt.Errorf("secret realm participant refresh panic: %s", textfmt.FormatPlainValue(r))
		}
	}()
	var u model.User
	if err := storage.DB.Where("telegram_id = ?", p.UserID).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil // Clear the preview in the publication transaction.
		}
		return false, err
	}
	if strings.TrimSpace(u.AbsUserID) == "" || u.Status != "active" || p.AbsUserID != u.AbsUserID {
		return false, nil
	}
	seconds, _, err := sectSecretRealmListeningSeconds(p, event, u.AbsUserID, refreshAt)
	if err != nil {
		return false, err
	}
	p.MajorRealm, p.MinorRealm = sectSecretRealmCultivationSnapshot(p.UserID)
	p.FinalHours = p.BaseHours
	if total, ok := secretRealmCultivationTotal(p.UserID); ok {
		p.FinalHours = total
	}
	p.FinalRawSeconds = p.BaseRawSeconds + seconds
	detail := CalculateSectSecretRealmRawDeltaDetail(p.BaseRawSeconds, p.FinalRawSeconds, p.CreatedAt, event.StartAt, event.EndAt, refreshAt)
	p.ObservedRawDeltaSeconds = detail.ObservedDeltaSeconds
	p.WallClockCapSeconds = detail.WallClockCapSeconds
	p.RawDeltaSeconds = seconds
	p.RawCapped = detail.WasCapped
	return true, nil
}

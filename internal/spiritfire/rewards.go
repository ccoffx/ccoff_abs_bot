package spiritfire

import (
	"fmt"
	"sort"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func PrepareEvent(tx *gorm.DB, key string, now time.Time, chatID ...int64) error {
	if !Installed(tx) {
		return nil
	}
	if err := Lock(tx); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	var state model.SpiritFireState
	if err := tx.First(&state, 1).Error; err != nil {
		return err
	}
	if Day(now) < state.LaunchDay {
		return nil
	}
	r, err := RulesAt(tx, Day(now))
	if err != nil {
		return err
	}
	event := model.SpiritFireEvent{ID: key, RulesJSON: Encode(r)}
	if len(chatID) > 0 {
		event.ChatID = chatID[0]
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error
}

type AwardResult struct {
	FireID      uint
	OwnerID     int64
	ScrollUsers []int64
}

// Activity callers supply only participants meeting their original effective
// contribution threshold. One lottery per event, all candidates equally likely.
func AwardInTx(tx *gorm.DB, key string, candidates []int64, boss bool, now time.Time) error {
	if !Installed(tx) {
		return nil
	}
	if err := Lock(tx); err != nil {
		return err
	}
	var event model.SpiritFireEvent
	err := tx.First(&event, "id = ?", key).Error

	if err != nil {
		return err
	}
	if event.Settled {
		return nil
	}
	rules, err := ParseRules(event.RulesJSON)
	if err != nil {
		return err
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		ok, err := Eligible(tx, id, now)
		if err != nil {
			return err
		}
		if ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := AwardResult{}
	chance := rules.RealmChance
	if boss {
		chance = rules.BossChance
	}
	if len(ids) > 0 && security.RandomIntRange(1, 100) <= chance {
		var free []model.SpiritFire
		if err := tx.Where("owner_id = 0 AND rank > 1").Order("id").Find(&free).Error; err != nil {
			return err
		}
		if len(free) > 0 {
			f := free[security.RandomIntRange(0, len(free)-1)]
			owner := ids[security.RandomIntRange(0, len(ids)-1)]
			res := tx.Model(&model.SpiritFire{}).Where("id = ? AND owner_id = 0", f.ID).Updates(map[string]any{"owner_id": owner, "acquired_at": now, "grace_at": nil})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrChanged
			}
			result.FireID = f.ID
			result.OwnerID = owner
			if err := MarkHoldings(tx, owner, now); err != nil {
				return err
			}
			if event.ChatID != 0 {
				if err := tx.Create(&model.SpiritFireNotice{UserID: event.ChatID, Text: fmt.Sprintf("🔥 异火出世！全服唯一的【%s】已认主道友 %d。发送“异火榜”查看归属。", f.Name, owner)}).Error; err != nil {
					return err
				}
			}
			if err := tx.Create(&model.SpiritFireNotice{UserID: owner, Text: fmt.Sprintf("🔥 异火出世！你获得了全服唯一的【%s】。私聊发送“我的异火”查看并选择启用。", f.Name)}).Error; err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		if security.RandomIntRange(1, 100) > rules.ScrollChance {
			continue
		}
		w := model.SpiritFireWallet{UserID: id, Scrolls: 1}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"scrolls": gorm.Expr("scrolls + 1")})}).Create(&w).Error; err != nil {
			return err
		}
		result.ScrollUsers = append(result.ScrollUsers, id)
		if err := tx.Create(&model.SpiritFireNotice{UserID: id, Text: "📜 活动机缘：获得焚诀古卷 ×1，已存入焚诀档案。"}).Error; err != nil {
			return err
		}
	}
	if err := tx.Model(&event).Updates(map[string]any{"settled": true, "result_json": Encode(result)}).Error; err != nil {
		return err
	}
	return Audit(tx, 0, "ACTIVITY", result.FireID, key+" "+Encode(result))
}

// AwardActivityInTx excludes activities that began before launch. A missing
// snapshot on a post-launch event is an error, never default reward rules.
func AwardActivityInTx(tx *gorm.DB, key string, ids []int64, boss bool, start, now time.Time) error {
	if !Installed(tx) {
		return nil
	}
	if err := Lock(tx); err != nil {
		return err
	}
	var state model.SpiritFireState
	if err := tx.First(&state, 1).Error; err != nil {
		return err
	}
	if start.IsZero() || Day(start) < state.LaunchDay {
		return nil
	}
	return AwardInTx(tx, key, ids, boss, now)
}

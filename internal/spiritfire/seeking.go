package spiritfire

import (
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const SeekingCost = 100
const ScrollBuyCost = 500
const FragmentsPerScroll = 10
const SeekingFireChance = 1
const SeekingScrollChance = 10

var ErrSeekingEmpty = errors.New("未认主异火已耗尽，寻缘暂停；可购买焚诀古卷，等待异火回归天地后再寻缘")

type SeekingResult struct {
	Count     int
	Points    int
	Fragments int
	Scrolls   int
	Fires     []model.SpiritFire
}

// SeekingQuote always requires an explicit confirmation, including single draws.
func SeekingQuote(db *gorm.DB, user int64, action string, quantity int, now time.Time) (model.SpiritFireQuote, error) {
	q := model.SpiritFireQuote{}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		ok, err := Eligible(tx, user, now)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIneligible
		}
		cost := 0
		switch action {
		case "seek":
			if quantity != 1 && quantity != 10 {
				return errors.New("寻缘仅支持单次或十次")
			}
			var n int64
			if err := tx.Model(&model.SpiritFire{}).Where("owner_id = 0 AND listing_id = 0 AND rank > 1").Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				return ErrSeekingEmpty
			}
			cost = quantity * SeekingCost
		case "scroll_buy":
			if quantity != 1 && quantity != 10 {
				return errors.New("古卷购买仅支持1本或10本")
			}
			cost = quantity * ScrollBuyCost
		case "scroll_merge":
			if quantity != 1 {
				return errors.New("每次合成一本古卷")
			}
		default:
			return errors.New("未知寻缘操作")
		}
		q = model.SpiritFireQuote{ID: security.GenerateRandomCode(24), UserID: user, Action: action, Quantity: quantity, Points: cost, ExpiresAt: now.Add(15 * time.Minute)}
		return tx.Create(&q).Error
	})
	return q, err
}

// The caller holds the shared fire write lock. Award, wallet debit, receipt and
// quote completion commit together; replaying a completed quote cannot reroll.
func confirmSeeking(tx *gorm.DB, q model.SpiritFireQuote, now time.Time, roll func(int, int) int) error {
	ok, err := Eligible(tx, q.UserID, now)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIneligible
	}
	// Require the full quoted budget before rolling. A low balance must never
	// let a caller retry rolled-back batches until a cheaper outcome appears.
	if q.Points > 0 {
		var u model.User
		if err := tx.Select("points").Where("telegram_id = ?", q.UserID).First(&u).Error; err != nil {
			return err
		}
		if u.Points < q.Points {
			return storage.ErrPointsNotEnough
		}
	}
	result := SeekingResult{}
	switch q.Action {
	case "seek":
		if (q.Quantity != 1 && q.Quantity != 10) || q.Points != q.Quantity*SeekingCost {
			return ErrChanged
		}
		var free []model.SpiritFire
		if err := tx.Where("owner_id = 0 AND listing_id = 0 AND rank > 1").Order("id").Find(&free).Error; err != nil {
			return err
		}
		if len(free) == 0 {
			return ErrSeekingEmpty
		}
		for i := 0; i < q.Quantity && len(free) > 0; i++ {
			result.Count++
			result.Fragments++
			if roll(1, 100) <= SeekingScrollChance {
				result.Scrolls++
			}
			if roll(1, 100) > SeekingFireChance {
				continue
			}
			index := roll(0, len(free)-1)
			f := free[index]
			res := tx.Model(&model.SpiritFire{}).Where("id = ? AND owner_id = 0 AND listing_id = 0", f.ID).Updates(map[string]any{"owner_id": q.UserID, "acquired_at": now, "grace_at": nil})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrChanged
			}
			f.OwnerID = q.UserID
			f.AcquiredAt = now
			f.GraceAt = nil
			result.Fires = append(result.Fires, f)
			free = append(free[:index], free[index+1:]...)
		}
		result.Points = result.Count * SeekingCost
	case "scroll_buy":
		if (q.Quantity != 1 && q.Quantity != 10) || q.Points != q.Quantity*ScrollBuyCost {
			return ErrChanged
		}
		result.Count = q.Quantity
		result.Scrolls = q.Quantity
		result.Points = q.Points
	case "scroll_merge":
		if q.Quantity != 1 || q.Points != 0 {
			return ErrChanged
		}
		result.Count = 1
		result.Scrolls = 1
		result.Fragments = -FragmentsPerScroll
	default:
		return ErrChanged
	}
	if err := storage.ApplyPointDeltaInTx(tx, q.UserID, -result.Points, "spirit_fire_"+q.Action, fmt.Sprintf("异火寻缘：%s ×%d", q.Action, result.Count), "spirit_fire", q.ID); err != nil {
		return err
	}
	// MarkHoldings upserts the wallet without replacing scrolls or fragments.
	if len(result.Fires) > 0 {
		if err := MarkHoldings(tx, q.UserID, now); err != nil {
			return err
		}
	} else {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SpiritFireWallet{UserID: q.UserID}).Error; err != nil {
			return err
		}
	}
	res := tx.Model(&model.SpiritFireWallet{}).Where("user_id = ? AND fragments >= ?", q.UserID, max(0, -result.Fragments)).Updates(map[string]any{
		"scrolls":       gorm.Expr("scrolls + ?", result.Scrolls),
		"fragments":     gorm.Expr("fragments + ?", result.Fragments),
		"seeking_spent": gorm.Expr("seeking_spent + ?", result.Points),
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("焚诀残页不足10张")
	}
	for _, f := range result.Fires {
		if err := tx.Create(&model.SpiritFireNotice{UserID: q.UserID, Text: fmt.Sprintf("🔥 异火寻缘获得全服唯一【%s】！发送“我的异火”查看并选择启用。", f.Name)}).Error; err != nil {
			return err
		}
	}
	if err := Audit(tx, q.UserID, q.Action, 0, q.ID+" "+Encode(result)); err != nil {
		return err
	}
	return tx.Model(&q).Updates(map[string]any{"done": true, "result_json": Encode(result)}).Error
}

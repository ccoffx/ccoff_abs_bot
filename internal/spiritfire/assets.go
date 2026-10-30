package spiritfire

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func Owned(tx *gorm.DB, user int64, id uint) (model.SpiritFire, error) {
	var f model.SpiritFire
	err := tx.Where("id = ? AND owner_id = ? AND listing_id = 0 AND rank > 1", id, user).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrLocked
	}
	return f, err
}
func LoadoutAt(tx *gorm.DB, user int64, day string) ([]uint, error) {
	var p model.SpiritFireLoadout
	err := tx.Where("user_id = ? AND effective_day <= ?", user, day).Order("effective_day DESC").First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return []uint{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []uint
	err = json.Unmarshal([]byte(p.FireIDs), &ids)
	return ids, err
}

// SaveLoadout is only called after the singleton write lock is acquired.
func SaveLoadout(tx *gorm.DB, user int64, ids []uint, now time.Time) error {
	p := model.SpiritFireLoadout{UserID: user, EffectiveDay: Tomorrow(now), FireIDs: Encode(ids)}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "effective_day"}}, DoUpdates: clause.AssignmentColumns([]string{"fire_ids"})}).Create(&p).Error
}
func Equip(db *gorm.DB, user int64, ids []uint, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
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
		seen := map[uint]bool{}
		for _, id := range ids {
			if seen[id] {
				return errors.New("不能重复启用同一种异火")
			}
			seen[id] = true
			if _, err := Owned(tx, user, id); err != nil {
				return err
			}
			if len(ids) > 1 {
				var n int64
				if err := tx.Model(&model.SpiritFireMastery{}).Where("user_id = ? AND fire_id = ?", user, id).Count(&n).Error; err != nil {
					return err
				}
				if n != 1 {
					return errors.New("融合的每一种异火都必须已掌控")
				}
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if err := SaveLoadout(tx, user, ids, now); err != nil {
			return err
		}
		return Audit(tx, user, "EQUIP", 0, Encode(ids))
	})
}
func RemoveFromLoadout(tx *gorm.DB, user int64, id uint, now time.Time) error {
	ids, err := LoadoutAt(tx, user, Tomorrow(now))
	if err != nil {
		return err
	}
	next := make([]uint, 0, len(ids))
	for _, v := range ids {
		if v != id {
			next = append(next, v)
		}
	}
	return SaveLoadout(tx, user, next, now)
}
func MasteryCount(tx *gorm.DB, user int64) (int64, error) {
	var n int64
	err := tx.Model(&model.SpiritFireMastery{}).Where("user_id = ?", user).Count(&n).Error
	return n, err
}
func Quote(db *gorm.DB, user int64, id uint, action string, now time.Time) (model.SpiritFireQuote, error) {
	q := model.SpiritFireQuote{}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		f, err := Owned(tx, user, id)
		if err != nil {
			return err
		}
		r, err := RulesAt(tx, Day(now))
		if err != nil {
			return err
		}
		q = model.SpiritFireQuote{ID: security.GenerateRandomCode(24), UserID: user, FireID: id, Action: action, AcquiredAt: f.AcquiredAt, ExpiresAt: now.Add(15 * time.Minute)}
		switch action {
		case "learn":
			ok, err := Eligible(tx, user, now)
			if err != nil {
				return err
			}
			if !ok {
				return ErrIneligible
			}
			var n int64
			if err := tx.Model(&model.SpiritFireMastery{}).Where("user_id = ? AND fire_id = ?", user, id).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return errors.New("此异火已永久掌控，无需重新学习")
			}
			q.MasteryCount, err = MasteryCount(tx, user)
			if err != nil {
				return err
			}
			q.Points = int(q.MasteryCount) * 1500
			q.Scrolls = int(q.MasteryCount)
		case "cashout":
			q.Points = r.Prices[f.Grade] * 60 / 100
		case "release":
		default:
			return errors.New("未知确认类型")
		}
		return tx.Create(&q).Error
	})
	return q, err
}
func Confirm(db *gorm.DB, user int64, key string, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		var q model.SpiritFireQuote
		if err := tx.Where("id = ? AND user_id = ?", key, user).First(&q).Error; err != nil {
			return err
		}
		if q.Done {
			return nil
		}
		if !now.Before(q.ExpiresAt) {
			return errors.New("确认单已过期，请重新操作")
		}
		if q.Action == "seek" || q.Action == "scroll_buy" || q.Action == "scroll_merge" {
			return confirmSeeking(tx, q, now, security.RandomIntRange)
		}
		if q.Action == "rules" {
			return confirmRules(tx, q, now)
		}
		f, err := Owned(tx, user, q.FireID)
		if err != nil {
			return err
		}
		if !f.AcquiredAt.Equal(q.AcquiredAt) {
			return ErrChanged
		}
		switch q.Action {
		case "learn":
			ok, err := Eligible(tx, user, now)
			if err != nil {
				return err
			}
			if !ok {
				return ErrIneligible
			}
			var mastered int64
			if err := tx.Model(&model.SpiritFireMastery{}).Where("user_id = ? AND fire_id = ?", user, f.ID).Count(&mastered).Error; err != nil {
				return err
			}
			if mastered > 0 {
				return ErrChanged
			}
			n, err := MasteryCount(tx, user)
			if err != nil {
				return err
			}
			if n != q.MasteryCount || q.Points != int(n)*1500 || q.Scrolls != int(n) {
				return ErrChanged
			}
			if q.Scrolls > 0 {
				res := tx.Model(&model.SpiritFireWallet{}).Where("user_id = ? AND scrolls >= ?", user, q.Scrolls).UpdateColumn("scrolls", gorm.Expr("scrolls - ?", q.Scrolls))
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return errors.New("焚诀古卷不足")
				}
			}
			if err := storage.ApplyPointDeltaInTx(tx, user, -q.Points, "spirit_fire_learn", "掌控"+f.Name, "spirit_fire", q.ID); err != nil {
				return err
			}
			if err := tx.Create(&model.SpiritFireMastery{UserID: user, FireID: f.ID, CreatedAt: now}).Error; err != nil {
				return err
			}
		case "cashout", "release":
			r, err := RulesAt(tx, Day(now))
			if err != nil {
				return err
			}
			if q.Action == "cashout" && q.Points != r.Prices[f.Grade]*60/100 {
				return ErrChanged
			}
			if err := ReleaseInTx(tx, f, now, q.Action); err != nil {
				return err
			}
			if err := storage.ApplyPointDeltaInTx(tx, user, q.Points, "spirit_fire_cashout", "回收"+f.Name, "spirit_fire", q.ID); err != nil {
				return err
			}
		default:
			return errors.New("确认类型异常")
		}
		if err := Audit(tx, user, q.Action, f.ID, Encode(q)); err != nil {
			return err
		}
		return tx.Model(&q).Update("done", true).Error
	})
}
func closeListing(tx *gorm.DB, id uint) error {
	if id == 0 {
		return nil
	}
	if err := tx.Model(&model.MarketplaceListing{}).Where("id = ?", id).Update("status", "closed").Error; err != nil {
		return err
	}
	return tx.Model(&model.MarketplaceSecret{}).Where("listing_id = ? AND status = ?", id, "available").Update("status", "closed").Error
}
func ReleaseInTx(tx *gorm.DB, f model.SpiritFire, now time.Time, reason string) error {
	if err := MarkHoldings(tx, f.OwnerID, now); err != nil {
		return err
	}
	if err := closeListing(tx, f.ListingID); err != nil {
		return err
	}
	if err := RemoveFromLoadout(tx, f.OwnerID, f.ID, now); err != nil {
		return err
	}
	r := tx.Model(&model.SpiritFire{}).Where("id = ? AND owner_id = ?", f.ID, f.OwnerID).Updates(map[string]any{"owner_id": 0, "listing_id": 0, "grace_at": nil})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrChanged
	}
	return Audit(tx, 0, "RELEASE", f.ID, fmt.Sprintf("owner=%d reason=%s", f.OwnerID, reason))
}
func ReleaseUserInTx(tx *gorm.DB, user int64, now time.Time) error {
	if !Installed(tx) {
		return nil
	}
	if err := Lock(tx); err != nil {
		return err
	}
	var fs []model.SpiritFire
	if err := tx.Where("owner_id = ?", user).Find(&fs).Error; err != nil {
		return err
	}
	for _, f := range fs {
		if err := ReleaseInTx(tx, f, now, "account_deleted"); err != nil {
			return err
		}
	}
	return nil
}

// TransferInTx is invoked by the ordinary marketplace checkout transaction.
func TransferInTx(tx *gorm.DB, listing model.MarketplaceListing, buyer int64, now time.Time) error {
	if err := Lock(tx); err != nil {
		return err
	}
	var f model.SpiritFire
	if err := tx.Where("listing_id = ? AND owner_id = ?", listing.ID, listing.SellerID).First(&f).Error; err != nil {
		return err
	}
	var u model.User
	if err := tx.Where("telegram_id = ?", buyer).First(&u).Error; err != nil {
		return err
	}
	if err := RemoveFromLoadout(tx, f.OwnerID, f.ID, now); err != nil {
		return err
	}
	res := tx.Model(&model.SpiritFire{}).Where("id = ? AND listing_id = ? AND owner_id = ?", f.ID, listing.ID, f.OwnerID).Updates(map[string]any{"owner_id": buyer, "listing_id": 0, "acquired_at": now, "grace_at": nil})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrChanged
	}
	if err := MarkHoldings(tx, f.OwnerID, now); err != nil {
		return err
	}
	if err := MarkHoldings(tx, buyer, now); err != nil {
		return err
	}
	return Audit(tx, buyer, "BUY", f.ID, fmt.Sprintf("seller=%d listing=%d", f.OwnerID, listing.ID))
}
func UnlockListingInTx(tx *gorm.DB, listing uint) error {
	if err := Lock(tx); err != nil {
		return err
	}
	return tx.Model(&model.SpiritFire{}).Where("listing_id = ?", listing).Update("listing_id", 0).Error
}

func MarkHoldings(tx *gorm.DB, user int64, now time.Time) error {
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"holdings_changed_at"})}).Create(&model.SpiritFireWallet{UserID: user, HoldingsChangedAt: now}).Error
}

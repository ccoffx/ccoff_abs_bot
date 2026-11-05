package spiritfire

import (
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"gorm.io/gorm"
)

const TreasureFireOneIn = 10000

// The caller commits the closed treasure round and this award together, so a
// settlement replay cannot roll again or grant another fire.
func AwardTreasureInTx(tx *gorm.DB, pick model.TreasurePick, now time.Time) (model.SpiritFire, error) {
	return awardTreasureInTx(tx, pick, now, security.RandomIntRange)
}

func awardTreasureInTx(tx *gorm.DB, pick model.TreasurePick, now time.Time, roll func(int, int) (int, error)) (model.SpiritFire, error) {
	var empty model.SpiritFire
	if pick.Reward != 10 || pick.RoundID == 0 || pick.UserID == 0 {
		return empty, nil
	}
	if err := Lock(tx); err != nil {
		return empty, err
	}
	ok, err := Eligible(tx, pick.UserID, now)
	if err != nil || !ok {
		return empty, err
	}
	n, err := roll(1, TreasureFireOneIn)
	if err != nil || n != 1 {
		return empty, err
	}
	var free []model.SpiritFire
	if err := tx.Where("owner_id = 0 AND listing_id = 0 AND rank > 1").Order("id").Find(&free).Error; err != nil {
		return empty, err
	}
	if len(free) == 0 {
		return empty, nil
	}
	index, err := roll(0, len(free)-1)
	if err != nil {
		return empty, err
	}
	f := free[index]
	res := tx.Model(&model.SpiritFire{}).Where("id = ? AND owner_id = 0 AND listing_id = 0", f.ID).
		Updates(map[string]any{"owner_id": pick.UserID, "acquired_at": now, "grace_at": nil})
	if res.Error != nil {
		return empty, res.Error
	}
	if res.RowsAffected != 1 {
		return empty, ErrChanged
	}
	if err := MarkHoldings(tx, pick.UserID, now); err != nil {
		return empty, err
	}
	if err := tx.Create(&model.SpiritFireNotice{UserID: pick.UserID, Text: fmt.Sprintf("🔥 寻宝奇遇！你的 10 积分宝箱额外开出全服唯一【%s】。私聊发送“我的异火”查看并选择启用。", f.Name)}).Error; err != nil {
		return empty, err
	}
	if err := Audit(tx, pick.UserID, "TREASURE", f.ID, fmt.Sprintf("round:%d box:%d chance:1/%d", pick.RoundID, pick.BoxIndex+1, TreasureFireOneIn)); err != nil {
		return empty, err
	}
	f.OwnerID, f.AcquiredAt, f.GraceAt = pick.UserID, now, nil
	return f, nil
}

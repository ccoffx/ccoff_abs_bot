package account

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
)

var ErrRebindAssetConflict = errors.New("目标 Telegram 已有冲突的独立资产，请先联系管理员处理后再换绑")

// Ownership fields are explicit. Telegram membership/security locks and audit
// actors remain attached to the person who actually performed the action.
var rebindAssetModels = []struct {
	value  any
	fields []string
}{
	{&model.InviteCode{}, []string{"UsedByID", "OwnerUserID", "CashedOutByID"}},
	{&model.RenewCode{}, []string{"UsedByID", "OwnerUserID", "CashedOutByID"}},
	{&model.ReferralCode{}, []string{"UserID"}},
	{&model.ReferralActivation{}, []string{"InviterID", "InviteeID"}},
	{&model.ReferralDailyActivationQuota{}, []string{"InviterID"}},
	{&model.ReferralMonthlyRewardQuota{}, []string{"InviterID"}},
	{&model.WhitelistCode{}, []string{"UsedByID"}},
	{&model.RedPacket{}, []string{"SenderID"}},
	{&model.RedPacketGrab{}, []string{"UserID"}},
	{&model.LotteryParticipant{}, []string{"UserID"}},
	{&model.LotteryWinner{}, []string{"UserID"}},
	{&model.LotteryClaimLog{}, []string{"UserID"}},
	{&model.TreasurePick{}, []string{"UserID"}},
	{&model.BlindBoxReceipt{}, []string{"UserID"}},
	{&model.ExchangeCodeReceipt{}, []string{"UserID"}},
	{&model.BookRequest{}, []string{"UserID", "AssigneeID"}},
	{&model.Cultivation{}, []string{"UserID"}},
	{&model.BreakthroughAttempt{}, []string{"UserID"}},
	{&model.BreakthroughRitual{}, []string{"UserID"}},
	{&model.BreakthroughGuard{}, []string{"UserID"}},
	{&model.CultivationTreasureExchange{}, []string{"UserID"}},
	{&model.Inventory{}, []string{"UserID"}},
	{&model.GardenPlot{}, []string{"UserID"}},
	{&model.GardenPlanting{}, []string{"UserID"}},
	{&model.GardenSeedPurchase{}, []string{"UserID"}},
	{&model.GardenHerbMarketSale{}, []string{"UserID"}},
	{&model.GardenRecipeUnlock{}, []string{"UserID"}},
	{&model.ItemUsageLog{}, []string{"UserID"}},
	{&model.ItemUsageQuota{}, []string{"UserID"}},
	{&model.ABSCrossDaySessionScanCursor{}, []string{"UserID"}},
	{&model.ABSCrossDaySessionAdjustment{}, []string{"UserID"}},
	{&model.ListeningAbuseRecord{}, []string{"UserID"}},
	{&model.DailyListeningStat{}, []string{"UserID"}},
	{&model.MarketplaceListing{}, []string{"SellerID"}},
	{&model.MarketplaceSecret{}, []string{"SellerID", "BuyerID"}},
	{&model.MarketplacePurchase{}, []string{"SellerID", "BuyerID"}},
	{&model.MarketplaceDispute{}, []string{"SellerID", "BuyerID"}},
	{&model.OpenRegistrationReservation{}, []string{"UserID"}},
	{&model.CompensationGrant{}, []string{"UserID"}},
	{&model.SectShopRenewClaim{}, []string{"UserID"}},
	{&model.Sect{}, []string{"OwnerID"}},
	{&model.SectMember{}, []string{"UserID"}},
	{&model.SectContributionLog{}, []string{"UserID"}},
	{&model.SectDailyTaskClaim{}, []string{"UserID"}},
	{&model.SectListeningDailyProgress{}, []string{"UserID"}},
	{&model.SectShopPurchase{}, []string{"UserID"}},
	{&model.SectCaveRetreat{}, []string{"UserID"}},
	{&model.SectHornBroadcast{}, []string{"SenderID"}},
	{&model.SectHornDelivery{}, []string{"UserID"}},
	{&model.SectLotteryEntry{}, []string{"UserID"}},
	{&model.SectLotteryPrize{}, []string{"AssignedUserID"}},
	{&model.SectLotteryWinner{}, []string{"UserID"}},
	{&model.SectLotteryReminder{}, []string{"UserID"}},
	{&model.SectSecretRealmEvent{}, []string{"GuardianUserID"}},
	{&model.SectSecretRealmParticipant{}, []string{"UserID"}},
	{&model.SectTechnologyLog{}, []string{"UserID"}},
	{&model.SpiritFire{}, []string{"OwnerID"}},
	{&model.SpiritFireMastery{}, []string{"UserID"}},
	{&model.SpiritFireWallet{}, []string{"UserID"}},
	{&model.SpiritFireLoadout{}, []string{"UserID"}},
	{&model.SpiritFireDaily{}, []string{"UserID"}},
	{&model.SpiritFireQuote{}, []string{"UserID"}},
	{&model.SpiritFireNotice{}, []string{"UserID"}},
	{&model.SignInStreak{}, []string{"UserID"}},
	{&model.SignInLog{}, []string{"UserID"}},
	{&model.SignInRewardClaim{}, []string{"UserID"}},
	{&model.PointTransaction{}, []string{"UserID"}},
	{&model.WorldBossParticipant{}, []string{"UserID"}},
}

func migrateRebindAssets(tx *gorm.DB, oldID, newID int64) error {
	if oldID == newID {
		return nil
	}
	// A recipient's simple wallet is additive. Independent profiles, plots,
	// event participation and quotas must never be overwritten on conflict.
	var rows []model.Inventory
	if err := tx.Where("user_id = ?", oldID).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var dest model.Inventory
		err := tx.Where("user_id = ? AND item_name = ?", newID, row.ItemName).First(&dest).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := tx.Unscoped().Model(&dest).Update("quantity", gorm.Expr("quantity + ?", row.Quantity)).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&row).Error; err != nil {
			return err
		}
	}
	var winners []model.LotteryWinner
	if err := tx.Unscoped().Where("user_id = ? AND reward_result_encrypted <> ''", oldID).Find(&winners).Error; err != nil {
		return err
	}
	for _, winner := range winners {
		identity := func(id int64) string {
			return fmt.Sprintf("activity=%d:winner=%d:user=%d", winner.ActivityID, winner.ID, id)
		}
		plain, err := security.DecryptSecretReceipt("lottery-reward-result", identity(oldID), winner.RewardResultEncrypted)
		if err != nil {
			return err
		}
		encrypted, err := security.EncryptSecretReceipt("lottery-reward-result", identity(newID), plain)
		if err != nil {
			return err
		}
		if err := tx.Unscoped().Model(&winner).Update("reward_result_encrypted", encrypted).Error; err != nil {
			return err
		}
	}
	for _, asset := range rebindAssetModels {
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(asset.value); err != nil {
			return err
		}
		for _, field := range asset.fields {
			column := stmt.Schema.LookUpField(field).DBName
			if err := tx.Unscoped().Model(asset.value).Where(column+" = ?", oldID).UpdateColumn(column, newID).Error; err != nil {
				if storage.IsUniqueConstraintError(err) {
					return fmt.Errorf("%w (%s.%s): %v", ErrRebindAssetConflict, stmt.Schema.Table, column, err)
				}
				return fmt.Errorf("migrate asset %s.%s: %w", stmt.Schema.Table, column, err)
			}
		}
	}
	return nil
}

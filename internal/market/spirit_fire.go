package market

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func handleSpiritFireListingCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "上架异火":
	default:
		return false
	}
	if !msg.Chat.IsPrivate() {
		telegram.SendPlainText(bot, msg.Chat.ID, "上架异火请私聊 Bot 操作。")
		return true
	}
	if len(fields) != 3 {
		telegram.SendPlainText(bot, msg.Chat.ID, "用法：上架异火 异火编号 单价；编号可在“我的异火”查看。挂牌48小时，手续费沿用交易行规则。")
		return true
	}
	id, err := strconv.ParseUint(fields[1], 10, 32)
	price, e := strconv.Atoi(fields[2])
	if err != nil || e != nil || id == 0 {
		telegram.SendPlainText(bot, msg.Chat.ID, "请输入正确的异火编号和积分单价。")
		return true
	}
	listing, err := createSpiritFireListing(storage.DB, msg.From.ID, textfmt.GetTelegramDisplayName(msg.From), uint(id), price, time.Now())
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, "上架失败："+textfmt.FormatPlainError(err))
		return true
	}
	telegram.SendPlainText(bot, msg.Chat.ID, fmt.Sprintf("🔥 异火已锁定并上架，商品 #%d，单价%d积分，48小时有效。可用“下架商品 %d”取消。", listing.ID, price, listing.ID))
	notifyMarketplaceListingCreated(bot, listing.ID, spiritfire.ListingType, listing.Name, price, 1, listing.SellerName)
	return true
}
func createSpiritFireListing(db *gorm.DB, user int64, name string, id uint, price int, now time.Time) (model.MarketplaceListing, error) {
	var listing model.MarketplaceListing
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		f, err := spiritfire.Owned(tx, user, id)
		if err != nil {
			return err
		}
		var account model.User
		if err := tx.Where("telegram_id = ?", user).First(&account).Error; err != nil {
			return err
		}
		r, err := spiritfire.RulesAt(tx, spiritfire.Day(now))
		if err != nil {
			return err
		}
		base := r.Prices[f.Grade]
		if price < marketplaceMinOriginalValuePrice(base) {
			return storage.ErrMarketplacePriceBelowFloor
		}
		if price > marketplaceMaxOriginalValuePrice(base) {
			return storage.ErrMarketplacePriceAboveCeiling
		}
		listing = model.MarketplaceListing{SellerID: user, SellerName: marketplaceStoredSellerName(name), Name: f.Name, ItemName: f.Name, ListingType: spiritfire.ListingType, UnitQuantity: 1, Price: price, Status: marketplaceStatusActive, ExpiresAt: marketplaceListingExpiresAtPtr(now)}
		if err := createMarketplaceListingInTx(tx, &listing); err != nil {
			return err
		}
		if err := createMarketplaceSecretInTx(tx, &model.MarketplaceSecret{ListingID: listing.ID, SellerID: user, CodeHash: fmt.Sprintf("fire:%d:%d", f.ID, listing.ID), CodeEnc: spiritfire.ListingType, Preview: f.Name, Status: marketplaceSecretAvailable}); err != nil {
			return err
		}
		res := tx.Model(&model.SpiritFire{}).Where("id = ? AND owner_id = ? AND listing_id = 0", id, user).Update("listing_id", listing.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return spiritfire.ErrChanged
		}
		if err := spiritfire.RemoveFromLoadout(tx, user, id, now); err != nil {
			return err
		}
		return spiritfire.Audit(tx, user, "LIST", id, fmt.Sprintf("listing=%d price=%d", listing.ID, price))
	})
	return listing, err
}

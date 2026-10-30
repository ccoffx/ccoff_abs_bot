package market

import (
	"errors"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
)

func setupFireMarket(t *testing.T) time.Time {
	t.Helper()
	setupCodeOwnershipTest(t)
	if err := storage.DB.AutoMigrate(spiritfire.Models()...); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.AutoMigrate(&model.ListeningAbuseRecord{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := spiritfire.Initialize(storage.DB, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id IN ?", []int64{1, 2, 3}).Updates(map[string]any{"points": 10000, "is_whitelist": true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.SpiritFire{}).Where("id = 19").Updates(map[string]any{"owner_id": 1, "acquired_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	return now
}
func TestFireMarketplaceTransferAndRebuy(t *testing.T) {
	now := setupFireMarket(t)
	q, err := spiritfire.Quote(storage.DB, 1, 19, "learn", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := spiritfire.Confirm(storage.DB, 1, q.ID, now); err != nil {
		t.Fatal(err)
	}
	cash, err := spiritfire.Quote(storage.DB, 1, 19, "cashout", now)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := createSpiritFireListing(storage.DB, 1, "seller", 19, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := spiritfire.Confirm(storage.DB, 1, cash.ID, now); !errors.Is(err, spiritfire.ErrLocked) {
		t.Fatalf("listed cashout: %v", err)
	}
	if _, err := createSpiritFireListing(storage.DB, 1, "seller", 19, 2000, now); err == nil {
		t.Fatal("duplicate listing")
	}
	if err := spiritfire.Equip(storage.DB, 1, []uint{19}, now); err == nil {
		t.Fatal("listed equipment")
	}
	var visible int64
	if err := storage.DB.Model(&model.MarketplaceListing{}).Where(marketplaceListingAllowedTypeCondition(), marketplaceTypeSecret, marketplaceTypeInventory).Where("id = ?", listing.ID).Count(&visible).Error; err != nil {
		t.Fatal(err)
	}
	if visible != 1 {
		t.Fatal("unique flames hidden from market")
	}
	result, err := purchaseMarketplaceListing(2, listing.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Quantity != 1 || len(result.Codes) != 0 || result.FeeAmount != 100 || result.SellerAmount != 1900 {
		t.Fatal(result)
	}
	if _, err := purchaseMarketplaceListing(3, listing.ID, 1); err == nil {
		t.Fatal("flame purchased twice")
	}
	var f model.SpiritFire
	if err := storage.DB.First(&f, 19).Error; err != nil {
		t.Fatal(err)
	}
	if f.OwnerID != 2 || f.ListingID != 0 {
		t.Fatal(f)
	}
	var mastery int64
	storage.DB.Model(&model.SpiritFireMastery{}).Where("user_id = 2").Count(&mastery)
	if mastery != 0 {
		t.Fatal("buyer inherited seller mastery")
	}
	again, err := createSpiritFireListing(storage.DB, 2, "buyer", 19, 2000, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := purchaseMarketplaceListing(1, again.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := spiritfire.Quote(storage.DB, 1, 19, "learn", now); err == nil {
		t.Fatal("relearning offered")
	}
	// Seller never loses permanent mastery, and both successful sales have orders.
	n, err := spiritfire.MasteryCount(storage.DB, 1)
	if err != nil || n != 1 {
		t.Fatalf("mastery=%d err=%v", n, err)
	}
	var orders int64
	storage.DB.Model(&model.MarketplacePurchase{}).Where("delivery_type = ?", spiritfire.ListingType).Count(&orders)
	if orders != 2 {
		t.Fatal(orders)
	}
}
func TestFireMarketplacePriceExpiryAndAtomicFailure(t *testing.T) {
	now := setupFireMarket(t)
	for _, p := range []int{1699, 2301} {
		if _, err := createSpiritFireListing(storage.DB, 1, "s", 19, p, now); err == nil {
			t.Fatal("invalid price accepted", p)
		}
	}
	l, err := createSpiritFireListing(storage.DB, 1, "s", 19, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := purchaseMarketplaceListing(2, l.ID, 2); err == nil {
		t.Fatal("unique quantity > 1")
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = 2").Update("points", 0).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := purchaseMarketplaceListing(2, l.ID, 1); err == nil {
		t.Fatal("insufficient points accepted")
	}
	var f model.SpiritFire
	storage.DB.First(&f, 19)
	if f.OwnerID != 1 || f.ListingID != l.ID {
		t.Fatal("ownership not rolled back", f)
	}
	if _, err := closeMarketplaceListingByID(storage.DB, l.ID); err != nil {
		t.Fatal(err)
	}
	storage.DB.First(&f, 19)
	if f.OwnerID != 1 || f.ListingID != 0 {
		t.Fatal(f)
	}
	expired := now.Add(-49 * time.Hour)
	l, err = createSpiritFireListing(storage.DB, 1, "s", 19, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&l).Update("expires_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := purchaseMarketplaceListing(2, l.ID, 1); err == nil {
		t.Fatal("expired listing sold")
	}
	if _, err := closeMarketplaceListingByID(storage.DB, l.ID); err != nil {
		t.Fatal(err)
	}
	storage.DB.First(&f, 19)
	if f.ListingID != 0 {
		t.Fatal(f)
	}
}

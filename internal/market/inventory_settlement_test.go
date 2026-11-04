package market

import (
	"errors"
	"testing"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func setupInventorySettlement(t *testing.T) uint {
	t.Helper()
	setupCodeOwnershipTest(t)
	if err := storage.DB.AutoMigrate(&model.Inventory{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE UNIQUE INDEX idx_test_inventory ON inventories(user_id, item_name) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.Inventory{UserID: 1, ItemName: "测试物品", Quantity: 5}).Error; err != nil {
		t.Fatal(err)
	}
	id, err := createMarketplaceInventoryListing(1, "seller", "测试物品", 100, 5)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func inventorySettlementQuantity(t *testing.T, user int64) int {
	t.Helper()
	var quantity int
	if err := storage.DB.Model(&model.Inventory{}).Where("user_id = ?", user).
		Select("COALESCE(SUM(quantity), 0)").Scan(&quantity).Error; err != nil {
		t.Fatal(err)
	}
	return quantity
}

func TestInventoryPurchaseAndCloseConserveStockAndPoints(t *testing.T) {
	id := setupInventorySettlement(t)
	result, err := purchaseMarketplaceListing(2, id, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Quantity != 2 || result.GrossAmount != 200 || result.FeeAmount != 10 || result.SellerAmount != 190 {
		t.Fatalf("incorrect settlement: %+v", result)
	}
	refund, err := closeMarketplaceListingInTx(storage.DB, 1, id)
	if err != nil {
		t.Fatal(err)
	}
	if refund != 3 || inventorySettlementQuantity(t, 1) != 3 || inventorySettlementQuantity(t, 2) != 2 {
		t.Fatal("purchase or close created/lost inventory")
	}
	if _, err := closeMarketplaceListingInTx(storage.DB, 1, id); !errors.Is(err, storage.ErrMarketplaceCloseNotFound) {
		t.Fatalf("closed stock refunded again: %v", err)
	}
	var orders []model.MarketplacePurchase
	if err := storage.DB.Where("listing_id = ?", id).Find(&orders).Error; err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 {
		t.Fatalf("expected two item receipts: %+v", orders)
	}
	for _, order := range orders {
		if order.Quantity != 1 || order.GrossAmount != 100 || order.FeeAmount != 5 || order.SellerAmount != 95 {
			t.Fatalf("receipt differs from actual settlement: %+v", order)
		}
	}
	for user, want := range map[int64]int{1: 1190, 2: 800} {
		var account model.User
		if err := storage.DB.Where("telegram_id = ?", user).First(&account).Error; err != nil {
			t.Fatal(err)
		}
		if account.Points != want {
			t.Fatalf("user %d points=%d want=%d", user, account.Points, want)
		}
	}
}

func TestInventorySettlementRejectsMissingOrInconsistentItem(t *testing.T) {
	for _, itemName := range []string{"", "其他物品", " 测试物品 "} {
		t.Run(itemName, func(t *testing.T) {
			id := setupInventorySettlement(t)
			if err := storage.DB.Model(&model.MarketplaceListing{}).Where("id = ?", id).Update("item_name", itemName).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := purchaseMarketplaceListing(2, id, 1); !errors.Is(err, storage.ErrInvalidMarketplaceListing) {
				t.Fatalf("invalid asset was purchased: %v", err)
			}
			if _, err := closeMarketplaceListingInTx(storage.DB, 1, id); !errors.Is(err, storage.ErrInvalidMarketplaceListing) {
				t.Fatalf("invalid asset was refunded: %v", err)
			}
			if inventorySettlementQuantity(t, 1) != 0 || inventorySettlementQuantity(t, 2) != 0 {
				t.Fatal("invalid listing granted invented inventory")
			}
			var stock int64
			if err := storage.DB.Model(&model.MarketplaceSecret{}).Where("listing_id = ? AND status = ?", id, marketplaceSecretAvailable).Count(&stock).Error; err != nil {
				t.Fatal(err)
			}
			var transactions int64
			if err := storage.DB.Model(&model.PointTransaction{}).Count(&transactions).Error; err != nil {
				t.Fatal(err)
			}
			if stock != 5 || transactions != 0 {
				t.Fatalf("failed settlement changed stock or points: stock=%d transactions=%d", stock, transactions)
			}
		})
	}
}

func TestMarketplacePurchaseRejectsNonpositiveQuantity(t *testing.T) {
	id := setupInventorySettlement(t)
	for _, quantity := range []int{0, -1} {
		if _, err := purchaseMarketplaceListing(2, id, quantity); !errors.Is(err, storage.ErrInvalidMarketplaceListing) {
			t.Fatalf("invalid quantity %d became a purchase: %v", quantity, err)
		}
	}
	if inventorySettlementQuantity(t, 2) != 0 {
		t.Fatal("invalid quantity delivered inventory")
	}
}

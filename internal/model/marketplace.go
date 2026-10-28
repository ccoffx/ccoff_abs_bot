package model

import (
	"time"

	"gorm.io/gorm"
)

type MarketplaceListing struct {
	gorm.Model

	SellerID     int64  `gorm:"index;not null"`
	SellerName   string `gorm:"index"`
	Name         string `gorm:"index;not null"`
	Description  string
	ListingType  string     `gorm:"index;not null;default:'secret'"`
	SecretSource string     `gorm:"index"`
	ItemName     string     `gorm:"index"`
	UnitQuantity int        `gorm:"default:1"`
	Price        int        `gorm:"index;not null"`
	Status       string     `gorm:"index;not null;default:'active'"`
	SoldCount    int        `gorm:"default:0"`
	ExpiresAt    *time.Time `gorm:"index"`
}

func (MarketplaceListing) TableName() string {
	return "marketplace_listings"
}

type MarketplaceSecret struct {
	gorm.Model

	ListingID   uint   `gorm:"index;not null"`
	SellerID    int64  `gorm:"index;not null"`
	CodeHash    string `gorm:"index;not null"`
	CodeEnc     string `gorm:"not null"`
	Preview     string
	TokenSource string `gorm:"index"`
	TokenRefID  uint   `gorm:"index"`
	Status      string `gorm:"index;not null;default:'available'"`
	BuyerID     int64  `gorm:"index"`
	SoldAt      *time.Time
}

func (MarketplaceSecret) TableName() string {
	return "marketplace_secrets"
}

type MarketplacePurchase struct {
	gorm.Model

	ListingID    uint   `gorm:"index;not null"`
	SecretID     uint   `gorm:"index;not null"`
	SellerID     int64  `gorm:"index;not null"`
	BuyerID      int64  `gorm:"index;not null"`
	ItemName     string `gorm:"index;not null"`
	DeliveryType string `gorm:"index;not null;default:'secret'"`
	Quantity     int    `gorm:"default:1"`
	Price        int
	GrossAmount  int
	FeeAmount    int
	SellerAmount int
	Status       string `gorm:"index;not null;default:'paid'"`
	CodePreview  string
}

func (MarketplacePurchase) TableName() string {
	return "marketplace_purchases"
}

type MarketplaceDispute struct {
	gorm.Model

	PurchaseID uint   `gorm:"index;not null"`
	ListingID  uint   `gorm:"index;not null"`
	SellerID   int64  `gorm:"index;not null"`
	BuyerID    int64  `gorm:"index;not null"`
	Reason     string `gorm:"not null"`
	Status     string `gorm:"index;not null;default:'open'"`
}

func (MarketplaceDispute) TableName() string {
	return "marketplace_disputes"
}

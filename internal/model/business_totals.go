package model

// Derived counters follow their source records, including corrections, deletion
// and account transfers. They are rebuilt once when first introduced.
type GardenProductionTotal struct {
	UserID       int64 `gorm:"primaryKey;autoIncrement:false"`
	HarvestCount int64 `gorm:"not null;default:0"`
	AlchemyCount int64 `gorm:"not null;default:0"`
}

type SectShopContributionTotal struct {
	SectID int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID int64 `gorm:"primaryKey;autoIncrement:false"`
	Spent  int64 `gorm:"not null;default:0"`
}

type BusinessTotalMigration struct {
	Key string `gorm:"primaryKey"`
}

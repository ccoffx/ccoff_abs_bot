package model

import (
	"time"

	"gorm.io/gorm"
)

type Inventory struct {
	gorm.Model
	UserID   int64  `gorm:"index;not null"`
	ItemName string `gorm:"index;not null"`
	Quantity int    `gorm:"default:0"`
}

// 药园灵田。
// 同一用户同一块田只能存在一条有效记录，唯一性由数据库索引保证。
type GardenPlot struct {
	gorm.Model
	UserID     int64     `gorm:"index;not null"`
	PlotNo     int       `gorm:"index;not null"`
	UnlockedAt time.Time `gorm:"index;not null"`
}

func (GardenPlot) TableName() string {
	return "garden_plots"
}

// 药园种植记录。
// MVP 中成熟按 matures_at 即时计算，status=growing 表示未收获。
type GardenPlanting struct {
	gorm.Model
	UserID           int64      `gorm:"index;not null"`
	PlotID           uint       `gorm:"index;not null"`
	PlotNo           int        `gorm:"index;not null"`
	SeedKey          string     `gorm:"index;not null"`
	SeedName         string     `gorm:"not null"`
	HerbName         string     `gorm:"index;not null"`
	PlantedAt        time.Time  `gorm:"index;not null"`
	MaturesAt        time.Time  `gorm:"index;not null"`
	HarvestedAt      *time.Time `gorm:"index"`
	MatureNotifiedAt *time.Time `gorm:"index"`
	Quantity         int        `gorm:"default:0"`
	Status           string     `gorm:"index;not null;default:'growing'"`
}

func (GardenPlanting) TableName() string {
	return "garden_plantings"
}

// 药园种子每日限购。
// day_key 使用北京时间 YYYY-MM-DD。
type GardenSeedPurchase struct {
	gorm.Model
	UserID   int64  `gorm:"index;not null"`
	SeedKey  string `gorm:"index;not null"`
	DayKey   string `gorm:"index;not null"`
	Quantity int    `gorm:"default:0"`
}

func (GardenSeedPurchase) TableName() string {
	return "garden_seed_purchases"
}

// GardenHerbMarketSale records per-user daily urgent herb market quota.
// day_key uses Beijing date YYYY-MM-DD.
type GardenHerbMarketSale struct {
	gorm.Model
	UserID   int64  `gorm:"index;not null"`
	SeedKey  string `gorm:"index;not null"`
	DayKey   string `gorm:"index;not null"`
	Quantity int    `gorm:"default:0"`
}

func (GardenHerbMarketSale) TableName() string {
	return "garden_herb_market_sales"
}

// 丹方解锁记录。
// 丹方永久解锁，同一用户同一丹方只允许一条有效记录。
type GardenRecipeUnlock struct {
	gorm.Model
	UserID     int64     `gorm:"index;not null"`
	RecipeKey  string    `gorm:"index;not null"`
	UnlockedAt time.Time `gorm:"index;not null"`
}

func (GardenRecipeUnlock) TableName() string {
	return "garden_recipe_unlocks"
}

// 丹药使用日志。
// 用于记录每一次实际吞服丹药的时间。
// 老逻辑和新逻辑都会用到它，所以不能删除。
type ItemUsageLog struct {
	gorm.Model
	UserID   int64     `gorm:"index;not null"`
	ItemName string    `gorm:"index;not null"`
	UsedAt   time.Time `gorm:"index"`
}

// 丹药周期额度表。
// 用于保证“每周 / 每月最多使用几次”的限制在并发下也安全。
// 例如：user_id + item_name + period_key 唯一。
type ItemUsageQuota struct {
	gorm.Model
	UserID    int64  `gorm:"index;not null"`
	ItemName  string `gorm:"index;not null"`
	PeriodKey string `gorm:"index;not null"`
	UsedCount int    `gorm:"default:0"`
}

func (ItemUsageQuota) TableName() string {
	return "item_usage_quotas"
}

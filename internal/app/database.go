package app

import (
	"fmt"
	"log"
	"time"

	cache "github.com/patrickmn/go-cache"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/operations"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func InitDB() {
	var err error

	storage.EnsureSQLiteDatabaseDir(config.AppConfig.DbURL)

	storage.DB, err = gorm.Open(sqlite.Open(config.AppConfig.DbURL+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("数据库连接失败: %s", textfmt.FormatPlainError(err))
	}

	sqlDB, err := storage.DB.DB()
	if err != nil {
		log.Fatal(err)
	}
	if err == nil {
		if err := operations.CheckCompatibility(sqlDB); err != nil {
			log.Fatal(err)
		}
		// SQLite 同一时间只有一个 writer。
		// 这里限制为单连接，避免多连接并发写入互相抢锁，造成 database is locked。
		sqlDB.SetMaxOpenConns(config.AppConfig.DatabaseMaxOpenConns)
		sqlDB.SetMaxIdleConns(config.AppConfig.DatabaseMaxIdleConns)
		sqlDB.SetConnMaxLifetime(time.Hour)

		if err := storage.DB.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", config.AppConfig.DatabaseBusyTimeoutMS)).Error; err != nil {
			log.Printf("⚠️ 设置 SQLite busy_timeout 失败: %s", textfmt.FormatPlainError(err))
		}
		if err := storage.DB.Exec("PRAGMA journal_mode = WAL").Error; err != nil {
			log.Printf("⚠️ 设置 SQLite WAL 模式失败: %s", textfmt.FormatPlainError(err))
		}
		if err := storage.DB.Exec("PRAGMA synchronous = NORMAL").Error; err != nil {
			log.Printf("⚠️ 设置 SQLite synchronous=NORMAL 失败: %s", textfmt.FormatPlainError(err))
		}
	}

	if err := storage.DB.AutoMigrate(
		&model.User{},
		&model.InviteCode{},
		&model.RenewCode{},
		&model.WhitelistCode{},
		&model.ReferralCode{},
		&model.ReferralActivation{},
		&model.ReferralDailyActivationQuota{},
		&model.ReferralMonthlyRewardQuota{},
		&model.MarketplaceListing{},
		&model.MarketplaceSecret{},
		&model.MarketplacePurchase{},
		&model.MarketplaceDispute{},
		&model.RedPacket{},
		&model.RedPacketGrab{},
		&model.SystemConfig{},
		&model.OpenRegistrationCampaign{},
		&model.OpenRegistrationReservation{},
		&model.CompensationCampaign{},
		&model.CompensationGrant{},
		&model.AuditLog{},
		&model.SecurityAttemptLock{},
		&model.ListeningAbuseRecord{},
		&model.PointTransaction{},
		&model.SignInStreak{},
		&model.SignInLog{},
		&model.SignInRewardClaim{},
		&model.AutoDeleteMsg{},
		&model.Cultivation{},
		&model.CultivationRealmConfig{},
		&model.CultivationMinorRealmConfig{},
		&model.BreakthroughConfig{},
		&model.BreakthroughAttempt{},
		&model.BreakthroughRitual{},
		&model.BreakthroughGuard{},
		&model.WorldBossEvent{},
		&model.WorldBossParticipant{},
		&model.Inventory{},
		&model.GardenPlot{},
		&model.GardenPlanting{},
		&model.GardenSeedPurchase{},
		&model.GardenHerbMarketSale{},
		&model.GardenRecipeUnlock{},
		&model.ItemUsageLog{},
		&model.ItemUsageQuota{},
		&model.TreasureRound{},
		&model.TreasurePick{},
		&model.BookRequest{},
		&model.BookRequestLog{},
		&model.BookRequestAnnouncementDelivery{},
		&model.BookRequestAnnouncementCandidateSnapshot{},
		&model.BookRequestAnnouncementPreviewCandidate{},
		&model.BookLibraryEvent{},
		&model.LotteryActivity{},
		&model.LotteryPrize{},
		&model.LotteryParticipant{},
		&model.LotteryWinner{},
		&model.LotteryClaimLog{},
		&model.Sect{},
		&model.SectMember{},
		&model.SectContributionLog{},
		&model.SectDailyTaskClaim{},
		&model.SectWeeklyTaskSettlement{},
		&model.SectListeningDailyProgress{},
		&model.DailyListeningStat{},
		&model.ABSCrossDaySessionAdjustment{},
		&model.ABSCrossDaySessionScanCursor{},
		&model.SectShopPurchase{},
		&model.SectShopRenewClaim{},
		&model.SectCaveRetreat{},
		&model.SectTechnology{},
		&model.SectTechnologyLog{},
		&model.CultivationTreasureExchange{},
		&model.SectSecretRealmEvent{},
		&model.SectSecretRealmParticipant{},
		&model.SectHornBroadcast{},
		&model.SectHornDelivery{},
		&model.SectLottery{},
		&model.SectLotteryEntry{},
		&model.SectLotteryPrize{},
		&model.SectLotteryWinner{},
		&model.SectLotteryReminder{},
	); err != nil {
		log.Fatalf("数据库结构初始化失败: %s", textfmt.FormatPlainError(err))
	}
	if security.GetSensitivePepper() == "" {
		log.Fatal("SECURITY_PEPPER is not configured; startup blocked. Configure a long-term fixed SECURITY_PEPPER in .env")
	}
	if err := storage.InitializeDatabaseIndexes(storage.DB); err != nil {
		log.Fatalf("数据库索引初始化失败: %s", textfmt.FormatPlainError(err))
	}
	if err := game.SeedDefaultCultivationConfigs(); err != nil {
		log.Fatalf("斗气默认配置初始化失败: %s", textfmt.FormatPlainError(err))
	}
	game.InitCultivationRuleCache()
	if err := operations.StampCompatibility(sqlDB); err != nil {
		log.Fatal(err)
	}
	// 初始化缓存，设置默认 5 分钟过期，每 10 分钟自动清理一次过期 Key。
	storage.GoCacheInstance = cache.New(5*time.Minute, 10*time.Minute)
}

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
	"ccoff-abs-bot/internal/sectbonus"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func InitDB() {
	var err error

	storage.EnsureSQLiteDatabaseDir(config.AppConfig.DbURL)

	// SQLite 的 busy_timeout 和 synchronous 按连接生效；写入 DSN 才能覆盖连接池中新建的连接。
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=%d&_synchronous=NORMAL", config.AppConfig.DbURL, config.AppConfig.DatabaseBusyTimeoutMS)
	storage.DB, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("数据库连接失败: %s", textfmt.FormatPlainError(err))
	}

	sqlDB, err := storage.DB.DB()
	if err != nil {
		log.Fatal(err)
	}
	if err := operations.CheckCompatibility(sqlDB); err != nil {
		log.Fatal(err)
	}
	// WAL 允许并发读取；写事务仍由 SQLite 串行处理。
	sqlDB.SetMaxOpenConns(config.AppConfig.DatabaseMaxOpenConns)
	sqlDB.SetMaxIdleConns(config.AppConfig.DatabaseMaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := storage.DB.AutoMigrate(
		&model.AnnouncementDelivery{},
		&model.JoinVerification{},
		&model.JoinVerificationPrompt{},
		&model.JoinVerificationGroupActivity{},
		&model.GroupReply{},
		&model.JoinVerificationEvent{},
		&model.User{},
		&model.AccountLifecycleClaim{},
		&model.AccountLifecycleNotice{},
		&model.OrphanAccountDeletion{},
		&model.BlindBoxReceipt{},
		&model.AdminCodeReceipt{},
		&model.ExchangeCodeReceipt{},
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
		&model.GardenAlchemyRecord{},
		&model.ItemUsageLog{},
		&model.ItemUsageQuota{},
		&model.TreasureRound{},
		&model.TreasurePick{},
		&model.TribulationRound{},
		&model.TribulationEntry{},
		&model.TribulationTitleStat{},
		&model.TribulationTitleHolder{},
		&model.TribulationTitleHistory{},
		&model.TitleEquipment{},
		&model.TitleGroup{},
		&model.TitleMember{},
		&model.TitleMemberSyncRequest{},
		&model.RankTitleHolder{},
		&model.RankTitleHistory{},
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
	if err := storage.DB.AutoMigrate(spiritfire.Models()...); err != nil {
		log.Fatalf("异火数据库结构初始化失败: %s", textfmt.FormatPlainError(err))
	}
	if err := storage.DB.AutoMigrate(sectbonus.Models()...); err != nil {
		log.Fatalf("宗主修炼加成表初始化失败: %s", err)
	}
	if err := sectbonus.Initialize(storage.DB, time.Now()); err != nil {
		log.Fatalf("宗主修炼加成初始化失败: %s", err)
	}
	if err := storage.InitializeAccountLifecycleGuards(storage.DB); err != nil {
		log.Fatalf("账号生命周期保护初始化失败: %s", err)
	}
	if err := storage.InitializeOrphanAccountDeletionGuards(storage.DB); err != nil {
		log.Fatalf("账号清理保护初始化失败: %s", err)
	}
	if err := spiritfire.Initialize(storage.DB, time.Now()); err != nil {
		log.Fatalf("异火初始化失败: %s", err)
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

package game

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func cultivationErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrCultivationNotFound):
		return "CULTIVATION_NOT_FOUND"
	case errors.Is(err, storage.ErrMaxRealmReached):
		return "MAX_REALM_REACHED"
	case errors.Is(err, storage.ErrConsolidating):
		return "CONSOLIDATING"
	case errors.Is(err, storage.ErrBreakthroughNotReady):
		return "NOT_READY"
	case errors.Is(err, storage.ErrInsufficientCultivation):
		return "INSUFFICIENT_CULTIVATION"
	case errors.Is(err, storage.ErrInsufficientPoints):
		return "INSUFFICIENT_POINTS"
	case errors.Is(err, storage.ErrNoBreakthroughPill):
		return "NO_PILL"
	case errors.Is(err, storage.ErrInvalidBreakthroughMode):
		return "INVALID_BREAKTHROUGH_MODE"
	case errors.Is(err, storage.ErrCultivationStateChanged):
		return "CULTIVATION_STATE_CHANGED"
	case errors.Is(err, storage.ErrRandomFailed):
		return "RANDOM_FAILED"
	case errors.Is(err, storage.ErrUserNotFound):
		return "USER_NOT_FOUND"
	case err != nil:
		return storage.FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

// GetOrCreateCultivation 获取或初始化用户的斗气档案
func GetOrCreateCultivation(userID int64) *model.Cultivation {
	var cul model.Cultivation

	err := storage.DB.Where("user_id = ?", userID).First(&cul).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("⚠️ 读取斗气档案失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			return nil
		}

		created, err := createCultivationIfMissing(userID)
		if err != nil {
			log.Printf("⚠️ 创建斗气档案失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
			return nil
		}
		cul = *created
	}

	// 每次读取时，根据当前累计听书时长同步当前大境界内的最高小段位
	SyncCultivationRealm(&cul)
	return &cul
}

func createCultivationIfMissing(userID int64) (*model.Cultivation, error) {
	if userID == 0 {
		return nil, fmt.Errorf("CULTIVATION_CREATE_INVALID")
	}
	cul := model.Cultivation{
		UserID:           userID,
		TotalAudioTime:   0,
		PillAudioTime:    0,
		MajorRealm:       0,
		MinorRealm:       1,
		TribulationFails: 0,
		ConsolidateUntil: time.Now().Add(-24 * time.Hour),
	}
	res := storage.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&cul)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		var existing model.Cultivation
		if err := storage.DB.Where("user_id = ?", userID).First(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	return &cul, nil
}

// GetRealmName 获取文本名称，例如 【大斗师中期】
func GetRealmName(cul *model.Cultivation) string {
	if cul == nil {
		return "【未知境界】"
	}

	realmTitle := getCultivationRealmTitle(cul.MajorRealm)
	if realmTitle == "" {
		majors := defaultCultivationRealmConfigs()
		if cul.MajorRealm < 0 || cul.MajorRealm >= len(majors) {
			return "【震古烁今大能】"
		}
		realmTitle = majors[cul.MajorRealm].TitleName
	}

	if cul.MajorRealm == 0 {
		return fmt.Sprintf("【%s】", realmTitle)
	}

	minorName := getCultivationMinorName(cul.MajorRealm, cul.MinorRealm)
	if minorName == "" {
		minors := []string{"", "初期", "中期", "后期", "圆满"}
		if cul.MinorRealm > 0 && cul.MinorRealm < len(minors) {
			minorName = minors[cul.MinorRealm]
		}
	}

	return fmt.Sprintf("【%s%s】", realmTitle, minorName)
}

// SyncCultivationRealm 根据总修为（苦修+药力），平滑自动晋升当前大境界内的“小段位”
func SyncCultivationRealm(cul *model.Cultivation) {
	if cul == nil {
		return
	}

	h := cul.TotalAudioTime + cul.PillAudioTime
	oldMajor := cul.MajorRealm
	oldMinor := cul.MinorRealm

	rules := GetCultivationRules()
	if rules == nil {
		return
	}

	// 斗之气阶段只允许按配置从 0 进入第一个突破目标，不自动跨越后续大境界。
	if cul.MajorRealm == 0 {
		if req, ok := getBreakthroughSettingFromRules(0); ok && h >= req.MinHours {
			cul.MajorRealm = req.ToMajorRealm
			cul.MinorRealm = 1
		}
	}

	// 小境界完全由 cultivation_minor_realm_configs.required_hours 驱动。
	if cul.MajorRealm > 0 {
		minors := rules.MinorRealms[cul.MajorRealm]
		if len(minors) > 0 {
			bestMinor := 1
			for _, minor := range minors {
				if h >= minor.RequiredHours && minor.MinorRealm > bestMinor {
					bestMinor = minor.MinorRealm
				}
			}
			cul.MinorRealm = bestMinor
		}
	}

	if cul.MinorRealm < 1 {
		cul.MinorRealm = 1
	}
	if cul.MinorRealm > 4 {
		cul.MinorRealm = 4
	}

	if oldMajor != cul.MajorRealm || oldMinor != cul.MinorRealm {
		// 只更新本函数负责的列（小段位，以及斗之气自动进阶时的大境界），
		// 并以读取时的大境界为条件做乐观锁。
		// 后台批量同步（notifier/宗门刷新/世界Boss加成）不持有用户级锁，
		// 若沿用全行 DB.Save(cul)，会用陈旧快照覆盖用户刚突破写入的
		// major_realm / consolidate_until / tribulation_fails，
		// 造成“扣了资源却退回原境界”的资产损失；这里改为定向条件更新规避。
		updates := map[string]interface{}{"minor_realm": cul.MinorRealm}
		if oldMajor != cul.MajorRealm {
			updates["major_realm"] = cul.MajorRealm
		}
		res := storage.DB.Model(&model.Cultivation{}).
			Where("user_id = ? AND major_realm = ?", cul.UserID, oldMajor).
			Updates(updates)
		if res.Error != nil {
			log.Printf("⚠️ 同步斗气段位失败: user=%d err=%s", cul.UserID, textfmt.FormatPlainError(res.Error))
		} else if res.RowsAffected == 0 {
			log.Printf("⚠️ 同步斗气段位未命中: user=%d old_major=%d new_major=%d new_minor=%d", cul.UserID, oldMajor, cul.MajorRealm, cul.MinorRealm)
		}
	}
}

// persistCultivationAudioTime 只更新 total_audio_time 列。
// 听书时长同步多由后台批量任务触发（不持用户级锁），必须避免全行
// DB.Save(cul) 用陈旧快照覆盖用户并发突破写入的 major_realm /
// minor_realm / consolidate_until / tribulation_fails / pill_audio_time。
func persistCultivationAudioTime(userID int64, totalAudioTime float64) {
	if userID == 0 {
		return
	}
	if totalAudioTime < 0 {
		totalAudioTime = 0
	}
	res := storage.DB.Model(&model.Cultivation{}).
		Where("user_id = ?", userID).
		UpdateColumn("total_audio_time", totalAudioTime)
	if res.Error != nil {
		log.Printf("⚠️ 更新累计听书时长失败: user=%d err=%s", userID, textfmt.FormatPlainError(res.Error))
	} else if res.RowsAffected == 0 {
		log.Printf("⚠️ 更新累计听书时长未命中: user=%d total_audio_time=%.2f", userID, totalAudioTime)
	}
}

type BreakthroughSetting struct {
	FromMajorRealm int
	ToMajorRealm   int

	PillName    string
	PointsCost  int
	MinHours    float64
	SuccessRate float64
	Cooldown    time.Duration

	GuaranteeFailCount int
	RefundRate         float64
	FailPenaltyPoints  int
}

func getBreakthroughSettingFromRules(fromMajor int) (BreakthroughSetting, bool) {
	rules := GetCultivationRules()
	if rules == nil {
		return BreakthroughSetting{}, false
	}

	cfg, ok := rules.Breakthroughs[fromMajor]
	if !ok || !cfg.Enabled {
		return BreakthroughSetting{}, false
	}

	return BreakthroughSetting{
		FromMajorRealm:     cfg.FromMajorRealm,
		ToMajorRealm:       cfg.ToMajorRealm,
		PillName:           cfg.PillName,
		PointsCost:         cfg.PointsCost,
		MinHours:           cfg.MinTotalHours,
		SuccessRate:        cfg.SuccessRate,
		Cooldown:           time.Duration(cfg.CooldownHours) * time.Hour,
		GuaranteeFailCount: cfg.GuaranteeFailCount,
		RefundRate:         cfg.RefundRate,
		FailPenaltyPoints:  cfg.FailPenaltyPoints,
	}, true
}

func isMaxCultivationRealm(major int) bool {
	rules := GetCultivationRules()
	if rules == nil {
		return major >= 5
	}

	realm, ok := rules.Realms[major]
	return ok && realm.IsMaxRealm
}

func getCultivationRealmTitle(major int) string {
	rules := GetCultivationRules()
	if rules == nil {
		return ""
	}

	realm, ok := rules.Realms[major]
	if !ok {
		return ""
	}

	if strings.TrimSpace(realm.TitleName) != "" {
		return realm.TitleName
	}

	return realm.Name
}

func getCultivationMinorName(major int, minor int) string {
	rules := GetCultivationRules()
	if rules == nil {
		return ""
	}

	for _, cfg := range rules.MinorRealms[major] {
		if cfg.MinorRealm == minor {
			return cfg.Name
		}
	}

	return ""
}

func createBreakthroughAttemptInTx(tx *gorm.DB, attempt *model.BreakthroughAttempt) error {
	if tx == nil || attempt == nil {
		return fmt.Errorf("BREAKTHROUGH_ATTEMPT_INVALID")
	}
	entry := *attempt
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("BREAKTHROUGH_ATTEMPT_CREATE_MISSED")
	}
	*attempt = entry
	return nil
}

// 1. 渡劫前置扫描预检
func HandleBreakthroughRequest(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID
	userName := msg.From.UserName
	if userName == "" {
		userName = msg.From.FirstName
	}
	safeName := textfmt.EscapeMarkdown(userName)

	cul := GetOrCreateCultivation(userID)
	if cul == nil {
		log.Printf("⚠️ 突破前斗气档案读取失败: user=%d", userID)
		telegram.ReplyTextToMessage(bot, msg, "❌ 斗气档案读取失败，请稍后再尝试突破。")
		return
	}

	if time.Now().Before(cul.ConsolidateUntil) {
		loc := time.FixedZone("CST", 8*3600)
		unlockTime := cul.ConsolidateUntil.In(loc).Format("2006-01-02 15:04")
		telegram.SendGroupAutoDeleteMessage(bot, chatID, fmt.Sprintf("⚠️ @%s 您刚刚突破境界，气血翻涌、根基不稳！\n\n请静心打坐巩固修为，**%s** 之后方可再次引动雷劫！", safeName, unlockTime))
		return
	}

	if cul.MinorRealm != 4 && cul.MajorRealm != 0 {
		telegram.SendGroupAutoDeleteMessage(bot, chatID, fmt.Sprintf("❌ @%s 您的修为尚未达到当前境界的【大圆满】瓶颈，盲目渡劫必会爆体身亡！请继续闭关听书打磨气血。", safeName))
		return
	}

	req, exists := getBreakthroughSettingFromRules(cul.MajorRealm)
	if !exists || isMaxCultivationRealm(cul.MajorRealm) {
		telegram.SendGroupAutoDeleteMessage(bot, chatID, fmt.Sprintf("⚡️ @%s 您已达到本界天道天花板，无法继续在人界突破！", safeName))
		return
	}

	// 🚨 判定标准升级：苦修 + 药力 双轨总修为
	totalH := cul.TotalAudioTime + cul.PillAudioTime
	if totalH < req.MinHours {
		telegram.SendGroupAutoDeleteMessage(bot, chatID, fmt.Sprintf("❌ @%s 突破需要总修为满 `%.1f` 小时！您当前总计仅有 `%.1f` 小时，强行渡劫必遭反噬！", safeName, req.MinHours, totalH))
		return
	}

	var u model.User
	if err := storage.DB.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		log.Printf("⚠️ 突破前钱包读取失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		telegram.ReplyTextToMessage(bot, msg, "❌ 钱包读取失败，请稍后再尝试突破。")
		return
	}

	hasPill := false
	if req.PointsCost > 0 {
		var inv model.Inventory
		err := storage.DB.Where("user_id = ? AND item_name = ?", userID, req.PillName).First(&inv).Error
		if err == nil && inv.Quantity > 0 {
			hasPill = true
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("⚠️ 突破前资源库存读取失败: user=%d item=%s err=%s", userID, textfmt.FormatPlainValue(req.PillName), textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, msg, "❌ 乾坤袋读取失败，请稍后再尝试突破。")
			return
		}
	}

	session := telegram.GetSession(userID)

	if req.PointsCost == 0 {
		// 斗之气聚气化旋
		session.SetTemp("bt_mode", "USE_INVENTORY")
		session.SetStep("WAITING_CONFIRM_BREAKTHROUGH")
		telegram.ReplyTextToMessage(bot, msg, "⚡️ 检测到您闭关苦修已感知天地斗气，是否立即【聚气化旋】，正式踏上修炼之途？\n👉 请回复 `确认渡劫` 或 `取消`。")
		telegram.UserSessions.Store(userID, session)
	} else if IsCultivationTreasureItemName(req.PillName) {
		// 至宝突破：必须持有对应至宝 + 足够积分，不允许代购（至宝由秘境信物兑换或世界 Boss 掉落）。
		if !hasPill {
			telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("❌ 突破所需至宝**【%s】**不在乾坤袋中。可在「聚宝斋 → 突破材料」用秘境信物兑换，或参与世界 Boss 获得。", textfmt.InventoryItemMarkdownName(req.PillName)))
		} else if u.Points < req.PointsCost {
			telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("❌ 至宝虽已入手，但渡劫需缴纳 `%d` 积分祭炼费，您当前仅有 `%d` 积分。", req.PointsCost, u.Points))
		} else {
			session.SetTemp("bt_mode", "USE_INVENTORY")
			session.SetStep("WAITING_CONFIRM_BREAKTHROUGH")
			telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("⚡️ 乾坤袋中至宝**【%s】**已就位。是否献祭此宝并缴纳 `%d` 积分渡劫费，引动九重天劫？\n👉 请回复 `确认渡劫` 或 `取消`。", textfmt.InventoryItemMarkdownName(req.PillName), req.PointsCost))
			telegram.UserSessions.Store(userID, session)
		}
	} else if hasPill {
		// 背包有药
		session.SetTemp("bt_mode", "USE_INVENTORY")
		session.SetStep("WAITING_CONFIRM_BREAKTHROUGH")
		telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("⚡️ 检测到乾坤袋中备有**【%s】**，是否立即吞服并引动雷劫？\n👉 请回复 `确认渡劫` 或 `取消`。", textfmt.InventoryItemMarkdownName(req.PillName)))
		telegram.UserSessions.Store(userID, session)
	} else if u.Points >= req.PointsCost {
		// 背包无药，但钱够代购
		session.SetTemp("bt_mode", "AUTO_BUY")
		session.SetStep("WAITING_CONFIRM_BREAKTHROUGH")
		telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("⚡️ 您乾坤袋中暂无【%s】。\n💰 您的积分充足，是否授权天道商行自动扣除 `%d` 积分代购**【%s】**并立即开始渡劫？\n👉 请回复 `确认代购并渡劫` 或 `取消`。", textfmt.InventoryItemMarkdownName(req.PillName), req.PointsCost, textfmt.InventoryItemMarkdownName(req.PillName)))
		telegram.UserSessions.Store(userID, session)
	} else {
		// 没钱没药，无情驳回
		telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf("❌ 突破失败。您既无**【%s】**，也缺少积分在聚宝斋购买（当前仅有 `%d` 积分，需要 `%d` 积分）。请努力积攒灵石！", textfmt.InventoryItemMarkdownName(req.PillName), u.Points, req.PointsCost))
	}
}

type BreakthroughOutcome struct {
	NewMajor           int
	NewMinor           int
	PillName           string
	Roll               int
	SuccessRate        float64
	BaseSuccessRate    float64
	IsGuaranteed       bool
	IsSuccess          bool
	RefundPoints       int
	FailPenaltyPoints  int
	TribulationFails   int
	GuaranteeFailCount int
	GuardCount         int
	AttemptID          uint
}

func ResolveBreakthrough(userID int64, mode string, ritualID uint, now time.Time) (*BreakthroughOutcome, error) {
	return resolveBreakthroughWithRoll(userID, mode, ritualID, now, func() (int, error) {
		value, err := rand.Int(rand.Reader, big.NewInt(100))
		if err != nil {
			return 0, storage.ErrRandomFailed
		}
		return int(value.Int64()) + 1, nil
	})
}

func resolveBreakthroughWithRoll(userID int64, mode string, ritualID uint, now time.Time, rollDice func() (int, error)) (*BreakthroughOutcome, error) {
	var (
		newMajor           int
		newMinor           int
		pillName           string
		roll               int
		successRate        float64
		isGuaranteed       bool
		isSuccess          bool
		refundPoints       int
		failPenaltyPoints  int
		tribulationFails   int
		guaranteeFailCount int
		attemptID          uint
	)

	var baseSuccessRate float64
	var guardCount int

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var cul model.Cultivation
		if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrCultivationNotFound
			}
			return err
		}

		req, exists := getBreakthroughSettingFromRules(cul.MajorRealm)
		if ritualID == 0 && (!exists || isMaxCultivationRealm(cul.MajorRealm)) {
			return storage.ErrMaxRealmReached
		}

		var ritual *model.BreakthroughRitual
		if ritualID != 0 {
			var loaded model.BreakthroughRitual
			if err := tx.First(&loaded, ritualID).Error; err != nil {
				return err
			}
			if loaded.Status != BreakthroughRitualWaiting {
				return ErrBreakthroughRitualClosed
			}
			if now.Before(loaded.Deadline) {
				return ErrBreakthroughRitualNotDue
			}
			if loaded.UserID != userID || loaded.Mode != mode || loaded.FromMajor != cul.MajorRealm || loaded.TribulationFails != cul.TribulationFails {
				return storage.ErrCultivationStateChanged
			}
			var rulesErr error
			req, rulesErr = decodeBreakthroughRitualRules(loaded)
			if rulesErr != nil {
				return rulesErr
			}
			ritual = &loaded
		} else {
			var active int64
			if err := tx.Model(&model.BreakthroughRitual{}).Where("user_id = ? AND status IN ?", userID, []string{BreakthroughRitualPublishing, BreakthroughRitualWaiting}).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return ErrBreakthroughRitualPending
			}
		}
		if ritual != nil {
			var user model.User
			if err := tx.Where("telegram_id = ?", userID).First(&user).Error; err != nil {
				return err
			}
			usable, err := storage.UserHasUsableLocalAbsAccountTxAt(tx, user, now)
			if err != nil {
				return err
			}
			if !usable || !storage.IsFormalAccount(user) {
				return ErrBreakthroughGuardIneligible
			}
			var guards int64
			if err := tx.Model(&model.BreakthroughGuard{}).Where("ritual_id = ? AND status = ?", ritualID, "joined").Count(&guards).Error; err != nil {
				return err
			}
			guardCount = int(guards)
		}
		baseSuccessRate = BreakthroughGuardSuccessRate(req.SuccessRate, 0)
		req.SuccessRate = BreakthroughGuardSuccessRate(baseSuccessRate, guardCount)

		pillName = req.PillName
		successRate = req.SuccessRate
		guaranteeFailCount = req.GuaranteeFailCount

		// 事务内再次校验冷却，避免确认后状态变化。
		if now.Before(cul.ConsolidateUntil) {
			return storage.ErrConsolidating
		}

		// 事务内再次校验是否大圆满。
		if cul.MinorRealm != 4 && cul.MajorRealm != 0 {
			return storage.ErrBreakthroughNotReady
		}

		// 事务内再次校验总修为。
		totalH := cul.TotalAudioTime + cul.PillAudioTime
		if totalH < req.MinHours {
			return storage.ErrInsufficientCultivation
		}

		resourceCostPoints := 0
		pillConsumed := false

		// 1. 扣突破资源：自动代购扣积分，背包模式扣除对应材料。
		if mode == "AUTO_BUY" {
			if req.PointsCost <= 0 || IsCultivationTreasureItemName(req.PillName) {
				// 初始突破无需代购，至宝突破禁止代购。
				return storage.ErrInvalidBreakthroughMode
			}

			if err := storage.ApplyPointDeltaInTx(
				tx,
				userID,
				-req.PointsCost,
				"breakthrough_auto_buy",
				fmt.Sprintf("突破自动代购【%s】，消耗 %d 积分", cultivationPointDescriptionName(req.PillName), req.PointsCost),
				"breakthrough",
				fmt.Sprintf("major:%d", cul.MajorRealm),
			); err != nil {
				if errors.Is(err, storage.ErrPointsNotEnough) {
					return storage.ErrInsufficientPoints
				}
				return err
			}

			resourceCostPoints = req.PointsCost

		} else if mode == "USE_INVENTORY" && req.PointsCost > 0 {
			res := tx.Model(&model.Inventory{}).
				Where("user_id = ? AND item_name = ? AND quantity > 0", userID, req.PillName).
				UpdateColumn("quantity", gorm.Expr("quantity - 1"))

			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return storage.ErrNoBreakthroughPill
			}

			pillConsumed = true

			// 至宝突破：除消耗至宝外，每次尝试还需当场缴纳积分渡劫费（失败不退，成功另按 RefundRate 返还）。
			if IsCultivationTreasureItemName(req.PillName) {
				if err := storage.ApplyPointDeltaInTx(
					tx,
					userID,
					-req.PointsCost,
					"breakthrough_treasure_fee",
					fmt.Sprintf("突破献祭【%s】，缴纳渡劫费 %d 积分", cultivationPointDescriptionName(req.PillName), req.PointsCost),
					"breakthrough",
					fmt.Sprintf("major:%d", cul.MajorRealm),
				); err != nil {
					if errors.Is(err, storage.ErrPointsNotEnough) {
						return storage.ErrInsufficientPoints
					}
					return err
				}

				resourceCostPoints = req.PointsCost
			}

		} else if mode != "USE_INVENTORY" {
			return storage.ErrInvalidBreakthroughMode
		}

		// 2. 生成随机结果。随机结果在事务内落库，避免扣资源后无结果。
		isGuaranteed = req.GuaranteeFailCount > 0 && cul.TribulationFails >= req.GuaranteeFailCount
		finalRate := req.SuccessRate
		if isGuaranteed {
			finalRate = 1.0
		}
		successRate = finalRate

		rolled, err := rollDice()
		if err != nil || rolled < 1 || rolled > 100 {
			return storage.ErrRandomFailed
		}
		roll = rolled

		isSuccess = isGuaranteed || roll <= int(math.Round(finalRate*100))

		if isSuccess {
			newMajor = req.ToMajorRealm
			newMinor = 1
			nextConsolidate := now.Add(req.Cooldown)

			// 3A. 成功：更新境界。
			res := tx.Model(&model.Cultivation{}).
				Where("user_id = ? AND major_realm = ? AND minor_realm = ?", userID, cul.MajorRealm, cul.MinorRealm).
				Updates(map[string]interface{}{
					"major_realm":       newMajor,
					"minor_realm":       newMinor,
					"tribulation_fails": 0,
					"consolidate_until": nextConsolidate,
				})

			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return storage.ErrCultivationStateChanged
			}

			// 成功返还 20% 代购成本。使用背包丹药时，按丹药标价返还。
			refundPoints = int(float64(req.PointsCost) * req.RefundRate)
			if refundPoints > 0 {
				if err := storage.ApplyPointDeltaInTx(
					tx,
					userID,
					refundPoints,
					"breakthrough_refund",
					fmt.Sprintf("突破成功，天道返还 %d 积分", refundPoints),
					"breakthrough",
					fmt.Sprintf("major:%d->%d", cul.MajorRealm, newMajor),
				); err != nil {
					return err
				}
			}

			attempt := model.BreakthroughAttempt{
				UserID:             userID,
				FromMajor:          cul.MajorRealm,
				FromMinor:          cul.MinorRealm,
				ToMajor:            newMajor,
				ToMinor:            newMinor,
				Mode:               mode,
				PillName:           req.PillName,
				PointsCost:         req.PointsCost,
				PillConsumed:       pillConsumed,
				SuccessRate:        successRate,
				BaseSuccessRate:    baseSuccessRate,
				GuardCount:         guardCount,
				GuardBonusPercent:  int(math.Round((req.SuccessRate - baseSuccessRate) * 100)),
				Roll:               roll,
				IsGuaranteed:       isGuaranteed,
				Result:             "success",
				Status:             "settled",
				ResourceCostPoints: resourceCostPoints,
				FailPenaltyPoints:  0,
				RefundPoints:       refundPoints,
				Detail:             "突破成功",
			}

			if err := createBreakthroughAttemptInTx(tx, &attempt); err != nil {
				return err
			}

			if err := settleBreakthroughRitualInTx(tx, ritual, attempt.ID, true); err != nil {
				return err
			}
			attemptID = attempt.ID
			if err := awardSectContributionTx(
				tx,
				userID,
				20,
				fmt.Sprintf("大境界突破成功：%d -> %d", newMajor-1, newMajor),
				"breakthrough_success",
				fmt.Sprintf("%d", attemptID),
			); err != nil {
				return err
			}
			return nil
		}

		// 3B. 失败：增加失败次数。
		tribulationFails = cul.TribulationFails + 1

		nextConsolidate := now.Add(req.Cooldown)
		res := tx.Model(&model.Cultivation{}).
			Where("user_id = ? AND major_realm = ? AND minor_realm = ?", userID, cul.MajorRealm, cul.MinorRealm).
			Updates(map[string]interface{}{
				"tribulation_fails": gorm.Expr("tribulation_fails + ?", 1),
				"consolidate_until": nextConsolidate,
			})

		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrCultivationStateChanged
		}

		// 失败调养费：最多扣 50，余额不足则扣剩余全部，文案按实际扣款展示。
		var txUser model.User
		if err := tx.Select("telegram_id", "username", "points").
			Where("telegram_id = ?", userID).
			First(&txUser).Error; err != nil {
			return err
		}

		failPenaltyPoints = req.FailPenaltyPoints
		if txUser.Points < failPenaltyPoints {
			failPenaltyPoints = txUser.Points
		}
		if failPenaltyPoints < 0 {
			failPenaltyPoints = 0
		}

		if failPenaltyPoints > 0 {
			if err := storage.ApplyPointDeltaInTx(
				tx,
				userID,
				-failPenaltyPoints,
				"breakthrough_fail_penalty",
				fmt.Sprintf("突破失败，扣除调养费 %d 积分", failPenaltyPoints),
				"breakthrough",
				fmt.Sprintf("major:%d", cul.MajorRealm),
			); err != nil {
				return err
			}
		}

		attempt := model.BreakthroughAttempt{
			UserID:             userID,
			FromMajor:          cul.MajorRealm,
			FromMinor:          cul.MinorRealm,
			ToMajor:            cul.MajorRealm,
			ToMinor:            cul.MinorRealm,
			Mode:               mode,
			PillName:           req.PillName,
			PointsCost:         req.PointsCost,
			PillConsumed:       pillConsumed,
			SuccessRate:        successRate,
			BaseSuccessRate:    baseSuccessRate,
			GuardCount:         guardCount,
			GuardBonusPercent:  int(math.Round((req.SuccessRate - baseSuccessRate) * 100)),
			Roll:               roll,
			IsGuaranteed:       isGuaranteed,
			Result:             "failed",
			Status:             "settled",
			ResourceCostPoints: resourceCostPoints,
			FailPenaltyPoints:  failPenaltyPoints,
			RefundPoints:       0,
			Detail:             fmt.Sprintf("突破失败，失败次数变为 %d", tribulationFails),
		}

		if err := createBreakthroughAttemptInTx(tx, &attempt); err != nil {
			return err
		}

		if err := settleBreakthroughRitualInTx(tx, ritual, attempt.ID, false); err != nil {
			return err
		}
		attemptID = attempt.ID
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &BreakthroughOutcome{NewMajor: newMajor, NewMinor: newMinor, PillName: pillName, Roll: roll, SuccessRate: successRate, BaseSuccessRate: baseSuccessRate, IsGuaranteed: isGuaranteed, IsSuccess: isSuccess, RefundPoints: refundPoints, FailPenaltyPoints: failPenaltyPoints, TribulationFails: tribulationFails, GuaranteeFailCount: guaranteeFailCount, GuardCount: guardCount, AttemptID: attemptID}, nil
}

// ExecuteBreakthrough settles immediately when no recruitment is needed.
func ExecuteBreakthrough(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, mode string) {
	executeBreakthrough(bot, msg, mode, 0)
}

func executeBreakthrough(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, mode string, ritualID uint) {
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return
	}
	userID, chatID := msg.From.ID, msg.Chat.ID
	userName := msg.From.UserName
	if userName == "" {
		userName = msg.From.FirstName
	}
	safeName := textfmt.EscapeMarkdown(userName)
	result, err := ResolveBreakthrough(userID, mode, ritualID, time.Now())
	if errors.Is(err, ErrBreakthroughRitualClosed) || errors.Is(err, ErrBreakthroughRitualNotDue) {
		return
	}
	if err != nil && ritualID != 0 {
		if isBreakthroughCancellationError(err) {
			if cancelErr := CancelBreakthroughRitual(ritualID, userID); cancelErr != nil {
				log.Printf("取消互道突破失败: ritual=%d err=%s", ritualID, textfmt.FormatPlainError(cancelErr))
				return
			}
			telegram.ReplyTextToMessage(bot, msg, "⚠️ 突破条件已变化，本次突破取消；互道投入已原额退还。请重新发起突破。")
			refreshBreakthroughRitualPanel(bot, ritualID, nil)
			return
		}
		log.Printf("互道突破结算暂时失败，将重试: ritual=%d err=%s", ritualID, textfmt.FormatPlainError(err))
		return
	}

	if errors.Is(err, ErrBreakthroughRitualPending) {
		telegram.ReplyTextToMessage(bot, msg, "⚠️ 您已有待结算的互道突破，请等待本局结束。")
		return
	}
	if err != nil {
		switch cultivationErrorCode(err) {
		case "CULTIVATION_NOT_FOUND":
			telegram.ReplyTextToMessage(bot, msg, "❌ 未找到您的斗气档案，请先发送 `听书报告` 或 `我的信息` 初始化档案。")
		case "MAX_REALM_REACHED":
			telegram.ReplyTextToMessage(bot, msg, "⚡️ 您已达到本界天道天花板，无法继续突破。")
		case "CONSOLIDATING":
			telegram.ReplyTextToMessage(bot, msg, "⚠️ 您仍处于境界巩固期，暂时无法再次渡劫。")
		case "NOT_READY":
			telegram.ReplyTextToMessage(bot, msg, "❌ 您尚未达到当前境界大圆满，无法引动雷劫。")
		case "INSUFFICIENT_CULTIVATION":
			telegram.ReplyTextToMessage(bot, msg, "❌ 当前总修为不足，无法突破。")
		case "INSUFFICIENT_POINTS":
			telegram.ReplyTextToMessage(bot, msg, "❌ 积分不足，无法完成突破消耗（材料代购或至宝渡劫费）。")
		case "NO_PILL":
			telegram.ReplyTextToMessage(bot, msg, "❌ 乾坤袋内突破材料不足，无法渡劫。")
		case "INVALID_BREAKTHROUGH_MODE":
			telegram.ReplyTextToMessage(bot, msg, "❌ 突破模式异常，请重新发送 `突破` 指令。")
		case "CULTIVATION_STATE_CHANGED":
			telegram.ReplyTextToMessage(bot, msg, "⚠️ 您的境界状态刚刚发生变化，请重新发送 `突破` 指令确认。")
		case "RANDOM_FAILED":
			telegram.ReplyTextToMessage(bot, msg, "❌ 天机骰盅异常，突破未执行，资源未扣除，请稍后重试。")
		default:
			log.Printf("❌ 突破事务失败: user=%d mode=%s err=%s", userID, textfmt.FormatPlainValue(mode), textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, msg, "❌ 渡劫执行失败，本次资源未扣除，请稍后重试。")
		}
		return
	}

	newMajor, newMinor, pillName := result.NewMajor, result.NewMinor, result.PillName
	roll, successRate, isGuaranteed, isSuccess := result.Roll, result.SuccessRate, result.IsGuaranteed, result.IsSuccess
	refundPoints, failPenaltyPoints := result.RefundPoints, result.FailPenaltyPoints
	tribulationFails, guaranteeFailCount, attemptID := result.TribulationFails, result.GuaranteeFailCount, result.AttemptID
	if ritualID != 0 {
		finishBreakthroughRitualPanel(bot, ritualID, result)
	}

	// 事务提交后再发 Telegram 消息、通知大群、注入天道奖池。
	// 这些都是外部动作，不能放进数据库事务内。
	if isSuccess {
		currentPool := 0
		isBurst := false
		poolInjected := true

		if refundPoints > 0 {
			var err error
			currentPool, isBurst, err = storage.AddPointsToFusionPoolWithError(refundPoints)
			if err != nil {
				poolInjected = false
				log.Printf("⚠️ 突破成功后天道奖池注入失败: user=%d attempt=%d points=%d err=%s", userID, attemptID, refundPoints, textfmt.FormatPlainError(err))
			}
		}

		progressText := ""
		if refundPoints > 0 && !poolInjected {
			progressText = "\n\n*(⚠️ 天道奖池注入暂未完成，系统已记录异常，请联系管理员核查。)*"
		} else if refundPoints > 0 && !isBurst {
			progressText = fmt.Sprintf(
				"\n\n*(📈 此时天道灵气池已汇聚 `%d/300` 积分，蓄满将降下全服大红包！)*",
				currentPool,
			)
		}

		newRealmName := GetRealmName(&model.Cultivation{
			MajorRealm: newMajor,
			MinorRealm: newMinor,
		})
		newRealmName = strings.TrimPrefix(strings.TrimSuffix(newRealmName, "】"), "【")

		breakItemDesc := fmt.Sprintf("吞服【%s】", pillName)
		if IsCultivationTreasureItemName(pillName) {
			breakItemDesc = fmt.Sprintf("献祭至宝【%s】", pillName)
		}

		announce := fmt.Sprintf(
			"⚡️💥 **【诸天异象·白日飞升】** 💥⚡️\n\n"+
				"恭喜道友 @%s %s，成功历经九重天劫！\n"+
				"✨ 恭迎阁下晋升至全新的 **【%s】** 境界！\n\n"+
				"🎲 天机判定：成功率 `%.0f%%`，本次掷点 `%d/100`，保底：`%t`\n"+
				"🧾 突破记录：`#%d`\n"+
				"💰 **天道恩赐**：晋升成功，返还本人 `%d` 积分！%s",
			safeName,
			breakItemDesc,
			newRealmName,
			successRate*100,
			roll,
			isGuaranteed,
			attemptID,
			refundPoints,
			progressText,
		)

		if result.GuardCount > 0 {
			announce += fmt.Sprintf("\n🤝 互道 %d 人：基础 %.0f%% → 最终 %.0f%%；互道者各到账10积分（净赚5分）。", result.GuardCount, result.BaseSuccessRate*100, result.SuccessRate*100)
		}
		func() {
			msg := tgbotapi.NewMessage(chatID, announce)
			if chatID < 0 {
				if !telegram.EnqueueAutoDelete(bot, msg, "cultivation_breakthrough_success", telegram.TelegramAsyncPriorityNormal, fmt.Sprintf("cultivation_success:%d", attemptID)) {
					log.Printf("发送斗气突破成功公告入队失败: chat=%d user=%d attempt=%d", chatID, userID, attemptID)
				}
				return
			}
			if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
				log.Printf("发送斗气突破成功公告失败: chat=%d user=%d attempt=%d err=%s", chatID, userID, attemptID, textfmt.FormatTelegramSendError(err))
			}
		}()

		if isBurst && config.AppConfig.NoticeGroupID != 0 {
			poolAnnounce := fmt.Sprintf(
				"%s\n\n"+
					"🌈 **【天降甘霖·仙气化雨】** 🌈\n"+
					"因道友突破引动天地异象，天道奖池已蓄满并自动爆开！\n\n"+
					"💰 降下红包: `300` 积分\n"+
					"📦 福泽份数: `30` 份\n\n"+
					"👇 众修士快回复关键字 【`沾仙气`】 汲取天地造化！",
				announce,
			)
			telegram.SendGroupAutoDeleteMessageAsync(bot, config.AppConfig.NoticeGroupID, poolAnnounce, "cultivation_pool_burst_notice", fmt.Sprintf("cultivation_pool_burst:%d", attemptID))
		} else {
			if config.AppConfig.NoticeGroupID != 0 && chatID != config.AppConfig.NoticeGroupID {
				telegram.SendGroupAutoDeleteMessageAsync(bot, config.AppConfig.NoticeGroupID, announce, "cultivation_breakthrough_notice", fmt.Sprintf("cultivation_notice:%d", attemptID))
			}
		}

		return
	}

	failText := fmt.Sprintf(
		"⚡️💔 **【渡劫失败·道行受损】** 💔⚡️\n\n"+
			"道友 @%s 在突破时遭遇九幽魔雷轰击，**【%s】在狂暴的天雷中化为飞灰！**\n"+
			"🎲 天机判定：成功率 `%.0f%%`，本次掷点 `%d/100`，保底：`%t`\n"+
			"🧾 突破记录：`#%d`\n"+
			"并且身受重伤，实际扣除 `%d` 积分调养费。\n\n"+
			"💡 天道感应：当前境界已连续失败 `%d/%d` 次，满%d次下次必定破阶成功！",
		safeName,
		pillName,
		successRate*100,
		roll,
		isGuaranteed,
		attemptID,
		failPenaltyPoints,
		tribulationFails,
		guaranteeFailCount,
		guaranteeFailCount,
	)

	if result.GuardCount > 0 {
		failText += fmt.Sprintf("\n🤝 互道 %d 人：基础 %.0f%% → 最终 %.0f%%；仅互道者损失投入的5积分，未参与者不受影响。", result.GuardCount, result.BaseSuccessRate*100, result.SuccessRate*100)
	}
	if config.AppConfig != nil && config.AppConfig.NoticeGroupID != 0 && chatID != config.AppConfig.NoticeGroupID {
		telegram.SendGroupAutoDeleteMessageAsync(bot, config.AppConfig.NoticeGroupID, failText, "cultivation_breakthrough_fail_notice", fmt.Sprintf("cultivation_fail_notice:%d", attemptID))
	}
	func() {
		msg := tgbotapi.NewMessage(chatID, failText)
		if chatID < 0 {
			if !telegram.EnqueueAutoDelete(bot, msg, "cultivation_breakthrough_fail", telegram.TelegramAsyncPriorityNormal, fmt.Sprintf("cultivation_fail:%d", attemptID)) {
				log.Printf("发送斗气突破失败公告入队失败: chat=%d user=%d attempt=%d", chatID, userID, attemptID)
			}
			return
		}
		if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
			log.Printf("发送斗气突破失败公告失败: chat=%d user=%d attempt=%d err=%s", chatID, userID, attemptID, textfmt.FormatTelegramSendError(err))
		}
	}()
}

func HandleCultivationRank(bot *tgbotapi.BotAPI, chatID int64) {
	type RankResult struct {
		Username       string
		SectName       string
		MajorRealm     int
		MinorRealm     int
		TotalAudioTime float64
		PillAudioTime  float64
	}

	var results []RankResult

	err := storage.DB.Table("cultivations").
		Select("users.username, sects.name as sect_name, cultivations.major_realm, cultivations.minor_realm, cultivations.total_audio_time, cultivations.pill_audio_time").
		Joins("left join users on users.telegram_id = cultivations.user_id").
		Joins("left join sect_members on sect_members.user_id = cultivations.user_id and sect_members.deleted_at is null").
		Joins("left join sects on sects.id = sect_members.sect_id and sects.deleted_at is null").
		Order("cultivations.major_realm desc, cultivations.minor_realm desc, (cultivations.total_audio_time + cultivations.pill_audio_time) desc, cultivations.total_audio_time desc").
		Limit(10).
		Scan(&results).Error

	if err != nil {
		if _, sendErr := telegram.SendAutoDelete(bot, tgbotapi.NewMessage(chatID, "❌ 天机阁拉取榜单异常，请联系管理员查看后台日志！")); sendErr != nil {
			log.Printf("发送斗气榜单错误提示失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(sendErr))
		}
		log.Printf("⚠️ 斗气榜查询失败: chat=%d err=%s", chatID, textfmt.FormatPlainError(err))
		return
	}

	if len(results) == 0 {
		telegram.SendGroupAutoDeleteMessage(bot, chatID, "📿 **【斗气风云榜】**\n\n尚无听友踏上斗气之途，快来听书积累修为吧。")
		return
	}

	reply := "📜 **【斗气风云榜·万界至尊】** 📜\n\n"
	medals := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}

	for i, r := range results {
		dummyCul := &model.Cultivation{MajorRealm: r.MajorRealm, MinorRealm: r.MinorRealm}
		realmName := GetRealmName(dummyCul)

		displayName := cultivationRankDisplayName(r.Username, r.SectName)

		totalShow := r.TotalAudioTime + r.PillAudioTime

		reply += fmt.Sprintf("%s **第%d名**: **%s**\n └ %s | 总修为 `%.1f` 小时\n   *(苦修: `%.1f` 小时 | 药力: `%.1f` 小时)*\n\n",
			medals[i], i+1, displayName, realmName, totalShow, r.TotalAudioTime, r.PillAudioTime)
	}

	reply += "*(💡 发送 `突破` 引动雷劫，提升境界排位！)*"
	msg := tgbotapi.NewMessage(chatID, reply)
	msg.ParseMode = "Markdown"
	if _, sendErr := telegram.SendAutoDelete(bot, msg); sendErr != nil {
		log.Printf("发送斗气榜单失败: chat=%d err=%s", chatID, textfmt.FormatTelegramSendError(sendErr))
	}
}

func cultivationRankDisplayName(username string, sectName string) string {
	name := strings.TrimSpace(username)
	if name == "" {
		name = "神秘道友"
	}
	name = textfmt.EscapeMarkdown(name)

	sectName = strings.TrimSpace(sectName)
	if sectName == "" {
		return name
	}
	return fmt.Sprintf("%s【%s】", name, textfmt.EscapeMarkdown(sectName))
}

func cultivationPointDescriptionName(name string) string {
	return textfmt.LotteryDisplayText(name, 80, "-")
}

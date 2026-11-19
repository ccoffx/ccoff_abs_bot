package game

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

type gardenLevelConfig struct {
	Experience      int64
	GrowthReduction int
	OverflowChance  int
}

var gardenLevels = [...]gardenLevelConfig{
	{0, 0, 0}, {48, 2, 8}, {192, 4, 16}, {480, 6, 24}, {960, 8, 32},
	{1800, 10, 40}, {3000, 12, 50}, {4800, 15, 60}, {7600, 18, 70}, {12000, 20, 80},
}

func gardenLevel(experience int64) int {
	level := 1
	for i, cfg := range gardenLevels {
		if experience < cfg.Experience {
			break
		}
		level = i + 1
	}
	return level
}

func gardenGrowDuration(base time.Duration, reduction int) time.Duration {
	return base * time.Duration(100-reduction) / 100
}

func gardenOverflowReward(cfg gardenSeedConfig) int {
	if cfg.Purchasable {
		return cfg.Price / 5
	}
	switch cfg.Key {
	case "longxue":
		return 16
	case "chixinlian":
		return 24
	default:
		return 0
	}
}

// Missing progress means a new garden. Other read failures must remain errors.
func gardenExperience(db *gorm.DB, userID int64) (int64, error) {
	var progress model.GardenProgress
	err := db.Where("user_id = ?", userID).Take(&progress).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return progress.Experience, err
}

func gardenProgressText(userID int64) string {
	experience, err := gardenExperience(storage.DB, userID)
	if err != nil {
		log.Printf("⚠️ 读取药园等级失败: user=%d err=%s", userID, textfmt.FormatPlainError(err))
		return "药园等级：读取失败\n"
	}
	level := gardenLevel(experience)
	cfg := gardenLevels[level-1]
	progress := fmt.Sprintf("累计经验 `%d`（已满级）", experience)
	if level < len(gardenLevels) {
		next := gardenLevels[level].Experience
		progress = fmt.Sprintf("累计经验 `%d/%d`，距升级还需 `%d`", experience, next, next-experience)
	}
	return fmt.Sprintf("药园等级：**Lv.%d**\n%s\n种植减时：`%d%%`｜灵气溢出：`%d%%`\n", level, progress, cfg.GrowthReduction, cfg.OverflowChance)
}

func gardenDurationPreview(cfg gardenSeedConfig, experience int64, err error) string {
	base := gardenDurationText(cfg.GrowDuration)
	if err != nil {
		return fmt.Sprintf("基础 %s，当前读取失败", base)
	}
	level := gardenLevels[gardenLevel(experience)-1]
	return fmt.Sprintf("基础 %s → 当前 %s", base, gardenDurationText(gardenGrowDuration(cfg.GrowDuration, level.GrowthReduction)))
}

func renderGardenLevels(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	var b strings.Builder
	b.WriteString("🌱 **【药园等级说明】**\n\n")
	b.WriteString(gardenProgressText(userID))
	b.WriteString("\n等级｜累计经验｜减时｜溢出概率\n")
	for i, cfg := range gardenLevels {
		fmt.Fprintf(&b, "Lv.%d｜%d｜%d%%｜%d%%\n", i+1, cfg.Experience, cfg.GrowthReduction, cfg.OverflowChance)
	}
	b.WriteString("\n成熟收获时，基础生长每小时获得 1 点经验；不受减时或产量影响。等级永久保留，满级后继续累计经验。\n\n播种时锁定本轮加成，升级从下次播种生效。每块田收获独立判定一次灵气溢出，与药草株数无关，积分直接入账，无额外每日上限。\n\n触发奖励：回气草 3、青灵叶 5、赤阳花 8、月影藤 12、玄岩参 18、紫晶芝 28、龙血果 16、炽心莲 24 积分。")
	return b.String(), gardenBackHomeMarkup()
}

type gardenHarvestItem struct {
	HerbName string
	Quantity int
}

type gardenHarvestResult struct {
	Plots        int
	Quantity     int
	Items        []gardenHarvestItem
	Experience   int
	SpiritPoints int
	LevelBefore  int
	LevelAfter   int
}

func (r gardenHarvestResult) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ 收获 %d 块田，共 %d 株药草\n", r.Plots, r.Quantity)
	for _, item := range r.Items {
		fmt.Fprintf(&b, "%s ×%d\n", textfmt.InventoryItemMarkdownName(item.HerbName), item.Quantity)
	}
	fmt.Fprintf(&b, "经验 +%d｜灵气溢出 +%d 积分", r.Experience, r.SpiritPoints)
	if r.LevelAfter > r.LevelBefore {
		cfg := gardenLevels[r.LevelAfter-1]
		fmt.Fprintf(&b, "\n🎉 药园升至 **Lv.%d**！下次播种生效：减时 %d%%，溢出概率 %d%%。", r.LevelAfter, cfg.GrowthReduction, cfg.OverflowChance)
	}
	return b.String()
}

func gardenRollOverflow(chance int) (bool, error) {
	if chance <= 0 {
		return false, nil
	}
	roll, err := security.RandomIntRange(1, 100)
	return err == nil && roll <= chance, err
}

func gardenHarvestPlot(userID int64, plotNo int) (gardenHarvestResult, error) {
	return gardenHarvest(userID, plotNo, false)
}

func gardenHarvestAll(userID int64) (gardenHarvestResult, error) {
	return gardenHarvest(userID, 0, true)
}

func gardenHarvest(userID int64, plotNo int, all bool) (gardenHarvestResult, error) {
	var result gardenHarvestResult
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var plantings []model.GardenPlanting
		query := tx.Where("user_id = ? AND status = ?", userID, gardenStatusGrowing)
		if all {
			query = query.Where("matures_at <= ?", now)
		} else {
			query = query.Where("plot_no = ?", plotNo)
		}
		if err := query.Order("plot_no asc").Find(&plantings).Error; err != nil {
			return err
		}
		if len(plantings) == 0 {
			if all {
				return storage.ErrGardenNoMaturePlant
			}
			return storage.ErrGardenNoActivePlant
		}
		if !all && plantings[0].MaturesAt.After(now) {
			return storage.ErrGardenNotMature
		}
		// Acquire the write transaction before reading and incrementing progress.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.GardenProgress{UserID: userID}).Error; err != nil {
			return err
		}
		experience, err := gardenExperience(tx, userID)
		if err != nil {
			return err
		}
		result.LevelBefore = gardenLevel(experience)
		for _, planting := range plantings {
			cfg, ok := GardenSeedByKey(planting.SeedKey)
			if !ok {
				return storage.ErrGardenSeedUnknown
			}
			qty, err := security.RandomIntRange(cfg.YieldMin, cfg.YieldMax)
			if err != nil {
				return err
			}
			hit, err := gardenRollOverflow(planting.OverflowChance)
			if err != nil {
				return err
			}
			points := 0
			if hit {
				points = planting.OverflowReward
			}
			res := tx.Model(&model.GardenPlanting{}).
				Where("id = ? AND status = ?", planting.ID, gardenStatusGrowing).
				Updates(map[string]any{"status": gardenStatusHarvest, "harvested_at": now, "quantity": qty, "overflow_points": points})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return storage.ErrGardenAlreadyHarvested
			}
			if err := GardenGrantInventoryInTx(tx, userID, cfg.HerbName, qty); err != nil {
				return err
			}
			if points > 0 {
				err := storage.ApplyPointDeltaInTx(tx, userID, points, "garden_spirit_overflow",
					fmt.Sprintf("收获【%s】灵气溢出，获得 %d 积分", gardenPointDescriptionName(cfg.HerbName), points), "garden_planting", strconv.FormatUint(uint64(planting.ID), 10))
				if err != nil {
					return err
				}
			}
			result.Plots++
			result.Quantity += qty
			result.Experience += planting.Experience
			result.SpiritPoints += points
			found := false
			for i := range result.Items {
				if result.Items[i].HerbName == cfg.HerbName {
					result.Items[i].Quantity += qty
					found = true
					break
				}
			}
			if !found {
				result.Items = append(result.Items, gardenHarvestItem{cfg.HerbName, qty})
			}
		}
		res := tx.Model(&model.GardenProgress{}).Where("user_id = ?", userID).
			UpdateColumn("experience", gorm.Expr("experience + ?", result.Experience))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("GARDEN_PROGRESS_UPDATE_MISSED")
		}
		result.LevelAfter = gardenLevel(experience + int64(result.Experience))
		return nil
	})
	if err != nil {
		return gardenHarvestResult{}, err
	}
	return result, nil
}

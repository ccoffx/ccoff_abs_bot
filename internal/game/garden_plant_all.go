package game

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

func newGardenPlanting(userID int64, plot model.GardenPlot, cfg gardenSeedConfig, level gardenLevelConfig, now time.Time) model.GardenPlanting {
	return model.GardenPlanting{
		UserID: userID, PlotID: plot.ID, PlotNo: plot.PlotNo,
		SeedKey: cfg.Key, SeedName: cfg.SeedName, HerbName: cfg.HerbName,
		PlantedAt: now, MaturesAt: now.Add(gardenGrowDuration(cfg.GrowDuration, level.GrowthReduction)),
		Experience: int(cfg.GrowDuration / time.Hour), GrowthReduction: level.GrowthReduction,
		OverflowChance: level.OverflowChance, OverflowReward: gardenOverflowReward(cfg), Status: gardenStatusGrowing,
	}
}

// Empty plots exclude all unharvested crops, including mature ones.
func gardenEmptyPlots(tx *gorm.DB, userID int64) ([]model.GardenPlot, error) {
	occupied := tx.Model(&model.GardenPlanting{}).Select("plot_id").Where("user_id = ? AND status = ?", userID, gardenStatusGrowing)
	var plots []model.GardenPlot
	err := tx.Where("user_id = ? AND id NOT IN (?)", userID, occupied).Order("plot_no asc").Find(&plots).Error
	return plots, err
}

// Retry the entire planting transaction with a fresh SQLite snapshot. Retrying
// just the write cannot recover a deferred transaction's stale read snapshot.
// The callback must only change database state and reset any captured result.
func gardenPlantTransaction(fn func(*gorm.DB) error) error {
	const attempts = 8
	for attempt := 0; ; attempt++ {
		err := storage.DB.Transaction(fn)
		var sqliteErr sqlite3.Error
		if err == nil || attempt == attempts-1 || !errors.As(err, &sqliteErr) ||
			(sqliteErr.Code != sqlite3.ErrBusy && sqliteErr.Code != sqlite3.ErrLocked) {
			return err
		}
		time.Sleep(5 * time.Millisecond * time.Duration(1<<min(attempt, 4)))
	}
}

type gardenPlantAllResult struct {
	HerbName  string
	PlotNos   []int
	PlantedAt time.Time
	MaturesAt time.Time
}

func (r gardenPlantAllResult) text() string {
	nos := make([]string, len(r.PlotNos))
	for i, no := range r.PlotNos {
		nos[i] = strconv.Itoa(no)
	}
	return fmt.Sprintf("🌱 已种植 %s ×%d，占用 %s 号田，预计 %s 后成熟。", textfmt.InventoryItemMarkdownName(r.HerbName), len(r.PlotNos), strings.Join(nos, "、"), gardenDurationText(r.MaturesAt.Sub(r.PlantedAt)))
}

func gardenPlantAll(userID int64, seedKey string) (gardenPlantAllResult, error) {
	var result gardenPlantAllResult
	cfg, ok := GardenSeedByKey(seedKey)
	if !ok {
		return result, storage.ErrGardenSeedUnknown
	}
	err := gardenPlantTransaction(func(tx *gorm.DB) error {
		result = gardenPlantAllResult{}
		plots, err := gardenEmptyPlots(tx, userID)
		if err != nil {
			return err
		}
		if len(plots) == 0 {
			return storage.ErrGardenNoEmptyPlot
		}
		var inventory model.Inventory
		if err := tx.Where("user_id = ? AND item_name = ?", userID, cfg.SeedName).Take(&inventory).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return storage.ErrGardenSeedNotEnough
			}
			return err
		}
		qty := min(len(plots), inventory.Quantity)
		if qty <= 0 {
			return storage.ErrGardenSeedNotEnough
		}
		res := tx.Model(&model.Inventory{}).Where("id = ? AND quantity >= ?", inventory.ID, qty).
			UpdateColumn("quantity", gorm.Expr("quantity - ?", qty))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return storage.ErrGardenSeedNotEnough
		}
		experience, err := gardenExperience(tx, userID)
		if err != nil {
			return err
		}
		level := gardenLevels[gardenLevel(experience)-1]
		now := time.Now()
		for _, plot := range plots[:qty] {
			planting := newGardenPlanting(userID, plot, cfg, level, now)
			if err := createGardenPlantingInTx(tx, &planting); err != nil {
				return err
			}
			result.PlotNos = append(result.PlotNos, plot.PlotNo)
			result.MaturesAt = planting.MaturesAt
		}
		result.HerbName, result.PlantedAt = cfg.HerbName, now
		return nil
	})
	if err != nil {
		return gardenPlantAllResult{}, err
	}
	return result, nil
}

func renderGardenPlantAllList(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	var b strings.Builder
	b.WriteString("🌱 **【一键种植】**\n\n")
	rows := make([][]tgbotapi.InlineKeyboardButton, 0)
	plots, plotErr := gardenEmptyPlots(storage.DB, userID)
	inv, invErr := gardenInventoryByNamesWithError(userID, gardenSeedItemNames())
	experience, progressErr := gardenExperience(storage.DB, userID)
	switch {
	case plotErr != nil || invErr != nil || progressErr != nil:
		log.Printf("⚠️ 读取批量种植信息失败: user=%d err=%s", userID, textfmt.FormatPlainError(errors.Join(plotErr, invErr, progressErr)))
		b.WriteString("种植信息读取失败，请稍后刷新重试。\n")
	case len(plots) == 0:
		b.WriteString("暂无空闲灵田，成熟作物需先收获；请前往灵田管理查看。\n")
	default:
		fmt.Fprintf(&b, "空闲灵田：`%d` 块\n选择一种种子，按田号顺序尽量种满空田。实际以执行时库存和空田为准。\n\n", len(plots))
		for _, cfg := range GardenSeeds {
			qty := inv[cfg.SeedName]
			if qty <= 0 {
				continue
			}
			fmt.Fprintf(&b, "%s：`%d` 枚，成熟 `%s`，产量 `%s`\n", textfmt.InventoryItemMarkdownName(cfg.SeedName), qty, gardenDurationPreview(cfg, experience, nil), gardenYieldText(cfg))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("种%s ×%d", cfg.HerbName, min(qty, len(plots))), "garden:plantall:"+cfg.Key)))
		}
		if len(rows) == 0 {
			b.WriteString("乾坤袋中暂无种子，请先去种子商店购买。\n")
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("刷新", "garden:plantalllist"),
		tgbotapi.NewInlineKeyboardButtonData("种子商店", "garden:shop"),
		tgbotapi.NewInlineKeyboardButtonData("返回灵田", "garden:fields"),
	))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...)
}

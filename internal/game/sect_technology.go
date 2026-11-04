package game

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func createSectTechnologyInTx(tx *gorm.DB, technology *model.SectTechnology) error {
	if tx == nil || technology == nil {
		return fmt.Errorf("SECT_TECHNOLOGY_INVALID")
	}
	entry := *technology
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_TECHNOLOGY_CREATE_MISSED")
	}
	*technology = entry
	return nil
}

func createSectTechnologyLogInTx(tx *gorm.DB, logEntry *model.SectTechnologyLog) error {
	if tx == nil || logEntry == nil {
		return fmt.Errorf("SECT_TECHNOLOGY_LOG_INVALID")
	}
	entry := *logEntry
	entry.UserName = textfmt.FormatPlainValue(entry.UserName)
	res := tx.Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SECT_TECHNOLOGY_LOG_CREATE_MISSED")
	}
	return nil
}

const (
	SectTechBossDamageBonus = "boss_damage_bonus"
	sectTechDailyTaskBonus  = "daily_task_bonus"
	sectTechMemberLimit     = "member_limit_bonus"

	sectTechnologyMaxLevel = 5
)

type sectTechnologyDefinition struct {
	Key         string
	Name        string
	Description string
}

var sectTechnologyDefinitions = []sectTechnologyDefinition{
	{Key: SectTechBossDamageBonus, Name: "玄音共鸣", Description: "世界 Boss 伤害加成"},
	{Key: sectTechDailyTaskBonus, Name: "香火鼎盛", Description: "宗门任务奖励加成"},
	{Key: sectTechMemberLimit, Name: "聚贤纳士", Description: "成员上限加成"},
}

func getSectTechnologyUpgradeCost(newLevel int) (fundsCost int, prestigeCost int, ok bool) {
	costs := map[int][2]int{
		1: {80, 40},
		2: {240, 120},
		3: {540, 270},
		4: {960, 480},
		5: {1500, 750},
	}

	cost, exists := costs[newLevel]
	if !exists {
		return 0, 0, false
	}

	return cost[0], cost[1], true
}

func parseSectTechnologyKey(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)

	index, err := strconv.Atoi(raw)
	if err == nil {
		if index >= 1 && index <= len(sectTechnologyDefinitions) {
			return sectTechnologyDefinitions[index-1].Key, true
		}
		return "", false
	}

	for _, def := range sectTechnologyDefinitions {
		if raw == def.Name || raw == def.Key {
			return def.Key, true
		}
	}

	return "", false
}

func getSectTechnologyName(key string) string {
	for _, def := range sectTechnologyDefinitions {
		if def.Key == key {
			return def.Name
		}
	}
	return key
}

func getSectTechnologyLevelTxChecked(tx *gorm.DB, sectID int64, techKey string) (int, error) {
	if tx == nil {
		tx = storage.DB
	}
	if tx == nil {
		return 0, fmt.Errorf("SECT_TECHNOLOGY_DB_UNAVAILABLE")
	}

	var tech model.SectTechnology
	if err := tx.Where("sect_id = ? AND tech_key = ?", sectID, techKey).First(&tech).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}

	if tech.Level < 0 {
		return 0, nil
	}
	if tech.Level > sectTechnologyMaxLevel {
		return sectTechnologyMaxLevel, nil
	}
	return tech.Level, nil
}

func getSectTechnologyLevelByUserChecked(userID int64, techKey string) (int, error) {
	var member model.SectMember
	if err := storage.DB.Where("user_id = ?", userID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}

	return getSectTechnologyLevelTxChecked(storage.DB, member.SectID, techKey)
}

func getSectMaxMembersWithTechTx(tx *gorm.DB, sect model.Sect) int {
	maxMembers, err := getSectMaxMembersWithTechTxChecked(tx, sect)
	if err != nil {
		return getSectMaxMembers(sect.Level)
	}
	return maxMembers
}

func getSectMaxMembersWithTechTxChecked(tx *gorm.DB, sect model.Sect) (int, error) {
	base := getSectMaxMembers(sect.Level)
	bonusLevel, err := getSectTechnologyLevelTxChecked(tx, int64(sect.ID), sectTechMemberLimit)
	if err != nil {
		return 0, err
	}
	return base + bonusLevel*5, nil
}

func getSectMaxMembersWithTech(sect model.Sect) int {
	return getSectMaxMembersWithTechTx(storage.DB, sect)
}

func getSectDailyTaskRewardsTxChecked(tx *gorm.DB, sectID int64) (int, int, error) {
	level, err := getSectTechnologyLevelTxChecked(tx, sectID, sectTechDailyTaskBonus)
	if err != nil {
		return 0, 0, err
	}
	return sectDailyTaskRewardContribution + level, sectDailyTaskRewardPrestige + level, nil
}

func sectTechnologyUpgradeErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrNotInSect):
		return "NOT_IN_SECT"
	case errors.Is(err, storage.ErrSectOnlyOwner):
		return "ONLY_OWNER"
	case errors.Is(err, storage.ErrSectMaxLevel):
		return "MAX_LEVEL"
	case errors.Is(err, storage.ErrSectFundsNotEnough):
		return "FUNDS_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectPrestigeNotEnough):
		return "PRESTIGE_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectResourceNotEnough):
		return "RESOURCE_NOT_ENOUGH"
	case errors.Is(err, storage.ErrSectTechnologyLevelChanged):
		return "TECH_LEVEL_CHANGED"
	case err != nil:
		return storage.FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func formatSectTechnologyEffect(key string, level int, sect model.Sect) string {
	switch key {
	case SectTechBossDamageBonus:
		return fmt.Sprintf("世界 Boss 伤害 +%d%%", level*2)
	case sectTechDailyTaskBonus:
		return fmt.Sprintf("宗门每日任务奖励：贡献 +%d，声望 +%d", sectDailyTaskRewardContribution+level, sectDailyTaskRewardPrestige+level)
	case sectTechMemberLimit:
		return fmt.Sprintf("成员上限 +%d，当前上限 %s 人", level*5, sectMaxMembersDisplayText(sect, "sect_technology_effect", 0))
	default:
		return "未知效果"
	}
}

func handleSectTechnology(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	var member model.SectMember
	if err := storage.DB.Where("user_id = ?", msg.From.ID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 您当前没有加入宗门。")
		} else {
			log.Printf("⚠️ 宗门科技面板成员档案读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, msg.Chat.ID, "❌ 宗门成员档案读取失败，请稍后再试。")
		}
		return
	}

	var sect model.Sect
	if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
		log.Printf("⚠️ 宗门科技面板宗门档案读取失败: sect=%d user=%d err=%s", member.SectID, msg.From.ID, textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "❌ 宗门档案读取失败，请稍后再试。")
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("🏯 **%s · 宗门科技**\n\n", textfmt.EscapeMarkdown(sect.Name)))

	for i, def := range sectTechnologyDefinitions {
		level, levelErr := getSectTechnologyLevelTxChecked(storage.DB, int64(sect.ID), def.Key)
		if levelErr != nil {
			log.Printf("⚠️ 宗门科技面板等级读取失败: sect=%d tech=%s user=%d err=%s", sect.ID, textfmt.FormatPlainValue(def.Key), msg.From.ID, textfmt.FormatPlainError(levelErr))
			b.WriteString(fmt.Sprintf("%d. **%s** Lv.读取失败/%d\n", i+1, def.Name, sectTechnologyMaxLevel))
			b.WriteString("效果：读取失败\n\n")
			continue
		}
		b.WriteString(fmt.Sprintf("%d. **%s** Lv.%d/%d\n", i+1, def.Name, level, sectTechnologyMaxLevel))
		b.WriteString(fmt.Sprintf("效果：%s\n", formatSectTechnologyEffect(def.Key, level, sect)))

		if level >= sectTechnologyMaxLevel {
			b.WriteString("已达当前最高等级\n\n")
			continue
		}

		nextLevel := level + 1
		fundsCost, prestigeCost, _ := getSectTechnologyUpgradeCost(nextLevel)
		b.WriteString(fmt.Sprintf("下级：Lv.%d，消耗资金 `%d`，声望 `%d`\n\n", nextLevel, fundsCost, prestigeCost))
	}

	b.WriteString(fmt.Sprintf("宗门资产：资金 `%d`，声望 `%d`\n", sect.Funds, sect.Prestige))
	b.WriteString("升级指令：`升级科技 玄音共鸣` 或 `升级科技 1`")

	telegram.ReplyText(bot, msg.Chat.ID, b.String())
}

func handleUpgradeSectTechnology(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, rawTech string, confirmed bool) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	techKey, ok := parseSectTechnologyKey(rawTech)
	if !ok {
		telegram.ReplyText(bot, chatID, "❌ 科技名称或编号错误。可用：`玄音共鸣`、`香火鼎盛`、`聚贤纳士`，或编号 `1-3`。")
		return
	}

	if !confirmed {
		var member model.SectMember
		if err := storage.DB.Where("user_id = ?", userID).First(&member).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				telegram.ReplyText(bot, chatID, "❌ 您当前没有加入宗门。")
			} else {
				log.Printf("⚠️ 宗门科技升级确认成员档案读取失败: user=%d tech=%s err=%s", userID, textfmt.FormatPlainValue(techKey), textfmt.FormatPlainError(err))
				telegram.ReplyText(bot, chatID, "❌ 宗门成员档案读取失败，请稍后再试。")
			}
			return
		}
		if !canUpgradeSectAsset(member.Role) {
			telegram.ReplyText(bot, chatID, "❌ 只有宗主或长老可以升级宗门科技。")
			return
		}

		var sect model.Sect
		if err := storage.DB.Where("id = ?", member.SectID).First(&sect).Error; err != nil {
			log.Printf("⚠️ 宗门科技升级确认宗门档案读取失败: sect=%d user=%d tech=%s err=%s", member.SectID, userID, textfmt.FormatPlainValue(techKey), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "❌ 宗门档案读取失败，请稍后再试。")
			return
		}

		currentLevel, levelErr := getSectTechnologyLevelTxChecked(storage.DB, member.SectID, techKey)
		if levelErr != nil {
			log.Printf("⚠️ 宗门科技升级确认等级读取失败: sect=%d tech=%s user=%d err=%s", member.SectID, textfmt.FormatPlainValue(techKey), userID, textfmt.FormatPlainError(levelErr))
			telegram.ReplyText(bot, chatID, "❌ 宗门科技读取失败，请稍后重试。")
			return
		}
		if currentLevel >= sectTechnologyMaxLevel {
			telegram.ReplyText(bot, chatID, fmt.Sprintf("✅ 科技【%s】已达到最高等级。", getSectTechnologyName(techKey)))
			return
		}

		nextLevel := currentLevel + 1
		fundsCost, prestigeCost, _ := getSectTechnologyUpgradeCost(nextLevel)

		telegram.ReplyText(bot, chatID, fmt.Sprintf(
			"⚠️ **升级科技确认**\n\n科技：**%s**\n当前等级：Lv.%d\n升级后：Lv.%d\n消耗资金：`%d`\n消耗声望：`%d`\n\n确认升级请发送：\n`确认升级科技 %s`",
			getSectTechnologyName(techKey),
			currentLevel,
			nextLevel,
			fundsCost,
			prestigeCost,
			getSectTechnologyName(techKey),
		))
		return
	}

	var sectName string
	var oldLevel int
	var newLevel int
	var fundsCost int
	var prestigeCost int
	var effectText string

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var member model.SectMember
		if err := tx.Where("user_id = ?", userID).First(&member).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return storage.ErrNotInSect
		}

		if !canUpgradeSectAsset(member.Role) {
			return storage.ErrSectOnlyOwner
		}

		var sect model.Sect
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", member.SectID).
			First(&sect).Error; err != nil {
			return err
		}

		sectName = sect.Name
		var levelErr error
		oldLevel, levelErr = getSectTechnologyLevelTxChecked(tx, int64(sect.ID), techKey)
		if levelErr != nil {
			return levelErr
		}
		if oldLevel >= sectTechnologyMaxLevel {
			return storage.ErrSectMaxLevel
		}

		newLevel = oldLevel + 1
		var costOK bool
		fundsCost, prestigeCost, costOK = getSectTechnologyUpgradeCost(newLevel)
		if !costOK {
			return storage.ErrSectMaxLevel
		}

		if sect.Funds < fundsCost {
			return storage.ErrSectFundsNotEnough
		}
		if sect.Prestige < prestigeCost {
			return storage.ErrSectPrestigeNotEnough
		}

		res := tx.Model(&model.Sect{}).
			Where("id = ? AND funds >= ? AND prestige >= ?", sect.ID, fundsCost, prestigeCost).
			Updates(map[string]interface{}{
				"funds":    gorm.Expr("funds - ?", fundsCost),
				"prestige": gorm.Expr("prestige - ?", prestigeCost),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return storage.ErrSectResourceNotEnough
		}

		var tech model.SectTechnology
		err := tx.Where("sect_id = ? AND tech_key = ?", sect.ID, techKey).First(&tech).Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			tech = model.SectTechnology{
				SectID:  int64(sect.ID),
				TechKey: techKey,
				Level:   newLevel,
			}
			if err := createSectTechnologyInTx(tx, &tech); err != nil {
				return err
			}
		} else {
			res := tx.Model(&model.SectTechnology{}).
				Where("id = ? AND level = ?", tech.ID, oldLevel).
				Update("level", newLevel)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return storage.ErrSectTechnologyLevelChanged
			}
		}

		if err := createSectTechnologyLogInTx(tx, &model.SectTechnologyLog{
			SectID:       int64(sect.ID),
			UserID:       userID,
			UserName:     textfmt.GetTelegramDisplayName(msg.From),
			TechKey:      techKey,
			OldLevel:     oldLevel,
			NewLevel:     newLevel,
			FundsCost:    fundsCost,
			PrestigeCost: prestigeCost,
		}); err != nil {
			return err
		}

		sect.Funds -= fundsCost
		sect.Prestige -= prestigeCost
		effectText = formatSectTechnologyEffect(techKey, newLevel, sect)
		return nil
	})

	if err != nil {
		switch sectTechnologyUpgradeErrorCode(err) {
		case "NOT_IN_SECT":
			telegram.ReplyText(bot, chatID, "❌ 您当前没有加入宗门。")
		case "ONLY_OWNER":
			telegram.ReplyText(bot, chatID, "❌ 只有宗主或长老可以升级宗门科技。")
		case "MAX_LEVEL":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("✅ 科技【%s】已达到最高等级。", getSectTechnologyName(techKey)))
		case "FUNDS_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("❌ 宗门资金不足，升级需要 `%d` 资金。", fundsCost))
		case "TECH_LEVEL_CHANGED":
			telegram.ReplyText(bot, chatID, "⚠️ 科技等级刚刚发生变化，请重新发送 `宗门科技` 查看。")
		case "PRESTIGE_NOT_ENOUGH":
			telegram.ReplyText(bot, chatID, fmt.Sprintf("❌ 宗门声望不足，升级需要 `%d` 声望。", prestigeCost))
		default:
			log.Printf("⚠️ 宗门科技升级失败: user=%d tech=%s err=%s", userID, textfmt.FormatPlainValue(techKey), textfmt.FormatPlainError(err))
			telegram.ReplyText(bot, chatID, "❌ 科技升级失败，请稍后重试。")
		}
		return
	}

	telegram.ReplyText(bot, chatID, fmt.Sprintf(
		"✅ **宗门科技升级成功！**\n\n宗门：**%s**\n科技：**%s**\n等级：Lv.%d -> Lv.%d\n消耗资金：`%d`\n消耗声望：`%d`\n当前效果：%s",
		textfmt.EscapeMarkdown(sectName),
		getSectTechnologyName(techKey),
		oldLevel,
		newLevel,
		fundsCost,
		prestigeCost,
		effectText,
	))
}

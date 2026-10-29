package app

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func HandleCultivationAdminWriteCommand(bot *tgbotapi.BotAPI, message *tgbotapi.Message, text string) bool {
	text = strings.TrimSpace(text)

	if message == nil || message.From == nil {
		return isCultivationAdminWriteCommand(text)
	}

	session := telegram.GetSession(message.From.ID)
	step := session.GetStep()
	pendingWrite := step == "WAITING_CULTIVATION_ADMIN_WRITE_REASON" || step == "WAITING_CONFIRM_CULTIVATION_ADMIN_WRITE"
	if !pendingWrite && !isCultivationAdminWriteCommand(text) {
		return false
	}

	telegram.RegisterIncomingGroupCommandForAutoDelete(message)

	if !storage.IsSuperAdmin(message.From.ID) {
		telegram.SendPlainText(bot, message.Chat.ID, "权限不足：斗气配置写入指令仅限超级管理员。")
		if pendingWrite {
			telegram.ClearSession(message.From.ID)
		}
		return true
	}

	if !message.Chat.IsPrivate() {
		telegram.SendPlainText(bot, message.Chat.ID, "斗气配置写入属于高危操作，请私聊机器人执行，群内不会生效。")
		if pendingWrite {
			telegram.ClearSession(message.From.ID)
		}
		return true
	}

	if pendingWrite {
		return handleCultivationAdminWriteSession(bot, message, text, session)
	}

	normalizedText := normalizeCultivationAdminWriteCommand(text)
	if normalizedText == "" {
		telegram.SendPlainText(bot, message.Chat.ID, "指令格式错误。")
		return true
	}

	session.SetTemp("cultivation_admin_write_command", normalizedText)
	session.SetStep("WAITING_CULTIVATION_ADMIN_WRITE_REASON")
	telegram.UserSessions.Store(message.From.ID, session)

	telegram.SendPlainText(
		bot,
		message.Chat.ID,
		fmt.Sprintf(
			("高危操作：该指令会修改全服斗气配置。\n\n待执行指令：\n"+textfmt.Copyable("%s")+"\n\n请输入本次变更原因，")+storage.AdminReasonRequirementText+"：",
			textfmt.FormatPlainValue(normalizedText),
		),
	)
	return true
}

func handleCultivationAdminWriteSession(bot *tgbotapi.BotAPI, message *tgbotapi.Message, text string, session *telegram.SessionState) bool {
	switch session.GetStep() {
	case "WAITING_CULTIVATION_ADMIN_WRITE_REASON":
		if text == "取消" {
			telegram.SendPlainText(bot, message.Chat.ID, "已取消斗气配置写入。")
			telegram.ClearSession(message.From.ID)
			return true
		}

		reason, ok := storage.ValidateAdminReason(text)
		if !ok {
			telegram.SendPlainText(bot, message.Chat.ID, storage.AdminReasonInvalidText)
			return true
		}

		pendingCommand := session.GetTemp("cultivation_admin_write_command")
		if pendingCommand == "" || !isCultivationAdminWriteCommand(pendingCommand) {
			telegram.SendPlainText(bot, message.Chat.ID, "斗气配置写入会话状态异常，已中止。请重新发起流程。")
			telegram.ClearSession(message.From.ID)
			return true
		}

		session.SetTemp("cultivation_admin_write_reason", reason)
		session.SetStep("WAITING_CONFIRM_CULTIVATION_ADMIN_WRITE")
		telegram.UserSessions.Store(message.From.ID, session)

		telegram.SendPlainText(
			bot,
			message.Chat.ID,
			fmt.Sprintf(
				"高危操作二次确认：该指令会修改全服斗气配置。\n\n待执行指令：\n"+textfmt.Copyable("%s")+"\n\n原因：%s\n\n确认执行请回复："+textfmt.Copyable("确认执行斗气配置")+"\n取消请回复："+textfmt.Copyable("取消"),
				textfmt.FormatPlainValue(pendingCommand),
				textfmt.FormatPlainValue(reason),
			),
		)
		return true

	case "WAITING_CONFIRM_CULTIVATION_ADMIN_WRITE":
		if text != "确认执行斗气配置" {
			telegram.SendPlainText(bot, message.Chat.ID, "已取消斗气配置写入。")
			telegram.ClearSession(message.From.ID)
			return true
		}

		pendingCommand := session.GetTemp("cultivation_admin_write_command")
		reason, ok := storage.ValidateAdminReason(session.GetTemp("cultivation_admin_write_reason"))
		if pendingCommand == "" || !isCultivationAdminWriteCommand(pendingCommand) || !ok {
			telegram.SendPlainText(bot, message.Chat.ID, "斗气配置写入会话状态异常，已中止。请重新发起流程。")
			telegram.ClearSession(message.From.ID)
			return true
		}

		reply, err := executeCultivationAdminWriteCommand(message.From.ID, pendingCommand, reason)
		if err != nil {
			telegram.SendPlainText(bot, message.Chat.ID, textfmt.FormatPlainError(err))
			telegram.ClearSession(message.From.ID)
			return true
		}

		telegram.SendPlainText(bot, message.Chat.ID, reply)
		telegram.ClearSession(message.From.ID)
		return true
	}

	telegram.ClearSession(message.From.ID)
	telegram.SendPlainText(bot, message.Chat.ID, "斗气配置写入会话状态异常，已中止。请重新发起流程。")
	return true
}

func normalizeCultivationAdminWriteCommand(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "确认") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "确认"))
	}
	return text
}

func isCultivationAdminWriteCommand(text string) bool {
	text = normalizeCultivationAdminWriteCommand(text)
	return text == "重载斗气配置" ||
		strings.HasPrefix(text, "设置突破成功率 ") ||
		strings.HasPrefix(text, "设置突破消耗 ") ||
		strings.HasPrefix(text, "设置突破冷却 ") ||
		strings.HasPrefix(text, "设置突破最低修为 ") ||
		strings.HasPrefix(text, "设置境界门槛 ") ||
		strings.HasPrefix(text, "设置小境界门槛 ")
}

func executeCultivationAdminWriteCommand(actorID int64, text string, reason string) (string, error) {
	text = strings.TrimSpace(text)

	if text == "重载斗气配置" {
		if err := game.ReloadCultivationRules(); err != nil {
			return "", fmt.Errorf("重载斗气配置失败：%w", err)
		}

		if err := storage.WriteAuditLogInTx(storage.DB, actorID, "RELOAD_CULTIVATION_RULES", "cultivation_rules", 0, fmt.Sprintf("超级管理员手动重载斗气配置缓存，原因：%s", textfmt.FormatPlainValue(reason))); err != nil {
			return "", fmt.Errorf("写入斗气配置重载审计失败：%w", err)
		}
		return "斗气配置缓存已重载。", nil
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", fmt.Errorf("指令格式错误。")
	}

	switch parts[0] {
	case "设置突破成功率":
		if len(parts) != 3 {
			return "", fmt.Errorf(("用法：" + "`设置突破成功率 大境界 成功率百分比`" + "\n例如：") + "`设置突破成功率 1 80`")
		}

		fromMajor, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		percent, err := textfmt.ParseFloatRange(parts[2], "成功率百分比", 0, 100)
		if err != nil {
			return "", err
		}

		successRate := percent / 100
		if err := validateBreakthroughSuccessRateForProduction(fromMajor, successRate); err != nil {
			return "", err
		}

		return updateBreakthroughConfigField(
			actorID,
			fromMajor,
			"success_rate",
			successRate,
			fmt.Sprintf("突破成功率调整为 %.0f%%", percent),
			reason,
		)

	case "设置突破消耗":
		if len(parts) != 3 {
			return "", fmt.Errorf(("用法：" + "`设置突破消耗 大境界 积分`" + "\n例如：") + "`设置突破消耗 1 100`")
		}

		fromMajor, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		pointsCost, err := textfmt.ParseNonNegativeInt(parts[2], "积分消耗")
		if err != nil {
			return "", err
		}
		if err := validateBreakthroughPointsCostForProduction(fromMajor, pointsCost); err != nil {
			return "", err
		}

		return updateBreakthroughConfigField(
			actorID,
			fromMajor,
			"points_cost",
			pointsCost,
			fmt.Sprintf("突破积分消耗调整为 %d", pointsCost),
			reason,
		)

	case "设置突破冷却":
		if len(parts) != 3 {
			return "", fmt.Errorf(("用法：" + "`设置突破冷却 大境界 小时`" + "\n例如：") + "`设置突破冷却 1 24`")
		}

		fromMajor, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		cooldownHours, err := textfmt.ParseNonNegativeInt(parts[2], "冷却小时")
		if err != nil {
			return "", err
		}
		if err := validateBreakthroughCooldownForProduction(fromMajor, cooldownHours); err != nil {
			return "", err
		}

		return updateBreakthroughConfigField(
			actorID,
			fromMajor,
			"cooldown_hours",
			cooldownHours,
			fmt.Sprintf("突破冷却调整为 %d 小时", cooldownHours),
			reason,
		)

	case "设置突破最低修为":
		if len(parts) != 3 {
			return "", fmt.Errorf(("用法：" + "`设置突破最低修为 大境界 小时`" + "\n例如：") + "`设置突破最低修为 1 50`")
		}

		fromMajor, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		minHours, err := parseNonNegativeFloat(parts[2], "最低修为小时")
		if err != nil {
			return "", err
		}
		if err := validateBreakthroughMinHoursForProduction(fromMajor, minHours); err != nil {
			return "", err
		}

		return updateBreakthroughConfigField(
			actorID,
			fromMajor,
			"min_total_hours",
			minHours,
			fmt.Sprintf("突破最低修为调整为 %.1f 小时", minHours),
			reason,
		)

	case "设置境界门槛":
		if len(parts) != 4 {
			return "", fmt.Errorf(("用法：" + "`设置境界门槛 大境界 最低小时 最高小时`" + "\n最高小时填 0 表示无上限\n例如：") + "`设置境界门槛 1 10 50`")
		}

		major, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		minHours, err := parseNonNegativeFloat(parts[2], "最低小时")
		if err != nil {
			return "", err
		}

		maxHours, err := parseNonNegativeFloat(parts[3], "最高小时")
		if err != nil {
			return "", err
		}

		return updateRealmThreshold(actorID, major, minHours, maxHours, reason)

	case "设置小境界门槛":
		if len(parts) != 4 {
			return "", fmt.Errorf(("用法：" + "`设置小境界门槛 大境界 小境界 所需小时`" + "\n例如：") + "`设置小境界门槛 1 2 20`")
		}

		major, err := textfmt.ParseNonNegativeInt(parts[1], "大境界")
		if err != nil {
			return "", err
		}

		minor, err := textfmt.ParseNonNegativeInt(parts[2], "小境界")
		if err != nil {
			return "", err
		}

		requiredHours, err := parseNonNegativeFloat(parts[3], "所需小时")
		if err != nil {
			return "", err
		}

		return updateMinorRealmThreshold(actorID, major, minor, requiredHours, reason)
	}

	return "", fmt.Errorf("未知斗气配置写入指令。")
}

func updateBreakthroughConfigField(actorID int64, fromMajor int, field string, value interface{}, detail string, reason string) (string, error) {
	allowedFields := map[string]bool{
		"success_rate":    true,
		"points_cost":     true,
		"cooldown_hours":  true,
		"min_total_hours": true,
	}
	if !allowedFields[field] {
		return "", fmt.Errorf("不允许更新该突破配置字段：%s", field)
	}

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var cfg model.BreakthroughConfig
		if err := tx.Where("from_major_realm = ?", fromMajor).First(&cfg).Error; err != nil {
			return fmt.Errorf("未找到突破配置：from_major_realm=%d", fromMajor)
		}

		res := tx.Model(&model.BreakthroughConfig{}).
			Where("id = ? AND from_major_realm = ?", cfg.ID, fromMajor).
			Update(field, value)
		if res.Error != nil {
			return fmt.Errorf("更新突破配置失败：%w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("突破配置更新未命中，请重试")
		}

		rules, err := game.LoadCultivationRulesFromDBHandle(tx)
		if err != nil {
			return fmt.Errorf("重新加载斗气配置失败：%w", err)
		}

		if err := game.ValidateCultivationRuleSet(rules); err != nil {
			return fmt.Errorf("配置校验失败，已回滚本次修改：%w", err)
		}

		if err := storage.WriteAuditLogInTx(
			tx,
			actorID,
			"UPDATE_BREAKTHROUGH_CONFIG",
			fmt.Sprintf("from_major_realm=%d field=%s", fromMajor, field),
			0,
			fmt.Sprintf("%s，原因：%s", detail, textfmt.FormatPlainValue(reason)),
		); err != nil {
			return fmt.Errorf("写入斗气配置审计失败：%w", err)
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	if err := game.ReloadCultivationRules(); err != nil {
		return "", fmt.Errorf("数据库已更新，但刷新斗气配置缓存失败：%w", err)
	}

	return fmt.Sprintf("已更新突破配置：from_major_realm=%d，%s。", fromMajor, detail), nil
}

func updateRealmThreshold(actorID int64, major int, minHours float64, maxHours float64, reason string) (string, error) {
	if maxHours > 0 && maxHours < minHours {
		return "", fmt.Errorf("境界最高小时不能小于最低小时。")
	}

	var realmName string

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var realm model.CultivationRealmConfig
		if err := tx.Where("major_realm = ?", major).First(&realm).Error; err != nil {
			return fmt.Errorf("未找到境界配置：major_realm=%d", major)
		}

		if realm.IsMaxRealm && maxHours > 0 {
			return fmt.Errorf("当前最高境界建议保持最高小时为 0，表示无上限。")
		}

		realmName = realm.Name

		res := tx.Model(&model.CultivationRealmConfig{}).
			Where("id = ? AND major_realm = ?", realm.ID, major).
			Updates(map[string]interface{}{
				"min_total_hours": minHours,
				"max_total_hours": maxHours,
			})
		if res.Error != nil {
			return fmt.Errorf("更新境界门槛失败：%w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("境界门槛更新未命中，请重试")
		}

		rules, err := game.LoadCultivationRulesFromDBHandle(tx)
		if err != nil {
			return fmt.Errorf("重新加载斗气配置失败：%w", err)
		}

		if err := game.ValidateCultivationRuleSet(rules); err != nil {
			return fmt.Errorf("配置校验失败，已回滚本次修改：%w", err)
		}

		detail := fmt.Sprintf("境界 %s(%d) 门槛调整为 %.1f - %.1f 小时", textfmt.FormatPlainValue(realmName), major, minHours, maxHours)
		if err := storage.WriteAuditLogInTx(tx, actorID, "UPDATE_REALM_THRESHOLD", fmt.Sprintf("major_realm=%d", major), 0, fmt.Sprintf("%s，原因：%s", detail, textfmt.FormatPlainValue(reason))); err != nil {
			return fmt.Errorf("写入斗气配置审计失败：%w", err)
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	if err := game.ReloadCultivationRules(); err != nil {
		return "", fmt.Errorf("数据库已更新，但刷新斗气配置缓存失败：%w", err)
	}

	return fmt.Sprintf("已更新境界门槛：%s，最低 %.1f 小时，最高 %.1f 小时。", textfmt.FormatPlainValue(realmName), minHours, maxHours), nil
}

func updateMinorRealmThreshold(actorID int64, major int, minor int, requiredHours float64, reason string) (string, error) {
	if minor < 1 || minor > 4 {
		return "", fmt.Errorf("小境界只能是 1-4。")
	}

	var minorName string

	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		var cfg model.CultivationMinorRealmConfig
		if err := tx.Where("major_realm = ? AND minor_realm = ?", major, minor).First(&cfg).Error; err != nil {
			return fmt.Errorf("未找到小境界配置：major_realm=%d minor_realm=%d", major, minor)
		}

		minorName = cfg.Name

		res := tx.Model(&model.CultivationMinorRealmConfig{}).
			Where("id = ? AND major_realm = ? AND minor_realm = ?", cfg.ID, major, minor).
			Update("required_hours", requiredHours)
		if res.Error != nil {
			return fmt.Errorf("更新小境界门槛失败：%w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("小境界门槛更新未命中，请重试")
		}

		rules, err := game.LoadCultivationRulesFromDBHandle(tx)
		if err != nil {
			return fmt.Errorf("重新加载斗气配置失败：%w", err)
		}

		if err := game.ValidateCultivationRuleSet(rules); err != nil {
			return fmt.Errorf("配置校验失败，已回滚本次修改：%w", err)
		}

		detail := fmt.Sprintf("小境界 %d-%d(%s) 门槛调整为 %.1f 小时", major, minor, textfmt.FormatPlainValue(minorName), requiredHours)
		if err := storage.WriteAuditLogInTx(tx, actorID, "UPDATE_MINOR_REALM_THRESHOLD", fmt.Sprintf("major_realm=%d minor_realm=%d", major, minor), 0, fmt.Sprintf("%s，原因：%s", detail, textfmt.FormatPlainValue(reason))); err != nil {
			return fmt.Errorf("写入斗气配置审计失败：%w", err)
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	if err := game.ReloadCultivationRules(); err != nil {
		return "", fmt.Errorf("数据库已更新，但刷新斗气配置缓存失败：%w", err)
	}

	return fmt.Sprintf("已更新小境界门槛：%d-%d %s，所需 %.1f 小时。", major, minor, textfmt.FormatPlainValue(minorName), requiredHours), nil
}

func parseNonNegativeFloat(raw string, fieldName string) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("%s必须是非负有限数字。", fieldName)
	}
	return value, nil
}

func validateBreakthroughSuccessRateForProduction(fromMajor int, successRate float64) error {
	if fromMajor <= 0 {
		return nil
	}

	if successRate <= 0 {
		return fmt.Errorf("生产保护：大境界突破成功率不能设置为 0%%。")
	}

	if successRate > 0.95 {
		return fmt.Errorf("生产保护：大境界突破成功率不能超过 95%%，避免误操作导致全服无成本晋升。")
	}

	return nil
}

func validateBreakthroughPointsCostForProduction(fromMajor int, pointsCost int) error {
	if fromMajor <= 0 {
		return nil
	}

	if pointsCost <= 0 {
		return fmt.Errorf("生产保护：非初始境界突破消耗不能设置为 0。")
	}

	return nil
}

func validateBreakthroughCooldownForProduction(fromMajor int, cooldownHours int) error {
	if fromMajor <= 0 {
		return nil
	}

	if cooldownHours < 1 {
		return fmt.Errorf("生产保护：非初始境界突破冷却不能低于 1 小时。")
	}

	return nil
}

func validateBreakthroughMinHoursForProduction(fromMajor int, minHours float64) error {
	if fromMajor <= 0 {
		return nil
	}

	if minHours <= 0 {
		return fmt.Errorf("生产保护：非初始境界突破最低修为必须大于 0 小时。")
	}

	return nil
}

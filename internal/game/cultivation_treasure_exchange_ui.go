package game

import (
	"errors"
	"fmt"
	"log"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func CultivationTreasureExchangeErrorText(err error) string {
	for _, expected := range []error{ErrCultivationTreasureUnavailable, ErrCultivationTreasureNoProfile,
		ErrCultivationTreasureTokens, ErrCultivationTreasureExpired, ErrCultivationTreasureCompleted, ErrCultivationTreasureRealmChanged} {
		if errors.Is(err, expected) {
			return expected.Error()
		}
	}
	return "材料兑换暂时不可用，请稍后再试。"
}

func cultivationTreasureBackMarkup() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回突破材料", "shop:treasure"),
		tgbotapi.NewInlineKeyboardButtonData("返回聚宝斋", "shop:home")))
}

func renderCultivationTreasureShop(userID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	cul, name, tokens, err := cultivationTreasureExchangeOffer(storage.DB, userID)
	if err != nil {
		if !errors.Is(err, ErrCultivationTreasureUnavailable) && !errors.Is(err, ErrCultivationTreasureNoProfile) {
			log.Printf("突破材料读取失败 user=%d err=%s", userID, textfmt.FormatPlainError(err))
		}
		return "🏪 **【聚宝斋·突破材料】**\n\n" + CultivationTreasureExchangeErrorText(err), telegram.TreasureShopBackMarkup()
	}
	text := fmt.Sprintf("🏪 **【聚宝斋·突破材料】**\n\n当前境界：%s\n所需至宝：**%s**\n秘境信物：`%d` 个\n\n兑换：`%d` 个秘境信物 → 至宝 `1` 件\n\n信物来自宗门秘境；兑换后放入乾坤袋，用于突破下一境。仅兑换当前境界对应至宝，至宝不可上架交易行。世界 Boss 仍有 10%% 概率掉落。",
		textfmt.EscapeMarkdown(GetRealmName(cul)), textfmt.InventoryItemMarkdownName(name), tokens, cultivationTreasureTokenCost)
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if tokens >= cultivationTreasureTokenCost {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("兑换 1 件至宝", "shop:treasure_prepare")))
	} else {
		text += "\n\n秘境信物不足，请参与宗门秘境积累。"
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("返回聚宝斋", "shop:home")))
	return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func HandleCultivationTreasureShopCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery, parts []string) {
	var text string
	var markup tgbotapi.InlineKeyboardMarkup
	var err error
	switch parts[1] {
	case "treasure":
		text, markup = renderCultivationTreasureShop(cb.From.ID)
	case "treasure_prepare":
		var exchange *model.CultivationTreasureExchange
		exchange, err = PrepareCultivationTreasureExchange(storage.DB, cb.From.ID)
		if err == nil {
			text = fmt.Sprintf("🏪 **【突破材料兑换确认】**\n\n消耗秘境信物：`%d` 个\n获得：**%s** × `1`\n\n确认后放入乾坤袋；本确认 15 分钟内有效。", exchange.TokenCost, textfmt.InventoryItemMarkdownName(exchange.TreasureName))
			markup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("确认兑换", fmt.Sprintf("shop:treasure_confirm:%d", exchange.ID)),
				tgbotapi.NewInlineKeyboardButtonData("取消", "shop:treasure")))
		}
	case "treasure_confirm":
		err = ErrCultivationTreasureExpired
		if len(parts) == 3 {
			if id, parseErr := strconv.ParseUint(parts[2], 10, 32); parseErr == nil && id > 0 {
				var exchange *model.CultivationTreasureExchange
				exchange, err = ConfirmCultivationTreasureExchange(storage.DB, cb.From.ID, uint(id))
				if err == nil {
					text = fmt.Sprintf("✅ **兑换成功**\n\n消耗秘境信物 `%d` 个，获得 **%s** × `1`，已放入乾坤袋。", exchange.TokenCost, textfmt.InventoryItemMarkdownName(exchange.TreasureName))
					markup = cultivationTreasureBackMarkup()
				}
			}
		}
	}
	if err != nil {
		log.Printf("突破材料兑换未完成 user=%d err=%s", cb.From.ID, textfmt.FormatPlainError(err))
		text = "❌ " + CultivationTreasureExchangeErrorText(err)
		markup = cultivationTreasureBackMarkup()
	}
	telegram.AnswerCallback(bot, cb.ID, "突破材料面板已更新")
	telegram.EditMenuPanel(bot, cb.Message.Chat.ID, cb.Message.MessageID, text, markup)
}

package titles

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

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func Render(chatID, userID int64, ranking bool) (string, tgbotapi.InlineKeyboardMarkup, error) {
	var b strings.Builder
	var rows [][]tgbotapi.InlineKeyboardButton
	err := Transaction(func(tx *gorm.DB) error {
		if err := refreshRankTitlesInTx(tx, time.Now()); err != nil {
			return err
		}
		var group model.TitleGroup
		if err := tx.First(&group, "chat_id = ?", chatID).Error; err != nil {
			return err
		}
		fmt.Fprintf(&b, "🏅 %s · %s\n\n", map[bool]string{false: "我的称号", true: "称号榜"}[ranking], textfmt.LotteryDisplayText(group.Name, 40, "交流群"))
		b.WriteString("境界称号默认佩戴，不限持有人数。榜首称号全服唯一，避雷称号每群唯一；并列保留原持有者。\n\n")
		var equipment model.TitleEquipment
		if !ranking {
			err := tx.Where("chat_id = ? AND user_id = ?", chatID, userID).First(&equipment).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			realmTitle, available, err := RealmTitleInTx(tx, userID)
			if err != nil {
				return err
			}
			if equipment.TitleKey == RealmKey || equipment.TitleKey == "" {
				if available {
					fmt.Fprintf(&b, "当前佩戴：📿 %s（境界称号）\n", realmTitle)
				} else {
					b.WriteString("当前佩戴：无\n")
				}
			} else if title, ok := ByKey(equipment.TitleKey); ok {
				fmt.Fprintf(&b, "当前佩戴：%s%s\n", title.Emoji, title.Name)
			} else {
				b.WriteString("当前佩戴：无\n")
			}
			if equipment.SyncHint != "" {
				b.WriteString(textfmt.FormatPlainValue(equipment.SyncHint) + "\n")
			}
			b.WriteByte('\n')
			b.WriteString("📿 境界称号｜账号自带，随境界自动更新\n")
			if available {
				fmt.Fprintf(&b, "当前境界：%s\n默认佩戴，恢复后会跟随境界变化。\n\n", realmTitle)
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("佩戴 · "+realmTitle, fmt.Sprintf("titles:equip:%d:realm", chatID))))
			} else {
				b.WriteString("尚未绑定听书账号，暂无境界称号。\n\n")
			}
		}
		if err := renderRankTitlesInTx(tx, &b, &rows, chatID, userID, ranking); err != nil {
			return err
		}
		b.WriteString("⚡ 天雷猜阵 · 唯一称号\n封阵时两边都有人才累计次数。\n\n")
		for _, title := range UniqueDefinitions {
			fmt.Fprintf(&b, "%s %s｜%s\n", title.Emoji, title.Name, title.Description)
			var holder model.TribulationTitleHolder
			err := tx.Where("chat_id = ? AND title_key = ?", chatID, title.Key).First(&holder).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if holder.UserID == 0 {
				b.WriteString("当前空缺\n")
			} else {
				fmt.Fprintf(&b, "现任：%s · %d 次\n", textfmt.LotteryDisplayText(holder.UserName, 24, "道友"), holder.Count)
			}
			if ranking {
				var leaders []model.TribulationTitleStat
				if err := tx.Where("chat_id = ? AND title_key = ?", chatID, title.Key).Order(clause.OrderBy{Expression: clause.Expr{
					SQL:  "count DESC, CASE WHEN user_id = ? THEN 0 ELSE 1 END, last_achieved_at, last_entry_id, user_id",
					Vars: []any{holder.UserID},
				}}).Limit(3).Find(&leaders).Error; err != nil {
					return err
				}
				for i, leader := range leaders {
					fmt.Fprintf(&b, "%d. %s · %d 次\n", i+1, textfmt.LotteryDisplayText(leader.UserName, 24, "道友"), leader.Count)
				}
			} else {
				var stat model.TribulationTitleStat
				err := tx.Where("chat_id = ? AND user_id = ? AND title_key = ?", chatID, userID, title.Key).First(&stat).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				fmt.Fprintf(&b, "我的记录：%d 次", stat.Count)
				if holder.UserID == userID {
					b.WriteString(" · 当前持有\n")
					var runner model.TribulationTitleStat
					err := tx.Where("chat_id = ? AND title_key = ? AND user_id <> ?", chatID, title.Key, userID).Order("count DESC").First(&runner).Error
					if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
						return err
					}
					fmt.Fprintf(&b, "其他玩家最高：%d 次\n", runner.Count)
					rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("佩戴 · "+title.Name, fmt.Sprintf("titles:equip:%d:%s", chatID, title.Key))))
				} else {
					var history int64
					if err := tx.Model(&model.TribulationTitleHistory{}).Where("chat_id = ? AND user_id = ? AND title_key = ?", chatID, userID, title.Key).Count(&history).Error; err != nil {
						return err
					}
					if history > 0 {
						b.WriteString(" · 曾经持有")
					}
					gap := holder.Count - stat.Count + 1
					if gap < 1 {
						gap = 1
					}
					fmt.Fprintf(&b, "\n还需增加 %d 次才能%s\n", gap, map[bool]string{false: "获得", true: "夺回"}[history > 0])
				}
			}
			b.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if !ranking {
		b.WriteString("每群仅佩戴一个称号；称号失效时回到境界称号。普通成员同步群标签，管理员保留原头衔。\n")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("恢复默认称号", fmt.Sprintf("titles:clear:%d", chatID)), tgbotapi.NewInlineKeyboardButtonData("称号榜", fmt.Sprintf("titles:rank:%d", chatID))))
	} else {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("我的称号", fmt.Sprintf("titles:view:%d", chatID))))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("选择交流群", "titles:groups:0")))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("返回我的档案", "menu:profile")))
	return b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

func renderGroups(userID int64, page int) (string, tgbotapi.InlineKeyboardMarkup, error) {
	const size = 8
	if page < 0 || page > 10000 {
		page = 0
	}
	var groups []model.TitleGroup
	err := storage.DB.Where("chat_id IN (SELECT chat_id FROM title_members WHERE user_id = ?)", userID).Order("chat_id").Offset(page * size).Limit(size + 1).Find(&groups).Error
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	var realmTitle string
	var available bool
	var honors strings.Builder
	err = Transaction(func(tx *gorm.DB) error {
		if err := refreshRankTitlesInTx(tx, time.Now()); err != nil {
			return err
		}
		var err error
		realmTitle, available, err = RealmTitleInTx(tx, userID)
		if err != nil {
			return err
		}
		return renderRankTitlesInTx(tx, &honors, nil, 0, userID, false)
	})
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	text := "🏅 我的称号\n\n"
	if available {
		text += "📿 境界称号：" + realmTitle + "（账号自带，默认佩戴）\n\n"
	} else {
		text += "绑定听书账号后可获得默认境界称号。\n\n"
	}
	text += honors.String()
	if len(groups) == 0 {
		text += "暂无本页的群记录；在机器人所在群发言后即可管理本群佩戴。"
	} else {
		text += "请选择交流群，查看各来源称号并切换佩戴。"
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, group := range groups {
		if i == size {
			break
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(textfmt.LotteryDisplayText(group.Name, 30, "交流群"), fmt.Sprintf("titles:view:%d", group.ChatID))))
	}
	var nav []tgbotapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("上一页", fmt.Sprintf("titles:groups:%d", page-1)))
	}
	if len(groups) > size {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("下一页", fmt.Sprintf("titles:groups:%d", page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("返回我的档案", "menu:profile")))
	return text, tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

func sendPanel(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string, markup tgbotapi.InlineKeyboardMarkup) {
	if messageID > 0 {
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		edit.ReplyMarkup = &markup
		if _, err := bot.Request(edit); err != nil && !strings.Contains(strings.ToLower(err.Error()), "message is not modified") {
			log.Printf("⚠️ 刷新称号页面失败: %s", textfmt.FormatTelegramSendError(err))
		}
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = markup
	if _, err := telegram.SendAutoDelete(bot, msg); err != nil {
		log.Printf("⚠️ 发送称号页面失败: %s", textfmt.FormatTelegramSendError(err))
	}
}

func HandleCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	switch text {
	case "我的称号", "称号榜":
	default:
		return false
	}
	if msg == nil || msg.From == nil || msg.Chat == nil {
		return true
	}
	if !msg.Chat.IsPrivate() {
		telegram.RegisterIncomingGroupCommandForAutoDelete(msg)
		if text == "我的称号" {
			telegram.SendPlainText(bot, msg.Chat.ID, "请私聊我，在「我的档案 → 我的称号」中查看和佩戴称号。")
			return true
		}
		if err := TrackMember(msg.Chat.ID, msg.Chat.Title, msg.From.ID); err != nil {
			telegram.SendPlainText(bot, msg.Chat.ID, errorText(err))
			return true
		}
		out, _, err := Render(msg.Chat.ID, msg.From.ID, true)
		if err != nil {
			out = errorText(err)
		}
		telegram.SendPlainText(bot, msg.Chat.ID, out)
		return true
	}
	if config.AppConfig != nil && config.AppConfig.NoticeGroupID < 0 {
		if err := TrackMember(config.AppConfig.NoticeGroupID, "", msg.From.ID); err != nil {
			telegram.SendPlainText(bot, msg.Chat.ID, errorText(err))
			return true
		}
	}
	out, markup, err := renderGroups(msg.From.ID, 0)
	if err != nil {
		telegram.SendPlainText(bot, msg.Chat.ID, errorText(err))
		return true
	}
	sendPanel(bot, msg.Chat.ID, 0, out, markup)
	return true
}

func HandleCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) bool {
	if cb == nil || !strings.HasPrefix(cb.Data, "titles:") {
		return false
	}
	if cb.From == nil || cb.Message == nil || cb.Message.Chat == nil || !cb.Message.Chat.IsPrivate() || cb.Message.Chat.ID != cb.From.ID {
		telegram.AnswerCallback(bot, cb.ID, "请在自己的私聊中管理称号")
		return true
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 3 {
		telegram.AnswerCallback(bot, cb.ID, "称号面板已失效")
		return true
	}
	var out string
	var markup tgbotapi.InlineKeyboardMarkup
	var err error
	if parts[1] == "groups" && len(parts) == 3 {
		page, parseErr := strconv.Atoi(parts[2])
		if parseErr != nil {
			telegram.AnswerCallback(bot, cb.ID, "页码无效")
			return true
		}
		out, markup, err = renderGroups(cb.From.ID, page)
	} else {
		chatID, parseErr := strconv.ParseInt(parts[2], 10, 64)
		if parseErr != nil || chatID >= 0 {
			telegram.AnswerCallback(bot, cb.ID, "交流群无效")
			return true
		}
		// 回调 data 可以被客户端伪造；私聊身份相符不代表有权访问任意群。
		var member model.TitleMember
		if memberErr := storage.DB.Where("chat_id = ? AND user_id = ?", chatID, cb.From.ID).Take(&member).Error; memberErr != nil {
			if errors.Is(memberErr, gorm.ErrRecordNotFound) {
				telegram.AnswerCallback(bot, cb.ID, "请先在该交流群发言，再从「我的称号」选择该群")
			} else {
				telegram.AnswerCallback(bot, cb.ID, errorText(memberErr))
			}
			return true
		}
		switch parts[1] {
		case "equip":
			if len(parts) != 4 {
				telegram.AnswerCallback(bot, cb.ID, "称号无效")
				return true
			}
			err = Equip(chatID, cb.From.ID, parts[3])
		case "clear":
			if len(parts) != 3 {
				telegram.AnswerCallback(bot, cb.ID, "称号无效")
				return true
			}
			err = Equip(chatID, cb.From.ID, "")
		case "view", "rank":
			if len(parts) != 3 {
				telegram.AnswerCallback(bot, cb.ID, "称号无效")
				return true
			}
		default:
			telegram.AnswerCallback(bot, cb.ID, "未知称号操作")
			return true
		}
		if err == nil {
			out, markup, err = Render(chatID, cb.From.ID, parts[1] == "rank")
		}
	}
	if err != nil {
		telegram.AnswerCallback(bot, cb.ID, errorText(err))
		return true
	}
	telegram.AnswerCallback(bot, cb.ID, "已更新")
	sendPanel(bot, cb.Message.Chat.ID, cb.Message.MessageID, out, markup)
	// 同步由独立巡检执行，避免标签请求拖慢按钮应答和其他玩家的结算。
	return true
}

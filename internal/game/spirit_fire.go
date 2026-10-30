package game

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func HandleSpiritFireCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.Chat == nil || msg.From == nil {
		return false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	switch cmd {
	case "异火寻缘", "购买焚诀", "合成焚诀", "异火榜", "异火持有榜", "我的异火", "焚诀", "启用异火", "学习异火", "回收异火", "放弃异火", "确认异火", "查看异火配置", "设置异火配置":
	default:
		return false
	}
	if !msg.Chat.IsPrivate() && cmd != "异火榜" && cmd != "异火持有榜" {
		telegram.SendPlainText(bot, msg.Chat.ID, "异火操作请私聊 Bot 执行。")
		return true
	}
	send := func(s string) {
		r := []rune(s)
		for len(r) > 3000 {
			telegram.SendPlainText(bot, msg.Chat.ID, string(r[:3000]))
			r = r[3000:]
		}
		if len(r) > 0 {
			telegram.SendPlainText(bot, msg.Chat.ID, string(r))
		}
	}
	fail := func(err error) { send("❌ 异火操作未完成：" + textfmt.FormatPlainError(err)) }
	now := time.Now()
	user := msg.From.ID
	if !spiritfire.Installed(storage.DB) {
		send("异火系统尚未开启。")
		return true
	}
	switch cmd {
	case "异火寻缘", "购买焚诀", "合成焚诀":
		if cmd == "异火寻缘" && len(fields) == 1 {
			panel, markup, err := spiritFireSeekingPanel(user)
			if err != nil {
				fail(err)
				break
			}
			out := tgbotapi.NewMessage(msg.Chat.ID, panel)
			out.ReplyMarkup = markup
			if _, err := bot.Send(out); err != nil {
				send("面板发送失败，请重新发送“异火寻缘”。")
			}
			break
		}
		quantity := 1
		if cmd == "合成焚诀" {
			if len(fields) != 1 {
				send("用法：合成焚诀（10张残页合成一本）")
				break
			}
		} else {
			if len(fields) != 2 {
				send("用法：" + cmd + " 1 或 " + cmd + " 10")
				break
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil || (n != 1 && n != 10) {
				send("数量仅支持1或10。")
				break
			}
			quantity = n
		}
		action := map[string]string{"异火寻缘": "seek", "购买焚诀": "scroll_buy", "合成焚诀": "scroll_merge"}[cmd]
		q, err := spiritfire.SeekingQuote(storage.DB, user, action, quantity, now)
		if err != nil {
			fail(err)
		} else {
			send(seekingQuoteText(q))
		}
	case "异火榜", "异火持有榜":
		text, err := spiritFireBoard(cmd == "异火持有榜", now)
		if err != nil {
			fail(err)
		} else {
			send(text)
		}
	case "我的异火", "焚诀":
		text, err := spiritFirePanel(user, now)
		if err != nil {
			fail(err)
		} else {
			send(text)
		}
	case "启用异火":
		if len(fields) < 2 {
			send("用法：启用异火 编号 编号…；只启用一火无需掌控，融合多火须全部已掌控。发送“启用异火 无”关闭。次日生效。")
			break
		}
		ids := []uint{}
		valid := true
		if !(len(fields) == 2 && fields[1] == "无") {
			for _, v := range fields[1:] {
				n, err := strconv.ParseUint(v, 10, 32)
				if err != nil || n == 0 {
					valid = false
					break
				}
				ids = append(ids, uint(n))
			}
		}
		if !valid {
			send("请输入异火榜中的数字编号。")
			break
		}
		if err := spiritfire.Equip(storage.DB, user, ids, now); err != nil {
			fail(err)
		} else {
			send("✅ 异火搭配已保存，北京时间次日生效。")
		}
	case "学习异火", "回收异火", "放弃异火":
		if len(fields) != 2 {
			send("用法：" + cmd + " 异火编号；编号见“我的异火”。")
			break
		}
		id, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil || id == 0 {
			send("请输入正确异火编号。")
			break
		}
		action := map[string]string{"学习异火": "learn", "回收异火": "cashout", "放弃异火": "release"}[cmd]
		q, err := spiritfire.Quote(storage.DB, user, uint(id), action, now)
		if err != nil {
			fail(err)
			break
		}
		detail := fmt.Sprintf("消耗%d积分、焚诀古卷%d本；永久掌控记录不会随交易重置。", q.Points, q.Scrolls)
		if action == "cashout" {
			detail = fmt.Sprintf("回收到账%d积分，异火回归天地；掌控记录保留。", q.Points)
		}
		if action == "release" {
			detail = "无偿放弃，异火回归天地；掌控记录保留。"
		}
		send(fmt.Sprintf("请确认%s #%d\n%s\n确认单15分钟有效。\n回复：确认异火 %s", cmd, id, detail, q.ID))
	case "确认异火":
		if len(fields) != 2 {
			send("请回复：确认异火 确认单编号")
			break
		}
		if err := spiritfire.Confirm(storage.DB, user, fields[1], now); err != nil {
			fail(err)
		} else {
			receipt, err := seekingReceipt(user, fields[1])
			if err != nil {
				send("✅ 操作已完成，但回执读取失败；请重新确认同一确认单查看，或发送“异火寻缘”核对资产。")
				break
			}
			if receipt != "" {
				send(receipt)
				break
			}
			send("✅ 异火操作已完成。可发送“我的异火”查看；需要启用时请重新选择搭配，修为效果次日生效。")
		}
	case "查看异火配置", "设置异火配置":
		if err := spiritfire.RequireAdmin(storage.DB, user); err != nil {
			send("仅超级管理员可查看和设置异火配置。")
			break
		}
		if cmd == "查看异火配置" {
			r, err := spiritfire.RulesAt(storage.DB, spiritfire.Day(now))
			if err != nil {
				fail(err)
				break
			}
			send("当前异火配置：\n" + spiritfire.Encode(r) + "\n使用：设置异火配置 完整JSON\n概率按百分数，prices和percents依次为黄、玄、地、天；修改后二次确认，次日生效。")
			break
		}
		raw := strings.TrimSpace(strings.TrimPrefix(text, cmd))
		r, err := spiritfire.ParseRules(raw)
		if err != nil {
			fail(err)
			break
		}
		q, err := spiritfire.RulesQuote(storage.DB, user, r, now)
		if err != nil {
			fail(err)
			break
		}
		send("异火配置将于北京时间次日生效：\n" + q.RulesJSON + "\n回复：确认异火 " + q.ID)
	}
	return true
}

func spiritFireBoard(holders bool, now time.Time) (string, error) {
	var fs []model.SpiritFire
	if err := storage.DB.Order("rank").Find(&fs).Error; err != nil {
		return "", err
	}
	r, err := spiritfire.RulesAt(storage.DB, spiritfire.Day(now))
	if err != nil {
		return "", err
	}
	var users []model.User
	if err := storage.DB.Select("telegram_id", "username").Find(&users).Error; err != nil {
		return "", err
	}
	names := map[int64]string{}
	for _, u := range users {
		names[u.TelegramID] = textfmt.FormatPlainValue(u.Username)
	}
	ownerName := func(id int64) string {
		if n := names[id]; n != "" {
			return fmt.Sprintf("%s（%d）", n, id)
		}
		return fmt.Sprint(id)
	}
	var b strings.Builder
	if !holders {
		b.WriteString("🔥 全服异火榜\n每种唯一，编号与原著榜位一致。\n\n")
		for _, f := range fs {
			if f.Rank == 1 {
				b.WriteString("#1 帝炎｜特殊｜暂未开放\n")
				continue
			}
			status := "未认主"
			if f.OwnerID != 0 {
				status = ownerName(f.OwnerID)
			}
			if f.ListingID != 0 {
				status += fmt.Sprintf("｜交易中 #%d", f.ListingID)
			}
			fmt.Fprintf(&b, "#%d %s｜%s阶｜+%d%%｜基价%d｜%s\n", f.ID, f.Name, spiritfire.GradeName(f.Grade), r.Percents[f.Grade], r.Prices[f.Grade], status)
		}
		return b.String(), nil
	}
	type entry struct {
		ID           int64
		Count, Score int
		At           time.Time
	}
	m := map[int64]*entry{}
	for _, f := range fs {
		if f.OwnerID == 0 {
			continue
		}
		e := m[f.OwnerID]
		if e == nil {
			e = &entry{ID: f.OwnerID}
			m[f.OwnerID] = e
		}
		e.Count++
		e.Score += f.Grade + 1
		if f.AcquiredAt.After(e.At) {
			e.At = f.AcquiredAt
		}
	}
	var wallets []model.SpiritFireWallet
	if err := storage.DB.Find(&wallets).Error; err != nil {
		return "", err
	}
	for _, w := range wallets {
		if e := m[w.UserID]; e != nil && !w.HoldingsChangedAt.IsZero() {
			e.At = w.HoldingsChangedAt
		}
	}
	entries := []*entry{}
	for _, e := range m {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, c := entries[i], entries[j]
		if a.Count != c.Count {
			return a.Count > c.Count
		}
		if a.Score != c.Score {
			return a.Score > c.Score
		}
		if !a.At.Equal(c.At) {
			return a.At.Before(c.At)
		}
		return a.ID < c.ID
	})
	b.WriteString("🏆 异火持有榜\n\n")
	for i, e := range entries {
		fmt.Fprintf(&b, "%d. %s｜%d种｜品阶分%d\n", i+1, ownerName(e.ID), e.Count, e.Score)
	}
	if len(entries) == 0 {
		b.WriteString("尚无异火认主。")
	}
	return b.String(), nil
}
func spiritFirePanel(user int64, now time.Time) (string, error) {
	var fs []model.SpiritFire
	var masters []model.SpiritFireMastery
	var w model.SpiritFireWallet
	var today model.SpiritFireDaily
	var next []uint
	var r spiritfire.Rules
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.Lock(tx); err != nil {
			return err
		}
		if err := tx.Where("owner_id = ?", user).Order("rank").Find(&fs).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", user).Order("created_at").Find(&masters).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", user).First(&w).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var err error
		today, err = spiritfire.Snapshot(tx, user, spiritfire.Day(now))
		if err != nil {
			return err
		}
		next, err = spiritfire.LoadoutAt(tx, user, spiritfire.Tomorrow(now))
		if err != nil {
			return err
		}
		r, err = spiritfire.RulesAt(tx, spiritfire.Day(now))
		return err
	})
	if err != nil {
		return "", err
	}
	learned := map[uint]bool{}
	for _, m := range masters {
		learned[m.FireID] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔥 我的异火与焚诀\n持有%d种｜历史掌控%d种｜古卷%d本\n下一种掌控：%d积分＋%d本古卷\n\n", len(fs), len(masters), w.Scrolls, len(masters)*1500, len(masters))
	fmt.Fprintf(&b, "焚诀残页：%d张｜累计寻缘及古卷购买消耗：%d积分\n", w.Fragments, w.SeekingSpent)
	for _, f := range fs {
		status := "未掌控"
		if learned[f.ID] {
			status = "已掌控"
		}
		if f.ListingID != 0 {
			status += fmt.Sprintf("｜已上架 #%d", f.ListingID)
		}
		fmt.Fprintf(&b, "#%d %s｜%s阶｜+%d%%｜%s\n", f.ID, f.Name, spiritfire.GradeName(f.Grade), r.Percents[f.Grade], status)
	}
	fmt.Fprintf(&b, "\n永久掌控编号：")
	for _, m := range masters {
		fmt.Fprintf(&b, "%d ", m.FireID)
	}
	fmt.Fprintf(&b, "\n今日启用：%s｜合计%d%%\n次日搭配：%v\n", today.FireIDs, today.Percent, next)
	stat, ok, err := GetTodayDailyListeningStatChecked(user, now)
	if err != nil {
		return "", err
	}
	if ok {
		fmt.Fprintf(&b, "\n今日基础净修为：%.2f小时\n异火额外修为：%.2f小时\n最终净修为：%.2f小时\n", stat.BaseCultivationHours, stat.SpiritFireHours, stat.BaseCultivationHours+stat.SpiritFireHours)
	}
	b.WriteString("\n操作：\n异火寻缘\n购买焚诀 1\n合成焚诀\n学习异火 编号\n启用异火 编号 编号…\n启用异火 无\n上架异火 编号 单价\n回收异火 编号\n放弃异火 编号\n\n单火无需掌控；融合须每种都已掌控。搭配次日生效，掌控记录永久保留。")
	return b.String(), nil
}

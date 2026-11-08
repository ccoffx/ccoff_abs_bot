package titles

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var rankNoticeMutex sync.Mutex

func NotifyRankTitles(bot *tgbotapi.BotAPI, now time.Time) {
	started := time.Now()
	notifyRankTitles(bot, func() time.Time { return now.Add(time.Since(started)) })
}

func notifyRankTitles(bot *tgbotapi.BotAPI, currentTime func() time.Time) {
	if bot == nil {
		return
	}
	rankNoticeMutex.Lock()
	defer rankNoticeMutex.Unlock()
	// 请求已发出而进程中断时，发送结果不确定，保留档案提示但不重复发消息。
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("notice_state = ?", "sending").Update("notice_state", "uncertain").Error; err != nil {
		log.Printf("⚠️ 恢复称号提醒失败: %s", textfmt.FormatPlainError(err))
		return
	}
	var pending []model.RankTitleHistory
	if err := storage.DB.Where("notice_state = ? AND released_at IS NULL AND next_notice_at <= ?", "pending", currentTime()).Order("id").Limit(10).Find(&pending).Error; err != nil {
		log.Printf("⚠️ 读取称号提醒失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, h := range pending {
		d, ok := ByKey(h.TitleKey)
		if !ok {
			continue
		}
		// 提醒任务独立于归属刷新运行；发送前重新检查周期，避免换期或
		// 上一条网络请求跨过零点后，仍提醒用户获得上一期的当前榜首。
		if period := rankings.TitlePeriodKey(h.TitleKey, currentTime()); period != "" && h.PeriodKey != period {
			if err := storage.DB.Model(&model.RankTitleHistory{}).Where("id = ? AND notice_state = ?", h.ID, "pending").Update("notice_state", "obsolete").Error; err != nil {
				log.Printf("⚠️ 撤销过期称号提醒失败: %s", textfmt.FormatPlainError(err))
			}
			continue
		}
		claim := storage.DB.Model(&model.RankTitleHistory{}).Where("id = ? AND notice_state = ? AND released_at IS NULL", h.ID, "pending").Update("notice_state", "sending")
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		period := rankTitlePeriodText(h.TitleKey, h.PeriodKey)
		msg := tgbotapi.NewMessage(h.UserID, fmt.Sprintf("🎉 获得榜首称号：%s %s\n%s\n%s的荣誉已记入档案。当前持有时，可在「我的档案 → 我的称号」直接佩戴。交流群同时仅佩戴一个称号，获得新称号不会替换当前选择；失去后恢复境界称号。\n\n当前归属以称号页面为准。", d.Emoji, d.Name, period, d.Description))
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("查看并佩戴称号", "titles:home")))
		_, err := telegram.SendNoAutoDelete(bot, msg)
		state := "sent"
		next := time.Time{}
		if err != nil {
			state = "uncertain"
			var apiErr *tgbotapi.Error
			var apiValue tgbotapi.Error
			if errors.As(err, &apiErr) || errors.As(err, &apiValue) {
				if apiErr == nil {
					apiErr = &apiValue
				}
				state = "failed"
				if apiErr.Code == 429 || apiErr.Code >= 500 {
					state = "pending"
					delay := time.Minute
					if apiErr.RetryAfter > int(delay.Seconds()) {
						delay = time.Duration(apiErr.RetryAfter) * time.Second
					}
					next = currentTime().Add(delay)
				}
			}
			log.Printf("⚠️ 榜首称号提醒未送达: user=%d err=%s", h.UserID, textfmt.FormatTelegramSendError(err))
		}
		if err := storage.DB.Model(&model.RankTitleHistory{}).Where("id = ? AND notice_state = ?", h.ID, "sending").Updates(map[string]any{"notice_state": state, "next_notice_at": next}).Error; err != nil {
			log.Printf("⚠️ 保存称号提醒结果失败: %s", textfmt.FormatPlainError(err))
		}
	}
}

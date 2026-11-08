package titles

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

var tagSyncMutex sync.Mutex

func SyncTags(bot *tgbotapi.BotAPI, now time.Time) {
	started := time.Now()
	// 网络同步串行，避免同一成员旧版本的请求覆盖新版本。
	tagSyncMutex.Lock()
	defer tagSyncMutex.Unlock()
	var pending []model.TitleEquipment
	current := now.Add(time.Since(started))
	const dueCondition = "version > synced_version AND next_sync_at <= ? AND (rate_limit_until IS NULL OR rate_limit_until <= ?)"
	if err := storage.DB.Select("chat_id", "user_id").Where(dueCondition, current, current).Order("next_sync_at, chat_id, user_id").Limit(10).Find(&pending).Error; err != nil {
		log.Printf("⚠️ 读取称号同步任务失败: %s", textfmt.FormatPlainError(err))
		return
	}
	for _, target := range pending {
		// 批次只保留成员编号。前一条网络请求可能很慢，轮到本成员时
		// 必须重读最新佩戴与重试期限，不能重新写入排队期间已撤下的称号。
		current = now.Add(time.Since(started))
		var equipment model.TitleEquipment
		if err := storage.DB.Where("chat_id = ? AND user_id = ?", target.ChatID, target.UserID).Where(dueCondition, current, current).Take(&equipment).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				log.Printf("⚠️ 重读成员称号同步任务失败: %s", textfmt.FormatPlainError(err))
			}
			continue
		}
		desired := equipment.DesiredTag
		query := storage.DB.Model(&model.TitleEquipment{}).Where("chat_id = ? AND user_id = ?", equipment.ChatID, equipment.UserID)
		var unconfirmed []string
		if equipment.UnconfirmedTagsJSON != "" {
			if err := json.Unmarshal([]byte(equipment.UnconfirmedTagsJSON), &unconfirmed); err != nil {
				log.Printf("⚠️ 读取称号同步意图失败: %s", textfmt.FormatPlainError(err))
				continue
			}
		}
		// 发送前持久化意图。外部成功后本地确认失败时，撤下操作仍能识别该标签。
		if desired != "" {
			for _, candidate := range []string{equipment.AttemptedTag, desired} {
				found := candidate == ""
				for _, tag := range unconfirmed {
					found = found || tag == candidate
				}
				if !found {
					unconfirmed = append(unconfirmed, candidate)
				}
			}
			encoded, err := json.Marshal(unconfirmed)
			if err != nil {
				log.Printf("⚠️ 编码称号同步意图失败: %s", textfmt.FormatPlainError(err))
				continue
			}
			if err := query.Updates(map[string]any{"attempted_tag": desired, "unconfirmed_tags_json": string(encoded)}).Error; err != nil {
				log.Printf("⚠️ 保存称号同步意图失败: %s", textfmt.FormatPlainError(err))
				continue
			}
			equipment.AttemptedTag = desired
		}
		var err error
		if desired != "" || equipment.AppliedTag != "" || equipment.AttemptedTag != "" || len(unconfirmed) > 0 {
			previous := append(unconfirmed, equipment.AppliedTag, equipment.AttemptedTag)
			err = telegram.SyncRegularMemberTag(bot, equipment.ChatID, equipment.UserID, desired, previous...)
		}
		if err == nil {
			// 即使期望版本在请求期间变化，也记录实际写入结果；新版仍保持待同步。
			if updateErr := query.Updates(map[string]any{"synced_version": equipment.Version, "applied_tag": desired, "attempted_tag": "", "unconfirmed_tags_json": "", "sync_hint": "", "next_sync_at": time.Time{}}).Error; updateErr != nil {
				log.Printf("⚠️ 保存称号同步结果失败: %s", textfmt.FormatPlainError(updateErr))
			}
			continue
		}
		hint := "群标签同步未完成，请确认机器人拥有管理成员标签权限；后台会重试"
		delay := time.Minute
		if errors.Is(err, telegram.ErrMemberTagAdministrator) {
			hint = err.Error()
			delay = time.Hour
		}
		var apiErr *tgbotapi.Error
		var apiValue tgbotapi.Error
		if errors.As(err, &apiErr) || errors.As(err, &apiValue) {
			if apiErr == nil {
				apiErr = &apiValue
			}
			if apiErr.RetryAfter > int(delay.Seconds()) {
				delay = time.Duration(apiErr.RetryAfter) * time.Second
			}
		}
		// Telegram 的等待时间从失败响应算起，不能沿用本批次开始前的时刻。
		failedAt := now.Add(time.Since(started))
		if apiErr != nil && apiErr.Code == 429 && apiErr.RetryAfter > 0 {
			// 请求途中可能已切换佩戴；限流对新版本仍有效，不能带旧版本条件。
			if updateErr := query.Update("rate_limit_until", failedAt.Add(time.Duration(apiErr.RetryAfter)*time.Second)).Error; updateErr != nil {
				log.Printf("⚠️ 保存群标签限流截止时间失败: %s", textfmt.FormatPlainError(updateErr))
			}
		}
		if updateErr := query.Where("version = ?", equipment.Version).Updates(map[string]any{"sync_hint": hint, "next_sync_at": failedAt.Add(delay)}).Error; updateErr != nil {
			log.Printf("⚠️ 保存称号同步重试失败: %s", textfmt.FormatPlainError(updateErr))
		}
		log.Printf("⚠️ 群称号标签待重试: chat=%d user=%d err=%s", equipment.ChatID, equipment.UserID, textfmt.FormatTelegramSendError(err))
	}
}

func startTitleTask(name string, task func()) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("⚠️ 称号%s巡检异常: %s", name, textfmt.FormatPlainValue(r))
					}
				}()
				task()
			}()
			<-ticker.C
		}
	}()
}

func StartScheduler(bot *tgbotapi.BotAPI) {
	// 归属计算与两类网络任务独立，Telegram 超时不会暂停榜首转移或境界刷新。
	var lastRefresh time.Time
	startTitleTask("归属", func() {
		now := time.Now()
		if now.Sub(lastRefresh) < 30*time.Second {
			return
		}
		rankErr := RefreshRankTitles(now)
		if rankErr != nil {
			log.Printf("⚠️ 刷新榜首称号失败: %s", textfmt.FormatPlainError(rankErr))
		}
		if err := RefreshDefaults(); err != nil {
			log.Printf("⚠️ 刷新默认境界称号失败: %s", textfmt.FormatPlainError(err))
		} else if rankErr == nil {
			lastRefresh = now
		}
	})
	startTitleTask("标签", func() { SyncTags(bot, time.Now()) })
	startTitleTask("提醒", func() { NotifyRankTitles(bot, time.Now()) })
}

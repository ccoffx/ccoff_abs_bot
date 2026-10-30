package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
)

func createWorldBossEventRecord(db *gorm.DB, event *model.WorldBossEvent) error {
	if db == nil || event == nil {
		return fmt.Errorf("WORLD_BOSS_EVENT_INVALID")
	}
	event.BossID = textfmt.FormatPlainValue(event.BossID)
	event.Name = textfmt.FormatPlainValue(event.Name)
	event.Status = textfmt.FormatPlainValue(event.Status)
	return db.Transaction(func(tx *gorm.DB) error {
		if err := spiritfire.PrepareEvent(tx, "boss:"+event.BossID, event.StartAt, event.ChatID); err != nil {
			return err
		}
		res := tx.Create(event)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("WORLD_BOSS_EVENT_CREATE_MISSED")
		}
		return nil
	})
}

func createWorldBossParticipantInTx(db *gorm.DB, participant *model.WorldBossParticipant) (bool, error) {
	if db == nil || participant == nil {
		return false, fmt.Errorf("WORLD_BOSS_PARTICIPANT_INVALID")
	}
	entry := *participant
	entry.BossID = textfmt.FormatPlainValue(entry.BossID)
	entry.UserName = textfmt.FormatPlainValue(entry.UserName)
	res := db.Clauses(clause.OnConflict{
		DoNothing: true,
	}).Create(&entry)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	*participant = entry
	return true, nil
}

const (
	worldBossName      = "域外心魔"
	worldBossStartHour = 21
	worldBossEndHour   = 22

	worldBossBaseHP           = 100
	worldBossMinHP            = 120
	worldBossMaxHP            = 450
	worldBossHPPerParticipant = 12

	worldBossActualDamagePerHour      = 30.0
	worldBossValidDamage              = 3.0
	worldBossCultivationBonusPerStage = 0.01
	worldBossCultivationBonusCap      = 0.25
	worldBossLiveRefreshInterval      = 2 * time.Minute
	worldBossJoinCloseBeforeEnd       = 15 * time.Minute
	worldBossSettlementStaleAfter     = 30 * time.Minute
	worldBossRedPacketTotalPoints     = 30
	worldBossRedPacketCount           = 10
)

func worldBossPointDescriptionName(name string) string {
	return textfmt.LotteryDisplayText(name, 80, "-")
}

var (
	lastWorldBossStartKey string
	worldBossRankOrder    = "damage DESC, delta_hours DESC, created_at ASC"
	// worldBossLiveRefreshMu 串行化实时伤害刷新：调度器、Boss状态/Boss排行/参加Boss 都会触发，
	// 每轮刷新按参与人数对 ABS 逐个拉取听书时长并回写伤害/血量，重入会导致重复 ABS 请求与伤害重复写库。
	worldBossLiveRefreshMu sync.Mutex
)

func isWorldBossOpenDay(weekday time.Weekday) bool {
	return weekday == time.Saturday || weekday == time.Sunday
}

func StartWorldBossScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for now := range ticker.C {
			if config.AppConfig.NoticeGroupID == 0 {
				continue
			}

			local := now.In(time.FixedZone("CST", 8*3600))
			dayKey := local.Format("2006-01-02")

			if isWorldBossOpenDay(local.Weekday()) && local.Hour() == worldBossStartHour && local.Minute() == 0 && lastWorldBossStartKey != dayKey {
				lastWorldBossStartKey = dayKey
				go startWorldBossEvent(bot, config.AppConfig.NoticeGroupID, local)
			}

			go refreshActiveWorldBosses(bot, local)
			go settleExpiredWorldBosses(bot, local)
		}
	}()

	log.Println("✅ 世界Boss调度器已启动：每周六、周日 21:00-22:00")
}

func HandleWorldBossCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil {
		return false
	}

	text = strings.TrimSpace(text)
	if text != "世界Boss" && text != "Boss状态" && text != "参加Boss" && text != "Boss排行" {
		return false
	}

	telegram.RegisterIncomingGroupCommandForAutoDelete(msg)

	switch text {
	case "世界Boss", "Boss状态":
		handleWorldBossStatus(bot, msg)
	case "参加Boss":
		handleJoinWorldBoss(bot, msg)
	case "Boss排行":
		handleWorldBossRank(bot, msg)
	}

	return true
}

func startWorldBossEvent(bot *tgbotapi.BotAPI, chatID int64, now time.Time) {
	startAt := time.Date(now.Year(), now.Month(), now.Day(), worldBossStartHour, 0, 0, 0, now.Location())
	endAt := time.Date(now.Year(), now.Month(), now.Day(), worldBossEndHour, 0, 0, 0, now.Location())
	bossID := "WB-" + startAt.Format("20060102")

	exists, err := worldBossEventExists(bossID)
	if err != nil {
		log.Printf("❌ 查询世界Boss是否已存在失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
		return
	}
	if exists {
		return
	}

	maxHP := calculateWorldBossMaxHP(0)

	event := model.WorldBossEvent{
		BossID:    bossID,
		Name:      worldBossName,
		ChatID:    chatID,
		Status:    "active",
		StartAt:   startAt,
		EndAt:     endAt,
		MaxHP:     maxHP,
		CurrentHP: float64(maxHP),
	}

	if err := createWorldBossEventRecord(storage.DB, &event); err != nil {
		log.Printf("❌ 创建世界Boss失败: %s", textfmt.FormatPlainError(err))
		return
	}

	notice := fmt.Sprintf(
		"🌑 **【世界Boss降临】** 🌑\n\n"+
			"域外心魔撕裂天幕，诸位道友请共同听书镇压！\n\n"+
			"Boss：**%s**\n"+
			"时间：`21:00 - 22:00`\n"+
			"血量：`%.2f/%d`（基础 `%d` + 每名参与者 `%d`，最低 `%d`，最高 `%d`）\n\n"+
			"发送 `参加Boss` 记录当前实际听书时长。\n"+
			"最后 `%d` 分钟停止新道友加入。\n"+
			"结算伤害 = 参与后 Boss 时段实际听书小时 × `%.0f` ×（1 + 修为加成 + 宗门科技）。\n"+
			"修为加成：斗者初期 `+1%%`，每小段 `+1%%`，最高 `+25%%`。\n\n"+
			"发送 `Boss状态` 查看进度，`Boss排行` 查看伤害榜。",
		textfmt.EscapeMarkdown(event.Name),
		event.CurrentHP,
		event.MaxHP,
		worldBossBaseHP,
		worldBossHPPerParticipant,
		worldBossMinHP,
		worldBossMaxHP,
		int(worldBossJoinCloseBeforeEnd.Minutes()),
		worldBossActualDamagePerHour,
	)

	telegram.SendGroupAutoDeleteMessage(bot, chatID, notice)
	ensureWorldBossLiveBoard(bot, event)
}

func worldBossEventExists(bossID string) (bool, error) {
	bossID = strings.TrimSpace(bossID)
	if bossID == "" {
		return false, fmt.Errorf("INVALID_WORLD_BOSS_ID")
	}
	var existing model.WorldBossEvent
	if err := storage.DB.Where("boss_id = ?", bossID).First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func calculateWorldBossMaxHP(participantCount int) int {
	if participantCount < 0 {
		participantCount = 0
	}

	maxHP := worldBossBaseHP + participantCount*worldBossHPPerParticipant
	if maxHP < worldBossMinHP {
		maxHP = worldBossMinHP
	}
	if maxHP > worldBossMaxHP {
		maxHP = worldBossMaxHP
	}
	return maxHP
}

func worldBossJoinDeadline(event model.WorldBossEvent) time.Time {
	return event.EndAt.Add(-worldBossJoinCloseBeforeEnd)
}

func canJoinWorldBossAt(event model.WorldBossEvent, now time.Time) bool {
	if event.BossID == "" || event.Status != "active" {
		return false
	}
	if now.Before(event.StartAt) || !now.Before(event.EndAt) {
		return false
	}
	return now.Before(worldBossJoinDeadline(event))
}

func refreshWorldBossStoredHPByParticipants(bossID string) (model.WorldBossEvent, error) {
	var event model.WorldBossEvent
	if err := storage.DB.Where("boss_id = ? AND status = ?", bossID, "active").First(&event).Error; err != nil {
		return event, err
	}

	var participantCount int64
	if err := storage.DB.Model(&model.WorldBossParticipant{}).
		Where("boss_id = ?", bossID).
		Count(&participantCount).Error; err != nil {
		return event, err
	}

	var totalDamage float64
	if err := storage.DB.Model(&model.WorldBossParticipant{}).
		Where("boss_id = ?", bossID).
		Select("COALESCE(SUM(damage), 0)").
		Scan(&totalDamage).Error; err != nil {
		return event, err
	}
	totalDamage = roundWorldBossDamage(totalDamage)

	maxHP := calculateWorldBossMaxHP(int(participantCount))
	currentHP := float64(maxHP) - totalDamage
	if currentHP < 0 {
		currentHP = 0
	} else {
		currentHP = roundWorldBossDamage(currentHP)
	}
	isKilled := totalDamage >= float64(maxHP)

	res := storage.DB.Model(&model.WorldBossEvent{}).
		Where("boss_id = ? AND status = ?", bossID, "active").
		Updates(map[string]interface{}{
			"max_hp":            maxHP,
			"current_hp":        currentHP,
			"is_killed":         isKilled,
			"participant_count": int(participantCount),
		})
	if res.Error != nil {
		return event, res.Error
	}
	if res.RowsAffected == 0 {
		return event, fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
	}

	event.MaxHP = maxHP
	event.CurrentHP = currentHP
	event.IsKilled = isKilled
	event.ParticipantCount = int(participantCount)
	return event, nil
}

func settleExpiredWorldBosses(bot *tgbotapi.BotAPI, now time.Time) {
	recoverStaleWorldBossSettlements(now)

	var events []model.WorldBossEvent
	if err := storage.DB.Where("status = ? AND end_at <= ?", "active", now).Find(&events).Error; err != nil {
		log.Printf("⚠️ 查询到期世界Boss失败，已跳过本轮结算扫描: now=%s err=%s", textfmt.FormatPlainValue(now.Format(time.RFC3339)), textfmt.FormatPlainError(err))
		return
	}

	for _, event := range events {
		settleWorldBoss(bot, event.BossID)
	}
}

func refreshActiveWorldBosses(bot *tgbotapi.BotAPI, now time.Time) {
	recoverStaleWorldBossSettlements(now)

	var events []model.WorldBossEvent
	if err := storage.DB.Where("status = ? AND start_at <= ? AND end_at > ?", "active", now, now).Find(&events).Error; err != nil {
		log.Printf("⚠️ 查询进行中世界Boss失败，已跳过本轮实时刷新: now=%s err=%s", textfmt.FormatPlainValue(now.Format(time.RFC3339)), textfmt.FormatPlainError(err))
		return
	}

	for _, event := range events {
		if time.Since(event.UpdatedAt) < worldBossLiveRefreshInterval {
			continue
		}
		refreshed, ran, err := tryRefreshWorldBossLiveDamage(event)
		if err != nil {
			log.Printf("⚠️ 世界Boss实时伤害刷新失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
			continue
		}
		if !ran {
			continue
		}
		ensureWorldBossLiveBoard(bot, refreshed)
		if refreshed.IsKilled {
			settleWorldBoss(bot, refreshed.BossID)
		}
	}
}

func recoverStaleWorldBossSettlements(now time.Time) {
	if storage.DB == nil {
		return
	}
	cutoff := now.Add(-worldBossSettlementStaleAfter)
	res := storage.DB.Model(&model.WorldBossEvent{}).
		Where("status = ? AND updated_at < ? AND settled_at IS NULL", "settling", cutoff).
		Update("status", "active")
	if res.Error != nil {
		log.Printf("世界Boss settling 恢复扫描失败: cutoff=%s err=%s", textfmt.FormatPlainValue(cutoff.Format(time.RFC3339)), textfmt.FormatPlainError(res.Error))
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("世界Boss settling 已恢复为 active: count=%d cutoff=%s", res.RowsAffected, textfmt.FormatPlainValue(cutoff.Format(time.RFC3339)))
	}
}

func refreshWorldBossLiveDamage(event model.WorldBossEvent) (model.WorldBossEvent, float64, error) {
	// 用户触发路径（Boss状态/Boss排行/参加Boss）使用阻塞锁：等待在途刷新完成后重算，
	// 保证参加后立即看到最新血量；调度器走 tryRefreshWorldBossLiveDamage，忙时跳过。
	worldBossLiveRefreshMu.Lock()
	defer worldBossLiveRefreshMu.Unlock()
	return refreshWorldBossLiveDamageLocked(event)
}

// tryRefreshWorldBossLiveDamage 调度器专用：已有刷新在途时直接跳过，等待下一轮 tick，避免重复 ABS 拉取。
// 返回 ran=false 表示本轮未执行，调用方不得基于旧数据刷新战榜或结算。
func tryRefreshWorldBossLiveDamage(event model.WorldBossEvent) (model.WorldBossEvent, bool, error) {
	if !worldBossLiveRefreshMu.TryLock() {
		return event, false, nil
	}
	defer worldBossLiveRefreshMu.Unlock()
	refreshed, _, err := refreshWorldBossLiveDamageLocked(event)
	return refreshed, true, err
}

func refreshWorldBossLiveDamageLocked(event model.WorldBossEvent) (model.WorldBossEvent, float64, error) {
	if event.BossID == "" || event.Status != "active" {
		return event, 0, nil
	}

	var participants []model.WorldBossParticipant
	if err := storage.DB.Where("boss_id = ? AND deleted_at IS NULL", event.BossID).Find(&participants).Error; err != nil {
		return event, 0, err
	}

	totalDamage := 0.0
	maxHP := calculateWorldBossMaxHP(len(participants))
	for i := range participants {
		p := &participants[i]

		var u model.User
		if err := storage.DB.Where("telegram_id = ?", p.UserID).First(&u).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				log.Printf("⚠️ 世界Boss实时读取本地档案失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), p.UserID, textfmt.FormatPlainError(err))
			}
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := clearWorldBossParticipantComputedDamage(*p, "user_not_found"); err != nil {
					return event, totalDamage, err
				}
			} else {
				totalDamage += p.Damage
			}
			continue
		}
		if u.AbsUserID == "" {
			if err := clearWorldBossParticipantComputedDamage(*p, "abs_unbound"); err != nil {
				return event, totalDamage, err
			}
			continue
		}

		finalHours, err := getWorldBossRawListeningHours(u.AbsUserID)
		if err != nil {
			log.Printf("⚠️ 世界Boss实时读取ABS失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), p.UserID, textfmt.FormatPlainError(err))
			totalDamage += p.Damage
			continue
		}

		maxDeltaHours := calculateWorldBossDamageWindowHours(*p, event, time.Now())
		damage, baseDamage, multiplier := calculateWorldBossDamage(p.BaseHours, finalHours, maxDeltaHours)
		damage, multiplier, err = ApplyWorldBossDamageBonusesChecked(p.UserID, baseDamage)
		if err != nil {
			log.Printf("⚠️ 世界Boss实时读取伤害加成失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), p.UserID, textfmt.FormatPlainError(err))
			totalDamage += p.Damage
			continue
		}

		deltaHours := math.Max(0, finalHours-p.BaseHours)
		if deltaHours > maxDeltaHours {
			deltaHours = maxDeltaHours
		}
		totalDamage += damage

		damageRes := storage.DB.Model(&model.WorldBossParticipant{}).
			Where("id = ? AND boss_id = ? AND user_id = ?", p.ID, event.BossID, p.UserID).
			Updates(map[string]interface{}{
				"final_hours": finalHours,
				"delta_hours": deltaHours,
				"base_damage": baseDamage,
				"multiplier":  multiplier,
				"damage":      damage,
			})
		if damageRes.Error != nil {
			log.Printf("⚠️ 世界Boss实时伤害写入失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), p.UserID, textfmt.FormatPlainError(damageRes.Error))
		} else if damageRes.RowsAffected == 0 {
			log.Printf("⚠️ 世界Boss实时伤害写入未命中: boss=%s user=%d participant=%d", textfmt.FormatPlainValue(event.BossID), p.UserID, p.ID)
		}
	}
	totalDamage = roundWorldBossDamage(totalDamage)

	currentHP := float64(maxHP) - totalDamage
	if currentHP < 0 {
		currentHP = 0
	} else {
		currentHP = roundWorldBossDamage(currentHP)
	}
	isKilled := totalDamage >= float64(maxHP)

	event.MaxHP = maxHP
	event.CurrentHP = currentHP
	event.IsKilled = isKilled
	event.ParticipantCount = len(participants)

	eventRes := storage.DB.Model(&model.WorldBossEvent{}).
		Where("boss_id = ? AND status = ?", event.BossID, "active").
		Updates(map[string]interface{}{
			"max_hp":            maxHP,
			"current_hp":        currentHP,
			"is_killed":         isKilled,
			"participant_count": len(participants),
		})
	if eventRes.Error != nil {
		return event, totalDamage, eventRes.Error
	}
	if eventRes.RowsAffected == 0 {
		return event, totalDamage, fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
	}

	return event, totalDamage, nil
}

func settleWorldBoss(bot *tgbotapi.BotAPI, bossID string) {
	res := storage.DB.Model(&model.WorldBossEvent{}).
		Where("boss_id = ? AND status = ?", bossID, "active").
		Update("status", "settling")
	if res.Error != nil || res.RowsAffected == 0 {
		return
	}

	var event model.WorldBossEvent
	if err := storage.DB.Where("boss_id = ?", bossID).First(&event).Error; err != nil {
		resetWorldBossToActive(bossID, err)
		return
	}

	var participants []model.WorldBossParticipant
	if err := storage.DB.Where("boss_id = ?", bossID).Find(&participants).Error; err != nil {
		resetWorldBossToActive(bossID, err)
		return
	}
	event.MaxHP = calculateWorldBossMaxHP(len(participants))

	totalDamage := 0.0
	rewardParticipants := make([]model.WorldBossParticipant, 0, len(participants))
	for i := range participants {
		p := &participants[i]

		var u model.User
		if err := storage.DB.Where("telegram_id = ?", p.UserID).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := clearWorldBossParticipantComputedDamage(*p, "user_not_found"); err != nil {
					resetWorldBossToActive(bossID, err)
					return
				}
				continue
			}
			resetWorldBossToActive(bossID, err)
			return
		}
		if u.AbsUserID == "" {
			if err := clearWorldBossParticipantComputedDamage(*p, "abs_unbound"); err != nil {
				resetWorldBossToActive(bossID, err)
				return
			}
			continue
		}

		finalHours, err := getWorldBossRawListeningHours(u.AbsUserID)
		if err != nil {
			// ABS can be temporarily unavailable during settlement; keep the last live damage so rewards are not dropped.
			log.Printf("world boss settlement ABS read failed, keeping recorded damage: boss=%s user=%d damage=%.2f err=%s", textfmt.FormatPlainValue(bossID), p.UserID, p.Damage, textfmt.FormatPlainError(err))
			totalDamage += p.Damage
			rewardParticipants = append(rewardParticipants, *p)
			continue
		}

		// Use current wall-clock time; the helper clamps the window to the event end time.
		maxDeltaHours := calculateWorldBossDamageWindowHours(*p, event, time.Now())
		damage, baseDamage, multiplier := calculateWorldBossDamage(p.BaseHours, finalHours, maxDeltaHours)
		damage, multiplier, err = ApplyWorldBossDamageBonusesChecked(p.UserID, baseDamage)
		if err != nil {
			resetWorldBossToActive(bossID, err)
			return
		}

		p.FinalHours = finalHours
		p.DeltaHours = math.Max(0, finalHours-p.BaseHours)
		if p.DeltaHours > maxDeltaHours {
			p.DeltaHours = maxDeltaHours
		}
		p.BaseDamage = baseDamage
		p.Multiplier = multiplier
		p.Damage = damage

		totalDamage += damage
		damageRes := storage.DB.Model(&model.WorldBossParticipant{}).
			Where("id = ? AND boss_id = ? AND user_id = ?", p.ID, bossID, p.UserID).
			Updates(map[string]interface{}{
				"final_hours": finalHours,
				"delta_hours": p.DeltaHours,
				"base_damage": baseDamage,
				"multiplier":  multiplier,
				"damage":      damage,
			})
		if damageRes.Error != nil {
			resetWorldBossToActive(bossID, damageRes.Error)
			return
		}
		if damageRes.RowsAffected == 0 {
			resetWorldBossToActive(bossID, fmt.Errorf("WORLD_BOSS_PARTICIPANT_DAMAGE_UPDATE_MISSED"))
			return
		}
		rewardParticipants = append(rewardParticipants, *p)
	}
	totalDamage = roundWorldBossDamage(totalDamage)

	killed := totalDamage >= float64(event.MaxHP)
	currentHP := float64(event.MaxHP) - totalDamage
	if currentHP < 0 {
		currentHP = 0
	} else {
		currentHP = roundWorldBossDamage(currentHP)
	}

	if err := grantWorldBossRewards(event, rewardParticipants, killed, currentHP); err != nil {
		log.Printf("❌ 世界Boss奖励结算失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
		resetWorldBossToActive(bossID, err)
		return
	}

	event.Status = "settled"
	event.CurrentHP = currentHP
	event.IsKilled = killed
	event.ParticipantCount = len(participants)

	if err := storage.DB.Where("boss_id = ?", bossID).First(&event).Error; err == nil {
		ensureWorldBossLiveBoard(bot, event)
	} else {
		log.Printf("⚠️ 世界Boss结算后事件重读失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
	}
	sendWorldBossSettlement(bot, event, killed, totalDamage)
}

func clearWorldBossParticipantComputedDamage(p model.WorldBossParticipant, reason string) error {
	if p.ID == 0 || p.BossID == "" {
		return fmt.Errorf("WORLD_BOSS_PARTICIPANT_INVALID")
	}
	res := storage.DB.Model(&model.WorldBossParticipant{}).
		Where("id = ? AND boss_id = ? AND user_id = ?", p.ID, p.BossID, p.UserID).
		Updates(map[string]interface{}{
			"final_hours": p.BaseHours,
			"delta_hours": 0,
			"base_damage": 0,
			"multiplier":  0,
			"damage":      0,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("WORLD_BOSS_PARTICIPANT_CLEAR_DAMAGE_MISSED")
	}
	log.Printf("world boss participant computed damage cleared: boss=%s user=%d reason=%s", textfmt.FormatPlainValue(p.BossID), p.UserID, textfmt.FormatPlainValue(reason))
	return nil
}

func resetWorldBossToActive(bossID string, reason error) {
	if reason != nil {
		log.Printf("⚠️ world boss settlement rolling back to active: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(reason))
	}
	res := storage.DB.Model(&model.WorldBossEvent{}).
		Where("boss_id = ? AND status = ?", bossID, "settling").
		Update("status", "active")
	if res.Error != nil {
		log.Printf("⚠️ world boss settlement rollback failed: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("⚠️ world boss settlement rollback missed active reset: boss=%s", textfmt.FormatPlainValue(bossID))
	}
}

func preciseWorldBossDamage(p model.WorldBossParticipant) float64 {
	if p.Damage < 0 {
		return 0
	}
	return p.Damage
}

func worldBossParticipantRankLess(left model.WorldBossParticipant, right model.WorldBossParticipant) bool {
	if left.Damage != right.Damage {
		return left.Damage > right.Damage
	}
	leftDamage := preciseWorldBossDamage(left)
	rightDamage := preciseWorldBossDamage(right)
	if leftDamage != rightDamage {
		return leftDamage > rightDamage
	}
	if left.DeltaHours != right.DeltaHours {
		return left.DeltaHours > right.DeltaHours
	}
	return left.CreatedAt.Before(right.CreatedAt)
}

func newWorldBossRedPacket(packetID string, event model.WorldBossEvent, now time.Time) model.RedPacket {
	return model.RedPacket{
		ID:          packetID,
		SenderID:    0,
		SenderName:  "世界Boss",
		TotalPoints: worldBossRedPacketTotalPoints,
		Count:       worldBossRedPacketCount,
		LeftCount:   worldBossRedPacketCount,
		LeftPoints:  worldBossRedPacketTotalPoints,
		CreatedAt:   now,
		RefType:     "world_boss",
		RefID:       event.BossID,
		ClaimScope:  storage.RedPacketClaimScopeWorldBossParticipant,
	}
}

func createWorldBossRedPacketInTx(tx *gorm.DB, packet *model.RedPacket) error {
	if tx == nil || packet == nil {
		return fmt.Errorf("WORLD_BOSS_REDPACKET_INVALID")
	}
	packet.ID = textfmt.FormatPlainValue(packet.ID)
	packet.SenderName = textfmt.FormatPlainValue(packet.SenderName)
	packet.RefType = textfmt.FormatPlainValue(packet.RefType)
	packet.RefID = textfmt.FormatPlainValue(packet.RefID)
	packet.ClaimScope = textfmt.FormatPlainValue(packet.ClaimScope)
	res := tx.Create(packet)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("WORLD_BOSS_REDPACKET_CREATE_MISSED")
	}
	return nil
}

func grantWorldBossRewards(event model.WorldBossEvent, participants []model.WorldBossParticipant, killed bool, currentHP float64) error {
	sort.Slice(participants, func(i, j int) bool {
		return worldBossParticipantRankLess(participants[i], participants[j])
	})

	// 击杀时的天道奖池 +10 注入必须与结算同事务、受 status='settling'→'settled'
	// 状态闸门保护，避免“已记结算但奖池注入因进程崩溃丢失”或后续重发结算
	// 消息导致重复注入。按项目锁序要求：先持 fusionPoolMutex，再开事务。
	return storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		if killed {
			packetID := "HB-WB-" + security.GenerateRandomCode(8)
			packet := newWorldBossRedPacket(packetID, event, time.Now())
			if err := createWorldBossRedPacketInTx(tx, &packet); err != nil {
				return err
			}
		}

		for idx, p := range participants {
			if p.Damage < worldBossValidDamage || p.IsRewarded {
				continue
			}

			points := 0
			contribution := 0
			prestige := 0

			if killed {
				points += 1
				contribution += 3

				switch idx {
				case 0:
					points += 6
					contribution += 10
					prestige += 3
				case 1:
					points += 4
					contribution += 7
					prestige += 2
				case 2:
					points += 2
					contribution += 5
					prestige += 1
				default:
					if idx < 10 {
						contribution += 3
					}
				}
			} else {
				contribution += 1
				if idx == 0 {
					contribution += 3
				} else if idx == 1 {
					contribution += 2
				} else if idx == 2 {
					contribution += 1
				}
			}

			if points > 0 {
				if err := storage.ApplyPointDeltaInTx(
					tx,
					p.UserID,
					points,
					"world_boss_reward",
					fmt.Sprintf("世界Boss【%s】奖励，伤害 %.2f", worldBossPointDescriptionName(event.Name), p.Damage),
					"world_boss",
					event.BossID,
				); err != nil {
					return err
				}
			}

			if contribution > 0 || prestige > 0 {
				if err := awardWorldBossSectRewardTx(tx, p.UserID, contribution, prestige, event.BossID); err != nil {
					return err
				}
			}

			// 至宝掉落：斗王及以上的参与者按当前大境界概率掉落“突破到下一境”的至宝。
			// 斗帝已封顶、斗之气~斗灵无至宝，rollWorldBossTreasureDropInTx 内部会直接跳过。
			if err := rollWorldBossTreasureDropInTx(tx, p.UserID); err != nil {
				return err
			}

			res := tx.Model(&model.WorldBossParticipant{}).
				Where("id = ? AND is_rewarded = ?", p.ID, false).
				Updates(map[string]interface{}{
					"reward_points":       points,
					"reward_contribution": contribution,
					"reward_prestige":     prestige,
					"is_rewarded":         true,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("WORLD_BOSS_PARTICIPANT_REWARD_MARK_MISSED")
			}
		}

		if killed {
			ids := []int64{}
			for _, p := range participants {
				if p.Damage >= worldBossValidDamage {
					ids = append(ids, p.UserID)
				}
			}
			if err := spiritfire.AwardActivityInTx(tx, "boss:"+event.BossID, ids, true, event.StartAt, time.Now()); err != nil {
				return err
			}
		}
		settledAt := time.Now()
		res := tx.Model(&model.WorldBossEvent{}).
			Where("boss_id = ? AND status = ?", event.BossID, "settling").
			Updates(map[string]interface{}{
				"status":            "settled",
				"max_hp":            event.MaxHP,
				"current_hp":        currentHP,
				"is_killed":         killed,
				"participant_count": len(participants),
				"settled_at":        &settledAt,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("WORLD_BOSS_STATUS_CHANGED")
		}

		// 击杀奖励：天道奖池 +10，与结算同事务提交，受状态闸门保护。
		if killed {
			if _, _, err := storage.AddPointsToFusionPoolInTx(tx, 10); err != nil {
				return err
			}
		}

		return nil
	})
}

// rollWorldBossTreasureDropInTx 世界Boss结算时，按参与者当前大境界概率掉落对应“突破到下一境”的至宝。
// 无斗气档案、斗之气~斗灵（无至宝）、斗帝（已封顶）均可直接跳过；掉落累加入 Inventory。
func rollWorldBossTreasureDropInTx(tx *gorm.DB, userID int64) error {
	if tx == nil || userID == 0 {
		return nil
	}
	var cul model.Cultivation
	if err := tx.Where("user_id = ?", userID).First(&cul).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	treasure := CultivationTreasureNameForRealm(cul.MajorRealm)
	if treasure == "" {
		return nil
	}
	if rand.Intn(100) < cultivationTreasureDropRatePercent() {
		return GardenGrantInventoryInTx(tx, userID, treasure, 1)
	}
	return nil
}

func awardWorldBossSectRewardTx(tx *gorm.DB, userID int64, contribution int, prestige int, bossID string) error {
	var member model.SectMember
	if err := tx.Where("user_id = ?", userID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	if contribution > 0 {
		res := tx.Model(&model.SectMember{}).
			Where("id = ?", member.ID).
			Updates(map[string]interface{}{
				"contribution":        gorm.Expr("contribution + ?", contribution),
				"weekly_contribution": gorm.Expr("weekly_contribution + ?", contribution),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("WORLD_BOSS_SECT_MEMBER_REWARD_MISSED")
		}
	}

	if prestige > 0 {
		res := tx.Model(&model.Sect{}).
			Where("id = ?", member.SectID).
			UpdateColumn("prestige", gorm.Expr("prestige + ?", prestige))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("WORLD_BOSS_SECT_PRESTIGE_REWARD_MISSED")
		}
	}

	delta := contribution
	if delta == 0 {
		delta = prestige
	}

	var updatedMember model.SectMember
	if err := tx.Select("id", "contribution").
		Where("id = ?", member.ID).
		First(&updatedMember).Error; err != nil {
		return err
	}

	return createSectContributionLogInTx(tx, &model.SectContributionLog{
		SectID:       member.SectID,
		UserID:       userID,
		Delta:        delta,
		Reason:       fmt.Sprintf("世界Boss奖励：贡献 +%d，声望 +%d", contribution, prestige),
		RefType:      "world_boss",
		RefID:        bossID,
		BalanceAfter: updatedMember.Contribution,
	})
}

func handleJoinWorldBoss(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	event, eventErr := getActiveWorldBossChecked()
	if errors.Is(eventErr, gorm.ErrRecordNotFound) {
		telegram.ReplyTextToMessage(bot, msg, "📜 当前没有开放中的世界Boss。\n\n开放时间：每周六、周日 `21:00 - 22:00`。")
		return
	}
	if eventErr != nil {
		log.Printf("⚠️ 世界Boss参加活动读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(eventErr))
		telegram.ReplyTextToMessage(bot, msg, "❌ 世界Boss状态读取失败，请稍后重试。")
		return
	}
	now := time.Now()
	if !canJoinWorldBossAt(event, now) {
		telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf(
			"⚠️ 本期世界Boss已进入最后 `%d` 分钟冲刺阶段，停止新道友加入。\n\n请等待下一场 Boss 降临。",
			int(worldBossJoinCloseBeforeEnd.Minutes()),
		))
		return
	}

	var u model.User
	if err := storage.DB.Where("telegram_id = ?", msg.From.ID).First(&u).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("⚠️ 世界Boss参加读取本地档案失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(err))
			telegram.ReplyTextToMessage(bot, msg, "❌ 参加世界Boss读取本地档案失败，请稍后重试。")
			return
		}
		telegram.ReplyTextToMessage(bot, msg, "❌ 参加世界Boss需要先绑定有效 ABS 听书账号。")
		return
	}
	if u.AbsUserID == "" {
		telegram.ReplyTextToMessage(bot, msg, "❌ 参加世界Boss需要先绑定有效 ABS 听书账号。")
		return
	}
	usable, statusErr := storage.UserHasUsableLocalAbsAccountAt(u, now)
	if statusErr != nil {
		log.Printf("⚠️ 世界Boss参加状态读取失败: user=%d abs=%s err=%s", msg.From.ID, textfmt.FormatPlainValue(u.AbsUserID), textfmt.FormatPlainError(statusErr))
		telegram.ReplyTextToMessage(bot, msg, "❌ 账号状态读取失败，请稍后重试。")
		return
	}
	if !usable {
		telegram.ReplyTextToMessage(bot, msg, "❌ 参加世界Boss需要当前有效且未暂停的 ABS 听书账号。")
		return
	}

	baseHours, err := getWorldBossRawListeningHours(u.AbsUserID)
	if err != nil {
		telegram.ReplyTextToMessage(bot, msg, "❌ 读取当前实际听书时长失败，请稍后重试。")
		return
	}

	userName := textfmt.GetTelegramDisplayName(msg.From)
	_, err = createWorldBossParticipantInTx(storage.DB, &model.WorldBossParticipant{
		BossID:    event.BossID,
		UserID:    msg.From.ID,
		UserName:  userName,
		BaseHours: baseHours,
	})
	if err != nil {
		telegram.ReplyTextToMessage(bot, msg, "❌ 参加失败，请稍后重试。")
		return
	}

	var p model.WorldBossParticipant
	baseHoursText := fmt.Sprintf("`%.1f` 小时", baseHours)
	if err := storage.DB.Where("boss_id = ? AND user_id = ?", event.BossID, msg.From.ID).First(&p).Error; err != nil {
		log.Printf("⚠️ 世界Boss参与记录读取失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), msg.From.ID, textfmt.FormatPlainError(err))
		baseHoursText = "`读取失败`"
	} else {
		baseHoursText = fmt.Sprintf("`%.1f` 小时", p.BaseHours)
	}
	if refreshed, err := refreshWorldBossStoredHPByParticipants(event.BossID); err == nil {
		event = refreshed
	} else {
		log.Printf("⚠️ 世界Boss参与后血量刷新失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), msg.From.ID, textfmt.FormatPlainError(err))
	}

	ensureWorldBossLiveBoard(bot, event)

	telegram.ReplyTextToMessage(bot, msg, fmt.Sprintf(
		"✅ 已加入世界Boss讨伐。\n\nBoss：**%s**\n当前基线实际听书：%s\n当前血量：`%.2f/%d`\n参与人数：`%d`\n\n请在 `22:00` 前实际听书，结算时仅按参与后的 Boss 时段实际听书时长造成伤害。",
		textfmt.EscapeMarkdown(event.Name),
		baseHoursText,
		event.CurrentHP,
		event.MaxHP,
		event.ParticipantCount,
	))
}

func handleWorldBossStatus(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	event, eventErr := getActiveOrLatestWorldBossChecked()
	if errors.Is(eventErr, gorm.ErrRecordNotFound) {
		telegram.ReplyText(bot, msg.Chat.ID, "📜 暂无世界Boss记录。\n\n开放时间：每周六、周日 `21:00 - 22:00`。")
		return
	}
	if eventErr != nil {
		log.Printf("⚠️ 世界Boss状态活动读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(eventErr))
		telegram.ReplyText(bot, msg.Chat.ID, "📜 世界Boss状态暂时读取失败，请稍后重试。")
		return
	}
	if event.Status == "active" {
		// 实时刷新按参与人数逐个拉取 ABS 听书时长，对单用户加冷却防止刷屏拖垮 ABS。
		if !storage.CheckRateLimit(msg.From.ID, "world_boss_live_refresh", 10*time.Second) {
			telegram.ReplyText(bot, msg.Chat.ID, "⏳ 查询太频繁了，请稍等几秒再刷新实时战况。")
			return
		}
		if refreshed, _, err := refreshWorldBossLiveDamage(event); err == nil {
			event = refreshed
			ensureWorldBossLiveBoard(bot, event)
			if event.IsKilled {
				settleWorldBoss(bot, event.BossID)
				if latest, latestErr := getActiveOrLatestWorldBossChecked(); latestErr == nil {
					event = latest
				} else if latestErr != nil && !errors.Is(latestErr, gorm.ErrRecordNotFound) {
					log.Printf("⚠️ 世界Boss状态结算后活动重读失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), msg.From.ID, textfmt.FormatPlainError(latestErr))
				}
			}
		} else {
			log.Printf("⚠️ 世界Boss状态实时刷新失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
		}
	}

	statusText := "进行中"
	if event.Status == "active" && event.IsKilled {
		statusText = "已击杀，待结算"
	} else if event.Status == "settled" {
		if event.IsKilled {
			statusText = "已击杀"
		} else {
			statusText = "未击杀"
		}
	}

	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf(
		"🌑 **世界Boss状态**\n\nBoss：**%s**\n状态：`%s`\n血量：`%.2f/%d`\n有效门槛：最终伤害 `>= %.2f`\n参与人数：`%d`\n\n血量按实际参与人数动态调整：基础 `%d`，每人 `%d`，最低 `%d`，最高 `%d`。\n伤害 = 参与后 Boss 时段实际听书小时 × `%.0f` ×（1 + 修为加成 + 宗门科技），不计算白天净修为，无单人伤害上限。\n修为加成：斗者初期 `+1%%`，每小段 `+1%%`，最高 `+25%%`。\n开放时间：每周六、周日 `21:00 - 22:00`，最后 `%d` 分钟停止新道友加入。\n发送 `参加Boss` 加入，`Boss排行` 查看榜单。",
		textfmt.EscapeMarkdown(event.Name),
		statusText,
		event.CurrentHP,
		event.MaxHP,
		worldBossValidDamage,
		event.ParticipantCount,
		worldBossBaseHP,
		worldBossHPPerParticipant,
		worldBossMinHP,
		worldBossMaxHP,
		worldBossActualDamagePerHour,
		int(worldBossJoinCloseBeforeEnd.Minutes()),
	))
}

func handleWorldBossRank(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	event, eventErr := getActiveOrLatestWorldBossChecked()
	if eventErr != nil && !errors.Is(eventErr, gorm.ErrRecordNotFound) {
		log.Printf("⚠️ 世界Boss排行活动读取失败: user=%d err=%s", msg.From.ID, textfmt.FormatPlainError(eventErr))
		telegram.ReplyText(bot, msg.Chat.ID, "📜 世界Boss排行暂时读取失败，请稍后重试。")
		return
	}
	if errors.Is(eventErr, gorm.ErrRecordNotFound) {
		telegram.ReplyText(bot, msg.Chat.ID, "📜 暂无世界Boss记录。")
		return
	}
	if event.Status == "active" {
		// 实时刷新按参与人数逐个拉取 ABS 听书时长，对单用户加冷却防止刷屏拖垮 ABS。
		if !storage.CheckRateLimit(msg.From.ID, "world_boss_live_refresh", 10*time.Second) {
			telegram.ReplyText(bot, msg.Chat.ID, "⏳ 查询太频繁了，请稍等几秒再刷新实时战况。")
			return
		}
		if refreshed, _, err := refreshWorldBossLiveDamage(event); err == nil {
			event = refreshed
			ensureWorldBossLiveBoard(bot, event)
			if event.IsKilled {
				settleWorldBoss(bot, event.BossID)
				if latest, latestErr := getActiveOrLatestWorldBossChecked(); latestErr == nil {
					event = latest
				} else if latestErr != nil && !errors.Is(latestErr, gorm.ErrRecordNotFound) {
					log.Printf("⚠️ 世界Boss排行结算后活动重读失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), msg.From.ID, textfmt.FormatPlainError(latestErr))
				}
			}
		} else {
			log.Printf("⚠️ 世界Boss排行实时刷新失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
		}
	}

	participants, err := worldBossTopParticipants(event.BossID, 10)
	if err != nil {
		log.Printf("⚠️ 世界Boss排行读取失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "📜 世界Boss排行暂时读取失败，请稍后重试。")
		return
	}

	if len(participants) == 0 {
		telegram.ReplyText(bot, msg.Chat.ID, "📜 当前世界Boss暂无参与者。")
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("🏆 **世界Boss伤害榜 · %s**\n\n", textfmt.EscapeMarkdown(event.Name)))

	for i, p := range participants {
		preciseDamage := preciseWorldBossDamage(p)
		b.WriteString(fmt.Sprintf(
			"%d. `%s` 伤害：`%.2f`  实听：`%.0f`分  倍率：`%.2f`\n",
			i+1,
			textfmt.EscapeMarkdown(p.UserName),
			preciseDamage,
			p.DeltaHours*60,
			p.Multiplier,
		))
	}

	telegram.ReplyText(bot, msg.Chat.ID, b.String())
}

func sendWorldBossSettlement(bot *tgbotapi.BotAPI, event model.WorldBossEvent, killed bool, totalDamage float64) {
	var rankText strings.Builder
	participants, err := worldBossTopParticipants(event.BossID, 10)
	if err != nil {
		log.Printf("⚠️ 世界Boss结算排行读取失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
		rankText.WriteString("排行读取失败，请稍后发送 `Boss排行` 查看。\n")
	} else {
		for i, p := range participants {
			rankText.WriteString(fmt.Sprintf("%d. `%s` 伤害 `%.2f`\n", i+1, textfmt.EscapeMarkdown(p.UserName), p.Damage))
		}
		if rankText.Len() == 0 {
			rankText.WriteString("本期暂无有效参与记录。\n")
		}
	}

	resultText := "未能击杀"
	rewardText := "未击杀奖励：不发积分、不发红包、不注入天道奖池；有效参与者宗门贡献 +1，前三名额外贡献 +3/+2/+1。"
	if killed {
		resultText = "击杀成功"
		// 天道奖池 +10 已在 grantWorldBossRewards 的结算事务内完成注入，
		// 此处不再重复注入，避免重发结算消息时二次注水。
		rewardText = fmt.Sprintf("击杀奖励：Boss 红包 `%d` 积分 / `%d` 份，仅本期参与修士可抢，发送 `抢` 领取；天道奖池 `+10`；有效参与者积分 +1、宗门贡献 +3。", worldBossRedPacketTotalPoints, worldBossRedPacketCount)
	}

	notice := fmt.Sprintf(
		"🌑 **【世界Boss结算】** 🌑\n\n"+
			"Boss：**%s**\n"+
			"结果：**%s**\n"+
			"总伤害：`%.2f/%d`\n\n"+
			"本期伤害仅按参与后的 Boss 时段实际听书时长计算，不计白天净修为。\n\n"+
			"%s\n\n"+
			"🏆 **伤害排行 Top 10**\n%s\n"+
			"发送 `Boss排行` 可查看本期榜单。",
		textfmt.EscapeMarkdown(event.Name),
		resultText,
		totalDamage,
		event.MaxHP,
		rewardText,
		rankText.String(),
	)

	targetChatID := event.ChatID
	if targetChatID == 0 {
		targetChatID = config.AppConfig.NoticeGroupID
	}
	if targetChatID != 0 {
		telegram.SendGroupAutoDeleteMessage(bot, targetChatID, notice)
	}
}

func renderWorldBossLiveBoard(event model.WorldBossEvent) string {
	participants, participantsErr := worldBossTopParticipants(event.BossID, 10)
	if participantsErr != nil {
		log.Printf("⚠️ 世界Boss实时战榜排行读取失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(participantsErr))
	}

	statusText := "进行中"
	if event.Status == "settling" {
		statusText = "结算中"
	} else if event.Status == "settled" {
		if event.IsKilled {
			statusText = "已击杀"
		} else {
			statusText = "未击杀"
		}
	} else if event.IsKilled {
		statusText = "已击杀，结算中"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		"🌑 **世界Boss实时战榜**\n\nBoss：**%s**\n状态：`%s`\n血量：`%.2f/%d`\n参与人数：`%d`\n规则：实听小时 × `%.0f` ×（1 + 修为加成 + 宗门科技），无单人伤害上限\n\n",
		textfmt.EscapeMarkdown(event.Name),
		statusText,
		event.CurrentHP,
		event.MaxHP,
		event.ParticipantCount,
		worldBossActualDamagePerHour,
	))

	if participantsErr != nil {
		b.WriteString("排行读取失败，请稍后发送 `Boss排行` 查看。\n")
		return b.String()
	}
	if len(participants) == 0 {
		b.WriteString("暂无道友参战。\n")
		return b.String()
	}

	for i, p := range participants {
		preciseDamage := preciseWorldBossDamage(p)
		b.WriteString(fmt.Sprintf(
			"%d. `%s` 伤害：`%.2f`  实听：`%.0f`分\n",
			i+1,
			textfmt.EscapeMarkdown(p.UserName),
			preciseDamage,
			p.DeltaHours*60,
		))
	}

	b.WriteString("\n每 2 分钟自动刷新；发送 `Boss排行` 可手动刷新。")
	return b.String()
}

func worldBossTopParticipants(bossID string, limit int) ([]model.WorldBossParticipant, error) {
	var participants []model.WorldBossParticipant
	if strings.TrimSpace(bossID) == "" {
		return participants, fmt.Errorf("WORLD_BOSS_ID_EMPTY")
	}
	query := storage.DB.Where("boss_id = ?", bossID).Order(worldBossRankOrder)
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&participants).Error; err != nil {
		return nil, err
	}
	return participants, nil
}

func ensureWorldBossLiveBoard(bot *tgbotapi.BotAPI, event model.WorldBossEvent) {
	if bot == nil || event.ChatID == 0 || event.BossID == "" {
		return
	}

	bossID := event.BossID
	if telegram.EnqueueTelegramAsync(telegram.TelegramAsyncJob{
		Kind:      "world_boss_live_board",
		DedupeKey: "world_boss_live_board:" + bossID,
		Priority:  telegram.TelegramAsyncPriorityLow,
		Send: func() error {
			return ensureWorldBossLiveBoardSync(bot, bossID)
		},
	}) {
		return
	}

	log.Printf("⚠️ 世界Boss实时战榜异步入队失败，改为同步刷新: boss=%s", textfmt.FormatPlainValue(bossID))
	if err := ensureWorldBossLiveBoardSync(bot, bossID); err != nil {
		log.Printf("⚠️ 世界Boss实时战榜同步刷新失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatTelegramSendError(err))
	}
}

func ensureWorldBossLiveBoardSync(bot *tgbotapi.BotAPI, bossID string) error {
	if bot == nil || bossID == "" {
		return nil
	}

	var event model.WorldBossEvent
	if err := storage.DB.Where("boss_id = ?", bossID).First(&event).Error; err != nil {
		log.Printf("⚠️ 世界Boss实时战榜读取事件失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
		return nil
	}
	if event.ChatID == 0 {
		return nil
	}

	text := renderWorldBossLiveBoard(event)
	if event.BoardChatID != 0 && event.BoardMessageID != 0 {
		edit := tgbotapi.NewEditMessageText(event.BoardChatID, event.BoardMessageID, text)
		edit.ParseMode = "Markdown"
		if _, err := bot.Send(telegram.PrepareCopyable(edit)); err == nil {
			return nil
		} else if telegram.IsTelegramMessageNotModifiedError(err) {
			return nil
		} else {
			log.Printf("⚠️ 世界Boss实时战榜编辑失败，将重发: boss=%s chat=%d message=%d err=%s", textfmt.FormatPlainValue(event.BossID), event.BoardChatID, event.BoardMessageID, textfmt.FormatTelegramSendError(err))
		}
	}

	msg := tgbotapi.NewMessage(event.ChatID, text)
	msg.ParseMode = "Markdown"
	sentMsg, err := telegram.SendNoAutoDelete(bot, msg)
	if err != nil {
		log.Printf("⚠️ 世界Boss实时战榜发送失败: boss=%s chat=%d err=%s", textfmt.FormatPlainValue(event.BossID), event.ChatID, textfmt.FormatTelegramSendError(err))
		return err
	}

	res := storage.DB.Model(&model.WorldBossEvent{}).
		Where("boss_id = ?", event.BossID).
		Updates(map[string]interface{}{
			"board_chat_id":    sentMsg.Chat.ID,
			"board_message_id": sentMsg.MessageID,
		})
	if res.Error != nil {
		log.Printf("⚠️ 世界Boss实时战榜消息ID记录失败: boss=%s chat=%d message=%d err=%s", textfmt.FormatPlainValue(event.BossID), sentMsg.Chat.ID, sentMsg.MessageID, textfmt.FormatPlainError(res.Error))
	} else if res.RowsAffected == 0 {
		log.Printf("⚠️ 世界Boss实时战榜消息ID记录未命中: boss=%s chat=%d message=%d", textfmt.FormatPlainValue(event.BossID), sentMsg.Chat.ID, sentMsg.MessageID)
	}
	return nil
}

func getActiveWorldBossChecked() (model.WorldBossEvent, error) {
	if storage.DB == nil {
		return model.WorldBossEvent{}, fmt.Errorf("WORLD_BOSS_DB_UNAVAILABLE")
	}

	var event model.WorldBossEvent
	now := time.Now()
	err := storage.DB.Where("status = ? AND start_at <= ? AND end_at > ?", "active", now, now).
		Order("start_at DESC").
		First(&event).Error
	if err != nil {
		return model.WorldBossEvent{}, err
	}
	return event, nil
}

func getActiveOrLatestWorldBossChecked() (model.WorldBossEvent, error) {
	if event, err := getActiveWorldBossChecked(); err == nil {
		return event, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.WorldBossEvent{}, err
	}
	if storage.DB == nil {
		return model.WorldBossEvent{}, fmt.Errorf("WORLD_BOSS_DB_UNAVAILABLE")
	}

	var event model.WorldBossEvent
	err := storage.DB.Order("start_at DESC").First(&event).Error
	if err != nil {
		return model.WorldBossEvent{}, err
	}
	return event, nil
}

func calculateWorldBossDamage(baseHours float64, finalHours float64, maxDeltaHours float64) (float64, float64, float64) {
	deltaHours := finalHours - baseHours
	if deltaHours < 0 {
		deltaHours = 0
	}
	if maxDeltaHours < 0 {
		maxDeltaHours = 0
	}
	if deltaHours > maxDeltaHours {
		deltaHours = maxDeltaHours
	}

	baseDamage := deltaHours * worldBossActualDamagePerHour
	multiplier := 1.0
	damage := baseDamage
	if damage < 0 {
		damage = 0
	}

	return damage, baseDamage, multiplier
}

func getWorldBossCultivationDamageBonusChecked(userID int64) (float64, error) {
	var cul model.Cultivation
	if err := storage.DB.Where("user_id = ?", userID).First(&cul).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	SyncCultivationRealm(&cul)
	return calculateWorldBossCultivationDamageBonus(cul.MajorRealm, cul.MinorRealm), nil
}

func calculateWorldBossCultivationDamageBonus(major int, minor int) float64 {
	if major <= 0 {
		return 0
	}
	if minor < 1 {
		minor = 1
	}
	if minor > 4 {
		minor = 4
	}

	stage := (major-1)*4 + minor
	if stage < 1 {
		return 0
	}
	bonus := float64(stage) * worldBossCultivationBonusPerStage
	if bonus > worldBossCultivationBonusCap {
		return worldBossCultivationBonusCap
	}
	return bonus
}

func getWorldBossSectDamageBonusChecked(userID int64) (float64, error) {
	level, err := getSectTechnologyLevelByUserChecked(userID, SectTechBossDamageBonus)
	if err != nil {
		return 0, err
	}
	if level <= 0 {
		return 0, nil
	}
	return float64(level) * 0.02, nil
}

func ApplyWorldBossDamageBonusesChecked(userID int64, baseDamage float64) (float64, float64, error) {
	if baseDamage <= 0 {
		return 0, 1, nil
	}
	cultivationBonus, err := getWorldBossCultivationDamageBonusChecked(userID)
	if err != nil {
		return 0, 1, err
	}
	sectBonus, err := getWorldBossSectDamageBonusChecked(userID)
	if err != nil {
		return 0, 1, err
	}
	multiplier := 1 + cultivationBonus + sectBonus
	damage := baseDamage * multiplier
	if damage < 0 {
		return 0, multiplier, nil
	}
	return roundWorldBossDamage(damage), multiplier, nil
}

func roundWorldBossDamage(damage float64) float64 {
	return math.Round(damage*100) / 100
}

func calculateWorldBossDamageWindowHours(p model.WorldBossParticipant, event model.WorldBossEvent, now time.Time) float64 {
	start := p.CreatedAt
	if start.IsZero() || start.Before(event.StartAt) {
		start = event.StartAt
	}

	end := now
	if end.IsZero() || end.After(event.EndAt) {
		end = event.EndAt
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Hours()
}

func getWorldBossRawListeningHours(absUserID string) (float64, error) {
	if absUserID == "" {
		return 0, storage.ErrAbsUserIDEmpty
	}

	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(absUserID), nil)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, fmt.Errorf("ABS_STATUS_%d", code)
	}

	var stats struct {
		TotalTime float64            `json:"totalTime"`
		Days      map[string]float64 `json:"days"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, err
	}

	rawSeconds := abs.AuthoritativeABSListeningTotalSeconds(stats.TotalTime, stats.Days)
	return rawSeconds / 3600.0, nil
}

// Activity refreshes use the same authoritative daily records and atomic
// contribution cursor as personal reports, never a personal-total delta.
func updateCultivationAudioHours(userID int64, _ float64) {
	if _, ok := syncCultivationFromDailyListeningStats(userID); !ok {
		log.Printf("活动刷新修为与宗门贡献结算失败: user=%d", userID)
	}
}

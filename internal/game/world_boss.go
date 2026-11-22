package game

import (
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

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/realm"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/spiritfire"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	"ccoff-abs-bot/internal/textfmt"
	"ccoff-abs-bot/internal/titles"
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
	worldBossRankOrder = "damage DESC, delta_hours DESC, created_at ASC"
	// worldBossLiveRefreshMu serializes scheduled damage refreshes and payouts.
	// Foreground queries never acquire it or perform remote synchronization.
	worldBossLiveRefreshMu sync.Mutex
)

func isWorldBossOpenDay(weekday time.Weekday) bool {
	return weekday == time.Saturday || weekday == time.Sunday
}

func worldBossShouldStartAt(local time.Time) bool {
	if !isWorldBossOpenDay(local.Weekday()) {
		return false
	}
	start := time.Date(local.Year(), local.Month(), local.Day(), worldBossStartHour, 0, 0, 0, local.Location())
	end := time.Date(local.Year(), local.Month(), local.Day(), worldBossEndHour, 0, 0, 0, local.Location())
	return !local.Before(start) && local.Before(end.Add(-worldBossJoinCloseBeforeEnd))
}

func StartWorldBossScheduler(bot *tgbotapi.BotAPI) {
	go func() {
		ticker := time.NewTicker(activitySchedulerInterval)
		defer ticker.Stop()

		runWorldBossSchedulerTick(bot, time.Now())
		for now := range ticker.C {
			runWorldBossSchedulerTick(bot, now)
		}
	}()

	log.Println("✅ 世界Boss调度器已启动：每周六、周日 21:00-22:00")
}

func runWorldBossSchedulerTick(bot *tgbotapi.BotAPI, now time.Time) {
	if config.AppConfig == nil || config.AppConfig.NoticeGroupID == 0 {
		return
	}
	local := now.In(time.FixedZone("CST", 8*3600))
	if worldBossShouldStartAt(local) {
		// The persisted Boss ID prevents duplicate starts. Keep checking during
		// the join window so a restart or transient database failure can recover.
		go startWorldBossEvent(bot, config.AppConfig.NoticeGroupID, local)
	}
	go refreshActiveWorldBosses(bot, local)
	go settleExpiredWorldBosses(bot, local)
}

func HandleWorldBossCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, text string) bool {
	if msg == nil || msg.From == nil {
		return false
	}

	text = strings.TrimSpace(text)
	if text != "世界Boss" && text != "Boss状态" && text != "参加Boss" && text != "Boss排行" && text != "Boss明细" {
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
	case "Boss明细":
		handleWorldBossDetail(bot, msg)
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
			"修为加成：按小境界进度递增，每个完整大境界 `+4%%`，最高 `+25%%`。\n\n"+
			"发送 `Boss状态` 查看进度，`Boss排行` 查看伤害榜，`Boss明细` 查看本人战况。",
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
	if event.BossID == "" || event.Status != "active" || event.IsKilled || event.ListeningCutoffAt != nil {
		return false
	}
	if now.Before(event.StartAt) || !now.Before(event.EndAt) {
		return false
	}
	return now.Before(worldBossJoinDeadline(event))
}

func refreshWorldBossStoredHPByParticipants(bossID string) (event model.WorldBossEvent, err error) {
	err = storage.DB.Transaction(func(tx *gorm.DB) error {
		event, err = refreshWorldBossStoredHPByParticipantsInTx(tx, bossID)
		return err
	})
	return event, err
}

func refreshWorldBossStoredHPByParticipantsInTx(tx *gorm.DB, bossID string) (event model.WorldBossEvent, err error) {
	err = func() error {
		// Serialize the local join/HP update with background publication, without
		// waiting for the refresh mutex or holding a transaction across ABS calls.
		lock := tx.Model(&model.WorldBossEvent{}).Where("boss_id = ? AND status = ?", bossID, "active").UpdateColumn("status", "active")
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			return fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
		}
		if err := tx.Where("boss_id = ? AND status = ?", bossID, "active").First(&event).Error; err != nil {
			return err
		}

		if event.IsKilled || event.ListeningCutoffAt != nil {
			return nil
		}

		var participantCount int64
		if err := tx.Model(&model.WorldBossParticipant{}).
			Where("boss_id = ?", bossID).
			Count(&participantCount).Error; err != nil {
			return err
		}

		var totalDamage float64
		if err := tx.Model(&model.WorldBossParticipant{}).
			Where("boss_id = ?", bossID).
			Select("COALESCE(SUM(damage), 0)").
			Scan(&totalDamage).Error; err != nil {
			return err
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
		if isKilled {
			cutoff := minTime(time.Now(), event.EndAt)
			event.ListeningCutoffAt = &cutoff
		}

		res := tx.Model(&model.WorldBossEvent{}).
			Where("boss_id = ? AND status = ?", bossID, "active").
			Updates(map[string]interface{}{
				"max_hp":              maxHP,
				"current_hp":          currentHP,
				"is_killed":           isKilled,
				"listening_cutoff_at": event.ListeningCutoffAt,
				"participant_count":   int(participantCount),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
		}

		event.MaxHP = maxHP
		event.CurrentHP = currentHP
		event.IsKilled = isKilled
		event.ParticipantCount = int(participantCount)
		return nil
	}()
	return event, err
}

func settleExpiredWorldBosses(bot *tgbotapi.BotAPI, now time.Time) {
	recoverStaleWorldBossSettlements(now)

	var events []model.WorldBossEvent
	if err := storage.DB.Where("status IN ? AND (end_at <= ? OR listening_cutoff_at IS NOT NULL OR is_killed = ?)", []string{"active", "settling"}, now, true).Find(&events).Error; err != nil {
		log.Printf("⚠️ 查询到期世界Boss失败，已跳过本轮结算扫描: now=%s err=%s", textfmt.FormatPlainValue(now.Format(time.RFC3339)), textfmt.FormatPlainError(err))
		return
	}

	runActivityEventBatch(len(events), func(i int) { settleWorldBoss(bot, events[i].BossID) })
}

func refreshActiveWorldBosses(bot *tgbotapi.BotAPI, now time.Time) {
	recoverStaleWorldBossSettlements(now)

	var events []model.WorldBossEvent
	if err := storage.DB.Where("status = ? AND start_at <= ? AND end_at > ?", "active", now, now).Find(&events).Error; err != nil {
		log.Printf("⚠️ 查询进行中世界Boss失败，已跳过本轮实时刷新: now=%s err=%s", textfmt.FormatPlainValue(now.Format(time.RFC3339)), textfmt.FormatPlainError(err))
		return
	}

	for _, event := range events {
		if event.IsKilled || event.ListeningCutoffAt != nil {
			settleWorldBoss(bot, event.BossID)
			continue
		}
		if !worldBossLiveRefreshDue(event, now) {
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

// tryRefreshWorldBossLiveDamage 调度器专用：已有刷新在途时直接跳过，等待下一轮 tick，避免重复 ABS 拉取。
// 返回 ran=false 表示本轮未执行，调用方不得基于旧数据刷新战榜或结算。
func tryRefreshWorldBossLiveDamage(event model.WorldBossEvent) (model.WorldBossEvent, bool, error) {
	if !worldBossLiveRefreshMu.TryLock() {
		return event, false, nil
	}
	defer worldBossLiveRefreshMu.Unlock()
	var current model.WorldBossEvent
	if err := storage.DB.Where("boss_id = ?", event.BossID).First(&current).Error; err != nil {
		return event, false, err
	}
	if !worldBossLiveRefreshDue(current, time.Now()) {
		return current, false, nil
	}
	refreshed, _, err := refreshWorldBossLiveDamageLocked(current)
	return refreshed, true, err
}

func refreshWorldBossLiveDamageLocked(event model.WorldBossEvent) (model.WorldBossEvent, float64, error) {
	if event.BossID == "" || event.Status != "active" {
		return event, 0, nil
	}
	var participants []model.WorldBossParticipant
	if err := storage.DB.Where("boss_id = ?", event.BossID).Find(&participants).Error; err != nil {
		return event, 0, err
	}
	refreshAt := time.Now()
	processed := make([]bool, len(participants))
	failures := make([]error, len(participants))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(listeningSyncWorkers-1, len(participants)) {
		workers.Go(func() {
			for i := range jobs {
				processed[i], failures[i] = refreshWorldBossLiveParticipant(&participants[i], event, refreshAt)
			}
		})
	}
	for i := range participants {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	incomplete := false
	for i, err := range failures {
		if err != nil {
			incomplete = true
			log.Printf("世界Boss后台刷新参与者失败: boss=%s user=%d err=%s", textfmt.FormatPlainValue(event.BossID), participants[i].UserID, textfmt.FormatPlainError(err))
		}
	}
	completedAt := time.Now()
	totalDamage := 0.0
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Acquire the writer before eligibility reads, without holding it during ABS.
		lock := tx.Model(&model.WorldBossEvent{}).Where("boss_id = ? AND status = ? AND listening_cutoff_at IS NULL", event.BossID, "active").UpdateColumn("status", "active")
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			return fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
		}
		incomplete = false
		for i := range participants {
			p := participants[i]
			eligible, err := activityLiveParticipantEligible(tx, p.UserID, p.AbsUserID, 0)
			if err != nil {
				return err
			}
			if !eligible {
				if err := clearWorldBossParticipantComputedDamageInTx(tx, p, "eligibility_changed_before_publication"); err != nil {
					return err
				}
				continue
			}
			if failures[i] != nil {
				// A failed account retains its saved damage; healthy accounts still publish.
				incomplete = true
				totalDamage += p.Damage
				continue
			}
			if !processed[i] {
				if err := clearWorldBossParticipantComputedDamageInTx(tx, p, "ineligible_for_live_preview"); err != nil {
					return err
				}
				continue
			}
			res := tx.Model(&model.WorldBossParticipant{}).
				Where("id = ? AND boss_id = ? AND user_id = ?", p.ID, event.BossID, p.UserID).
				Updates(map[string]interface{}{"final_hours": p.FinalHours, "delta_hours": p.DeltaHours,
					"base_damage": p.BaseDamage, "multiplier": p.Multiplier, "damage": p.Damage})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("WORLD_BOSS_PARTICIPANT_DAMAGE_UPDATE_MISSED")
			}
			totalDamage += p.Damage
		}
		totalDamage = roundWorldBossDamage(totalDamage)
		var count int64
		if err := tx.Model(&model.WorldBossParticipant{}).Where("boss_id = ?", event.BossID).Count(&count).Error; err != nil {
			return err
		}
		event.MaxHP = calculateWorldBossMaxHP(int(count))
		event.CurrentHP = math.Max(0, roundWorldBossDamage(float64(event.MaxHP)-totalDamage))
		event.IsKilled = totalDamage >= float64(event.MaxHP)
		event.ParticipantCount = int(count)
		event.LiveRefreshedAt, event.LiveRefreshIncomplete = &completedAt, incomplete
		if event.IsKilled {
			cutoff := minTime(completedAt, event.EndAt)
			event.ListeningCutoffAt = &cutoff
		}
		res := tx.Model(&model.WorldBossEvent{}).
			Where("boss_id = ? AND status = ? AND listening_cutoff_at IS NULL", event.BossID, "active").
			Updates(map[string]interface{}{"max_hp": event.MaxHP, "current_hp": event.CurrentHP, "is_killed": event.IsKilled,
				"participant_count": event.ParticipantCount, "live_refreshed_at": completedAt, "live_refresh_incomplete": incomplete,
				"listening_cutoff_at": event.ListeningCutoffAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("WORLD_BOSS_ACTIVE_STATE_CHANGED")
		}
		return nil
	})
	log.Printf("Boss刷新完成: boss=%s participants=%d failures=%d duration_ms=%d", textfmt.FormatPlainValue(event.BossID), len(participants), countActivityFailures(failures), time.Since(refreshAt).Milliseconds())
	return event, totalDamage, err
}

func settleWorldBoss(bot *tgbotapi.BotAPI, bossID string) {
	// Scheduled passes retry later rather than accumulating blocked goroutines.
	if !worldBossLiveRefreshMu.TryLock() {
		return
	}
	defer worldBossLiveRefreshMu.Unlock()
	now := time.Now()
	var event model.WorldBossEvent
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Acquire the writer before reading the cutoff, and publish the claim
		// and its immutable deadline together before any final ABS requests.
		res := tx.Model(&model.WorldBossEvent{}).
			Where("boss_id = ? AND status IN ?", bossID, []string{"active", "settling"}).Update("status", "settling")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.Where("boss_id = ?", bossID).First(&event).Error; err != nil {
			return err
		}
		if event.ListeningCutoffAt != nil {
			return nil
		}
		cutoff := minTime(now, event.EndAt)
		// Recover kills published by older versions before they could save a
		// cutoff. Their last live publication is the known kill observation.
		if event.IsKilled && event.LiveRefreshedAt != nil {
			cutoff = minTime(cutoff, *event.LiveRefreshedAt)
		}
		event.ListeningCutoffAt = &cutoff
		return tx.Model(&event).Update("listening_cutoff_at", cutoff).Error
	})
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("世界Boss结算认领失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
		}
		return
	}

	var participants []model.WorldBossParticipant
	if err := storage.DB.Where("boss_id = ?", bossID).Find(&participants).Error; err != nil {
		resetWorldBossToActive(bossID, err)
		return
	}
	event.MaxHP = calculateWorldBossMaxHP(len(participants))
	snapshotErrors := captureActivitySnapshotBatch(len(participants), func(i int) error {
		p := &participants[i]
		var current model.User
		if err := storage.DB.Where("telegram_id = ?", p.UserID).First(&current).Error; err != nil {
			return err
		}
		if current.Status != "active" || p.AbsUserID == "" || current.AbsUserID != p.AbsUserID {
			return nil
		}
		_, _, err := worldBossListeningHours(p, event, p.AbsUserID, time.Now())
		return err
	})

	for _, err := range snapshotErrors {
		if errors.Is(err, ErrActivitySnapshotPending) {
			return
		}
	}

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
		if u.Status != "active" || p.AbsUserID == "" || u.AbsUserID != p.AbsUserID {
			if err := clearWorldBossParticipantComputedDamage(*p, "binding_changed_or_unbound"); err != nil {
				resetWorldBossToActive(bossID, err)
				return
			}
			continue
		}

		if snapshotErrors[i] != nil || !p.ListeningSnapshot.Finalized {
			resetWorldBossToActive(bossID, fmt.Errorf("WORLD_BOSS_FINAL_SNAPSHOT_FAILED: %v", snapshotErrors[i]))
			return
		}
		provenHours := p.ListeningSnapshot.Seconds / 3600

		// Use current wall-clock time; the helper clamps the window to the event end time.
		maxDeltaHours := calculateWorldBossDamageWindowHours(*p, event, time.Now())
		damage, baseDamage, multiplier := calculateWorldBossDamage(provenHours, maxDeltaHours)
		damage, multiplier, err := ApplyWorldBossDamageBonusesChecked(p.UserID, baseDamage)
		if err != nil {
			resetWorldBossToActive(bossID, err)
			return
		}

		p.FinalHours = p.BaseHours + provenHours
		finalHours := p.FinalHours
		p.DeltaHours = provenHours
		if p.DeltaHours > maxDeltaHours {
			p.DeltaHours = maxDeltaHours
		}
		p.BaseDamage = baseDamage
		p.Multiplier = multiplier
		p.Damage = damage

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
	settledDamage, err := grantWorldBossRewards(&event, rewardParticipants)
	if err != nil {
		log.Printf("❌ 世界Boss奖励结算失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
		resetWorldBossToActive(bossID, err)
		return
	}

	if err := storage.DB.Where("boss_id = ?", bossID).First(&event).Error; err == nil {
		ensureWorldBossLiveBoard(bot, event)
	} else {
		log.Printf("⚠️ 世界Boss结算后事件重读失败: boss=%s err=%s", textfmt.FormatPlainValue(bossID), textfmt.FormatPlainError(err))
	}
	sendWorldBossSettlement(bot, event, event.IsKilled, settledDamage)
}

func clearWorldBossParticipantComputedDamage(p model.WorldBossParticipant, reason string) error {
	return clearWorldBossParticipantComputedDamageInTx(storage.DB, p, reason)
}

func clearWorldBossParticipantComputedDamageInTx(tx *gorm.DB, p model.WorldBossParticipant, reason string) error {
	if p.ID == 0 || p.BossID == "" {
		return fmt.Errorf("WORLD_BOSS_PARTICIPANT_INVALID")
	}
	res := tx.Model(&model.WorldBossParticipant{}).
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
	targetChatID := event.ChatID
	if targetChatID == 0 && config.AppConfig != nil {
		targetChatID = config.AppConfig.NoticeGroupID
	}
	return model.RedPacket{
		NoticeChatID: targetChatID,
		ID:           packetID,
		SenderID:     0,
		SenderName:   "世界Boss",
		TotalPoints:  worldBossRedPacketTotalPoints,
		Count:        worldBossRedPacketCount,
		LeftCount:    worldBossRedPacketCount,
		LeftPoints:   worldBossRedPacketTotalPoints,
		CreatedAt:    now,
		RefType:      "world_boss",
		RefID:        event.BossID,
		ClaimScope:   storage.RedPacketClaimScopeWorldBossParticipant,
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

func grantWorldBossRewards(event *model.WorldBossEvent, participants []model.WorldBossParticipant) (float64, error) {
	totalDamage := 0.0

	// 击杀时的天道奖池 +10 注入必须与结算同事务、受 status='settling'→'settled'
	// 状态闸门保护，避免“已记结算但奖池注入因进程崩溃丢失”或后续重发结算
	// 消息导致重复注入。按项目锁序要求：先持 fusionPoolMutex，再开事务。
	err := storage.RunFusionPoolLockedTransaction(func(tx *gorm.DB) error {
		// Serialize binding validation and payouts with account deletion/recreation.
		if err := tx.Exec("UPDATE world_boss_events SET id = id WHERE boss_id = ?", event.BossID).Error; err != nil {
			return err
		}
		eligible := make([]model.WorldBossParticipant, 0, len(participants))
		for _, p := range participants {
			var bound int64
			if err := tx.Model(&model.User{}).Where("telegram_id = ? AND abs_user_id = ? AND abs_user_id <> '' AND status = ?", p.UserID, p.AbsUserID, "active").Count(&bound).Error; err != nil {
				return err
			}
			if bound != 1 {
				if err := clearWorldBossParticipantComputedDamageInTx(tx, p, "binding_changed_before_reward"); err != nil {
					return err
				}
				continue
			}
			eligible = append(eligible, p)
			totalDamage += p.Damage
		}
		participants = eligible
		sort.Slice(participants, func(i, j int) bool {
			return worldBossParticipantRankLess(participants[i], participants[j])
		})
		totalDamage = roundWorldBossDamage(totalDamage)
		killed := totalDamage >= float64(event.MaxHP)
		currentHP := math.Max(0, roundWorldBossDamage(float64(event.MaxHP)-totalDamage))
		event.IsKilled, event.CurrentHP = killed, currentHP
		event.ParticipantCount = len(participants)

		if killed {
			packetID := "HB-WB-" + security.GenerateRandomCode(8)
			packet := newWorldBossRedPacket(packetID, *event, time.Now())
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
			if err := spiritfire.AwardInTx(tx, "boss:"+event.BossID, ids, true, time.Now()); err != nil {
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
			if _, _, err := storage.AddPointsToFusionPoolInTx(tx, 10, event.ChatID); err != nil {
				return err
			}
		}

		return titles.RefreshBossTitleInTx(tx, settledAt)
	})
	return totalDamage, err
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
	event, err := getActiveWorldBossChecked()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		replyActivityEntry(bot, msg, "📜 当前没有开放中的世界Boss。")
		return
	}
	if err != nil {
		replyActivityEntry(bot, msg, "❌ 世界Boss状态读取失败，请稍后重试。")
		return
	}
	var user model.User
	if err := storage.DB.Where("telegram_id = ?", msg.From.ID).First(&user).Error; err != nil {
		replyActivityEntry(bot, msg, "❌ 账号档案读取失败，请稍后重试。")
		return
	}
	var existing model.WorldBossParticipant
	err = storage.DB.Where("boss_id = ? AND user_id = ?", event.BossID, msg.From.ID).First(&existing).Error
	if err == nil {
		if existing.AbsUserID != user.AbsUserID {
			replyActivityEntry(bot, msg, "⚠️ 本期已有参与记录，但账号绑定已变化，无法重新入场。请发送 `Boss明细` 查看记录。")
			return
		}
		replyActivityEntry(bot, msg, formatWorldBossEntry(event, existing))
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		replyActivityEntry(bot, msg, "❌ 参与记录读取失败，请稍后重试。")
		return
	}
	if !canJoinWorldBossAt(event, time.Now()) {
		replyActivityEntry(bot, msg, "⏳ 当前 Boss 已停止新道友加入，请等待下一场活动。")
		return
	}
	usable, err := storage.UserHasUsableLocalAbsAccountAt(user, time.Now())
	if err != nil || !usable {
		replyActivityEntry(bot, msg, "❌ 参加世界Boss需要有效且未暂停的 ABS 听书账号。")
		return
	}
	submitActivityEntry(bot, msg, model.ActivityEntryRequest{Kind: activityEntryBoss, EventID: event.BossID, UserID: msg.From.ID,
		AbsUserID: user.AbsUserID, DeadlineAt: worldBossJoinDeadline(event)})
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
	statusText := "进行中"
	if event.Status != "settled" && (event.Status == "settling" || event.ListeningCutoffAt != nil || (event.Status == "active" && !time.Now().Before(event.EndAt))) {
		statusText = "结算中"
	} else if event.Status == "active" && event.IsKilled {
		statusText = "已击杀，待结算"
	} else if event.Status == "settled" {
		if event.IsKilled {
			statusText = "已击杀"
		} else {
			statusText = "未击杀"
		}
	}

	telegram.ReplyText(bot, msg.Chat.ID, fmt.Sprintf(
		"🌑 **世界Boss状态**\n\nBoss：**%s**\n状态：`%s`\n血量：`%.2f/%d`\n有效门槛：最终伤害 `>= %.2f`\n参与人数：`%d`\n\n血量按实际参与人数动态调整：基础 `%d`，每人 `%d`，最低 `%d`，最高 `%d`。\n伤害 = 参与后 Boss 时段实际听书小时 × `%.0f` ×（1 + 修为加成 + 宗门科技），不计算白天净修为，无单人伤害上限。\n修为加成：按小境界进度递增，每个完整大境界 `+4%%`，最高 `+25%%`。\n开放时间：每周六、周日 `21:00 - 22:00`，最后 `%d` 分钟停止新道友加入。\n发送 `参加Boss` 加入，`Boss排行` 查看榜单。%s",
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
		worldBossLiveRefreshNotice(event),
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
	participants, err := worldBossTopParticipants(event.BossID, 10)
	if err != nil {
		log.Printf("⚠️ 世界Boss排行读取失败: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatPlainError(err))
		telegram.ReplyText(bot, msg.Chat.ID, "📜 世界Boss排行暂时读取失败，请稍后重试。")
		return
	}

	if len(participants) == 0 {
		telegram.ReplyText(bot, msg.Chat.ID, "📜 当前世界Boss暂无参与者。"+worldBossLiveRefreshNotice(event))
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

	b.WriteString("\n发送 `Boss明细` 查看本人的听书时长与伤害。\n")
	b.WriteString(worldBossLiveRefreshNotice(event))
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
		if config.AppConfig != nil {
			targetChatID = config.AppConfig.NoticeGroupID
		}
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
		b.WriteString(worldBossLiveRefreshNotice(event))
		return b.String()
	}
	if len(participants) == 0 {
		b.WriteString("暂无道友参战。\n")
		b.WriteString(worldBossLiveRefreshNotice(event))
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

	b.WriteString(worldBossLiveRefreshNotice(event))
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
		Kind:        "world_boss_live_board",
		DedupeKey:   "world_boss_live_board:" + bossID,
		Priority:    telegram.TelegramAsyncPriorityLow,
		MaxAttempts: 1,
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
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
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
		} else if !telegram.IsTelegramMessageUnavailableForEdit(err) {
			log.Printf("⚠️ 世界Boss实时战榜编辑失败，保留原消息: boss=%s err=%s", textfmt.FormatPlainValue(event.BossID), textfmt.FormatTelegramSendError(err))
			return err
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

func calculateWorldBossDamage(deltaHours float64, maxDeltaHours float64) (float64, float64, float64) {
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
	count := realm.StageCount(major)
	if count == 0 {
		return 0
	}
	if minor > count {
		minor = count
	}
	// 每个大境界仍占原先四阶段的总加成，按实际星/转/阶细分。
	stage := float64((major-1)*4) + 4*float64(minor)/float64(count)
	bonus := stage * worldBossCultivationBonusPerStage
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
	if event.ListeningCutoffAt != nil && end.After(*event.ListeningCutoffAt) {
		end = *event.ListeningCutoffAt
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Hours()
}

// Activity refreshes use the same authoritative daily records and atomic
// contribution cursor as personal reports, never a personal-total delta.
func updateCultivationAudioHours(userID int64, _ float64) {
	if _, ok := syncCultivationFromDailyListeningStats(userID); !ok {
		log.Printf("活动刷新修为与宗门贡献结算失败: user=%d", userID)
	}
}

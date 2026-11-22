package game

import (
	"errors"
	"fmt"
	"math"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
)

type activityEntryIneligible struct{ reason string }

func (e *activityEntryIneligible) Error() string { return e.reason }
func entryIneligible(reason string) error        { return &activityEntryIneligible{reason} }

func validateActivityEntryTx(tx *gorm.DB, r model.ActivityEntryRequest, now time.Time) error {
	if !now.Before(r.DeadlineAt) {
		return entryIneligible("已超过入场期限")
	}
	var user model.User
	if err := tx.Where("telegram_id = ?", r.UserID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return entryIneligible("账号已不存在")
		}
		return err
	}
	if user.AbsUserID != r.AbsUserID {
		return entryIneligible("账号绑定已变化，请重新报名")
	}
	usable, err := storage.UserHasUsableLocalAbsAccountTxAt(tx, user, now)
	if err != nil {
		return err
	}
	if !usable {
		return entryIneligible("听书账号已过期、暂停或不可用")
	}
	switch r.Kind {
	case activityEntryBoss:
		var event model.WorldBossEvent
		if err := tx.Where("boss_id = ?", r.EventID).First(&event).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return entryIneligible("Boss 活动已不存在")
			}
			return err
		}
		if !canJoinWorldBossAt(event, now) {
			return entryIneligible("Boss 已停止入场或已结束")
		}
	case activityEntryRealm:
		var event model.SectSecretRealmEvent
		if err := tx.Where("realm_id = ?", r.EventID).First(&event).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return entryIneligible("秘境活动已不存在")
			}
			return err
		}
		if event.SectID != r.SectID || !canJoinSectSecretRealmAt(event, now) {
			return entryIneligible("秘境已关闭或宗门已变化")
		}
		var count int64
		if err := tx.Model(&model.SectMember{}).Where("id = ? AND user_id = ? AND sect_id = ?", r.MemberID, r.UserID, r.SectID).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return entryIneligible("宗门成员资格已变化，请重新报名")
		}
	default:
		return entryIneligible("活动类型无效")
	}
	return nil
}

func commitActivityEntry(r model.ActivityEntryRequest, baseline model.ActivityListeningSnapshot, baseHours float64, requestedAt time.Time) (string, error) {
	result := ""
	err := storage.DB.Transaction(func(tx *gorm.DB) error {
		// Claim a short SQLite write transaction only after network sampling.
		lock := tx.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status = ?", r.ID, r.Generation, "pending").UpdateColumn("status", "pending")
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			return entryIneligible("入场申请状态已变化")
		}
		now := time.Now()
		if err := validateActivityEntryTx(tx, r, now); err != nil {
			return err
		}
		if requestedAt.IsZero() || baseline.BaselineDay != SectDayKey(requestedAt) || baseline.BaselineDay != SectDayKey(now) {
			return errors.New("基线采样期间已跨日，需重新采样")
		}
		if r.Kind == activityEntryBoss {
			var event model.WorldBossEvent
			if err := tx.Where("boss_id = ?", r.EventID).First(&event).Error; err != nil {
				return err
			}
			var participant model.WorldBossParticipant
			err := tx.Where("boss_id = ? AND user_id = ?", r.EventID, r.UserID).First(&participant).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var day model.DailyListeningStat
				err := tx.Select("capped_seconds").Where("user_id = ? AND abs_user_id = ? AND day_key = ?", r.UserID, r.AbsUserID, baseline.BaselineDay).First(&day).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				baseline.BaselineSeconds = dailyListeningCreditSeconds(baseline.BaselineDay, math.Max(baseline.BaselineSeconds, day.CappedSeconds), now)
				participant = model.WorldBossParticipant{Model: gorm.Model{CreatedAt: now}, BossID: r.EventID, UserID: r.UserID, UserName: r.UserName,
					AbsUserID: r.AbsUserID, BaseHours: baseline.BaselineSeconds / 3600, ListeningSnapshot: baseline}
				if _, err := createWorldBossParticipantInTx(tx, &participant); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if participant.AbsUserID != r.AbsUserID {
				return entryIneligible("本期已有其他绑定的参与记录，无法重新入场")
			}
			var errHP error
			event, errHP = refreshWorldBossStoredHPByParticipantsInTx(tx, r.EventID)
			if errHP != nil {
				return errHP
			}
			result = formatWorldBossEntry(event, participant)
		} else {
			var event model.SectSecretRealmEvent
			if err := tx.Where("realm_id = ?", r.EventID).First(&event).Error; err != nil {
				return err
			}
			profile, err := sectSecretRealmProfileFromSnapshotChecked(event.ProfileKey, event.ConfigSnapshot)
			if err != nil {
				return err
			}
			var participant model.SectSecretRealmParticipant
			err = tx.Where("realm_id = ? AND user_id = ?", r.EventID, r.UserID).First(&participant).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var cultivation model.Cultivation
				if err := tx.Where("user_id = ?", r.UserID).First(&cultivation).Error; err != nil {
					return err
				}
				if cultivation.MajorRealm < profile.MinMajorRealm {
					return entryIneligible("当前境界尚未达到本档秘境要求")
				}
				participant = model.SectSecretRealmParticipant{Model: gorm.Model{CreatedAt: now}, RealmID: r.EventID, SectID: r.SectID, UserID: r.UserID,
					UserName: r.UserName, AbsUserID: r.AbsUserID, MajorRealm: cultivation.MajorRealm, MinorRealm: cultivation.MinorRealm,
					BaseHours: baseHours, BaseRawSeconds: baseline.BaselineSeconds, ListeningSnapshot: baseline}
				if err := createSectSecretRealmParticipantIfMissingInTx(tx, &participant); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if participant.AbsUserID != r.AbsUserID {
				return entryIneligible("本次已有其他绑定的参与记录，无法重新入场")
			}
			var count int64
			if err := tx.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ?", r.EventID).Count(&count).Error; err != nil {
				return err
			}
			res := tx.Model(&model.SectSecretRealmEvent{}).Where("realm_id = ? AND status = ?", r.EventID, "active").Update("participant_count", count)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return entryIneligible("秘境已停止入场")
			}
			result = formatSectSecretRealmEntry(event, participant, profile)
		}
		res := tx.Model(&model.ActivityEntryRequest{}).Where("id = ? AND generation = ? AND status = ?", r.ID, r.Generation, "pending").
			Updates(map[string]any{"status": "joined", "completed_at": now, "result_text": result, "last_error": "", "next_attempt_at": nil})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return entryIneligible("入场申请状态已变化")
		}
		return nil
	})
	return result, err
}

func formatWorldBossEntry(event model.WorldBossEvent, p model.WorldBossParticipant) string {
	return fmt.Sprintf("✅ 已加入世界Boss讨伐。\n\n道友：`%s`\nBoss：**%s**\n正式入场：`%s`（北京时间）\n入场当日有效听书基线：`%.2f` 小时\n当前血量：`%.2f/%d`\n参与人数：`%d`\n\n伤害从正式入场后开始计算；发送 `Boss明细` 查看个人战况。",
		textfmt.EscapeMarkdown(p.UserName), textfmt.EscapeMarkdown(event.Name), p.CreatedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"),
		p.ListeningSnapshot.BaselineSeconds/3600, event.CurrentHP, event.MaxHP, event.ParticipantCount)
}

func formatSectSecretRealmEntry(event model.SectSecretRealmEvent, p model.SectSecretRealmParticipant, profile SectSecretRealmProfileConfig) string {
	return fmt.Sprintf("✅ 已进入宗门秘境【%s】。\n\n道友：`%s`\n档位：`%s`\n当前境界：%s\n正式入场：`%s`（北京时间）\n当前基线净修为：`%.1f` 小时\n入场当日有效听书基线：`%.2f` 小时\n\n秘境进度从正式入场后开始计算；前 `%.1f` 小时全额计入，发送 `宗门秘境明细` 查看个人记录。",
		textfmt.EscapeMarkdown(event.Name), textfmt.EscapeMarkdown(p.UserName), textfmt.EscapeMarkdown(profile.Name), sectSecretRealmRealmMarkdown(p.MajorRealm, p.MinorRealm),
		p.CreatedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05"), p.BaseHours, p.ListeningSnapshot.BaselineSeconds/3600, profile.PressureFullHours)
}

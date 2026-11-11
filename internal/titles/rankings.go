package titles

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
)

func heldInTx(tx *gorm.DB, chatID, userID int64, d Definition) (bool, error) {
	var count int64
	var query *gorm.DB
	if d.Source == "ranking" {
		query = tx.Model(&model.RankTitleHolder{}).Where("title_key = ? AND user_id = ?", d.Key, userID)
	} else {
		query = tx.Model(&model.TribulationTitleHolder{}).Where("chat_id = ? AND title_key = ? AND user_id = ?", chatID, d.Key, userID)
	}
	err := query.Count(&count).Error
	return count > 0, err
}

func RefreshRankTitles(now time.Time) error {
	started := time.Now()
	return Transaction(func(tx *gorm.DB) error {
		// 等待称号锁或数据库连接可能跨过换期点；以真正开始读取的
		// 时间计算周期，避免旧任务把已更新的榜首恢复到上一期。
		return refreshRankTitlesInTx(tx, now.Add(time.Since(started)))
	})
}

func refreshRankTitlesInTx(tx *gorm.DB, now time.Time) error {
	return refreshRankTitlesWithListeningInTx(tx, now, false)
}

var ErrListeningBatchPeriodChanged = errors.New("听书刷新跨月，等待新月完整刷新后发布称号")

// RefreshRankTitlesAfterListeningBatch publishes listening-dependent titles
// only after a successful batch that finishes in the same month. publishedAt
// is the batch end time; lock/connection wait time also counts toward rollover.
// A cross-month batch releases expired titles but returns an error so callers
// know that a complete batch in the new month is still needed.
func RefreshRankTitlesAfterListeningBatch(batchStartedAt, publishedAt time.Time) error {
	waitStarted := time.Now()
	periodChanged := false
	err := Transaction(func(tx *gorm.DB) error {
		now := publishedAt.Add(time.Since(waitStarted))
		periodChanged = rankings.TitlePeriodKey(rankings.ListeningTitle, batchStartedAt) != rankings.TitlePeriodKey(rankings.ListeningTitle, now)
		return refreshRankTitlesWithListeningInTx(tx, now, !periodChanged)
	})
	if err != nil {
		return err
	}
	if periodChanged {
		return ErrListeningBatchPeriodChanged
	}
	return nil
}

func refreshRankTitlesWithListeningInTx(tx *gorm.DB, now time.Time, completedListeningBatch bool) error {
	var wealth model.User
	if err := rankings.Wealth(tx).Take(&wealth).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var cultivation struct {
		UserID   int64
		UserName string
	}
	if err := rankings.Cultivation(tx).Select("cultivations.user_id, users.username AS user_name").Take(&cultivation).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var sect model.Sect
	if err := rankings.Sects(tx).Take(&sect).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var owner model.User
	if sect.OwnerID > 0 {
		if err := tx.Where("telegram_id = ?", sect.OwnerID).Take(&owner).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	candidates := []model.RankTitleHolder{
		{TitleKey: rankings.WealthTitle, UserID: wealth.TelegramID, UserName: wealth.Username},
		{TitleKey: rankings.CultivationTitle, UserID: cultivation.UserID, UserName: cultivation.UserName},
		{TitleKey: rankings.SectTitle, UserID: owner.TelegramID, UserName: owner.Username, SubjectID: sect.ID},
	}
	activityCandidates, err := rankings.ActivityTitleCandidates(tx, now)
	if err != nil {
		return err
	}
	candidates = append(candidates, activityCandidates...)
	for _, candidate := range candidates {
		if !completedListeningBatch && (candidate.TitleKey == rankings.ListeningTitle || candidate.TitleKey == rankings.CultivationTitle) {
			// 不用个人刷新或批次中间态重新评选。月榜仍需按时释放旧期
			// 归属；新期榜首只能由下一次完整批次发布。
			var old model.RankTitleHolder
			err := tx.Where("title_key = ?", candidate.TitleKey).Take(&old).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil && old.PeriodKey == candidate.PeriodKey {
				continue
			}
			candidate.UserID, candidate.UserName, candidate.Score = 0, "", 0
		}
		if err := updateRankTitleInTx(tx, candidate, now); err != nil {
			return err
		}
	}
	return nil
}

func updateRankTitleInTx(tx *gorm.DB, next model.RankTitleHolder, now time.Time) error {
	var old model.RankTitleHolder
	err := tx.Where("title_key = ?", next.TitleKey).Take(&old).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && old.UserID == next.UserID && old.PeriodKey == next.PeriodKey {
		return tx.Model(&old).Updates(map[string]any{"user_name": next.UserName, "subject_id": next.SubjectID, "score": next.Score}).Error
	}
	if old.UserID > 0 {
		if err := tx.Model(&model.RankTitleHistory{}).Where("title_key = ? AND user_id = ? AND released_at IS NULL", old.TitleKey, old.UserID).Update("released_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.RankTitleHistory{}).Where("title_key = ? AND user_id = ? AND notice_state = ?", old.TitleKey, old.UserID, "pending").Update("notice_state", "obsolete").Error; err != nil {
			return err
		}
		if old.UserID != next.UserID {
			var equipment []model.TitleEquipment
			if err := tx.Where("title_key = ? AND user_id = ?", old.TitleKey, old.UserID).Find(&equipment).Error; err != nil {
				return err
			}
			for _, e := range equipment {
				if err := FallbackInTx(tx, e.ChatID, e.UserID, old.TitleKey); err != nil {
					return err
				}
			}
		}
	}
	next.AcquiredAt = now
	if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&next).Error; err != nil {
		return err
	}
	if next.UserID == 0 {
		return nil
	}
	return tx.Create(&model.RankTitleHistory{TitleKey: next.TitleKey, UserID: next.UserID, SubjectID: next.SubjectID, PeriodKey: next.PeriodKey, AcquiredAt: now, NoticeState: "pending"}).Error
}

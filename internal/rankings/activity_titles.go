package rankings

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
)

var titleLocation = time.FixedZone("CST", 8*3600)

func activityTitlePeriodStarts(now time.Time) (time.Time, time.Time) {
	local := now.In(titleLocation)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, titleLocation)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, titleLocation)
	daysSinceMonday := (int(local.Weekday()) + 6) % 7
	return month, day.AddDate(0, 0, -daysSinceMonday)
}

// TitlePeriodKey returns the current Beijing statistical period, or an empty
// string for lifetime titles.
func TitlePeriodKey(titleKey string, now time.Time) string {
	month, _ := activityTitlePeriodStarts(now)
	switch titleKey {
	case ListeningTitle:
		return month.Format("2006-01")
	default:
		return ""
	}
}

// ListeningTitleRecords is shared by monthly ranking and incumbent eligibility.
func ListeningTitleRecords(db *gorm.DB, now time.Time) *gorm.DB {
	month, _ := activityTitlePeriodStarts(now)
	today := now.In(titleLocation).Format("2006-01-02")
	return db.Model(&model.DailyListeningStat{}).
		Joins("JOIN users ON users.telegram_id = daily_listening_stats.user_id AND users.abs_user_id = daily_listening_stats.abs_user_id").
		Where("users.deleted_at IS NULL AND users.telegram_id > 0 AND users.abs_user_id <> '' AND users.status <> ?", "unbound").
		Where("daily_listening_stats.day_key >= ? AND daily_listening_stats.day_key <= ? AND daily_listening_stats.capped_seconds > 0", month.Format("2006-01-02"), today)
}

// ActivityTitleCandidates reads committed business records. A zero score never
// earns a title. Ties retain only an incumbent from the same statistical period.
func ActivityTitleCandidates(db *gorm.DB, now time.Time) ([]model.RankTitleHolder, error) {
	queries := []struct {
		key, period, score string
		query              *gorm.DB
	}{
		{ListeningTitle, TitlePeriodKey(ListeningTitle, now), "SUM(daily_listening_stats.capped_seconds) / 3600.0",
			ListeningTitleRecords(db, now)},
		{HarvestTitle, "", "SUM(garden_plantings.quantity)",
			db.Model(&model.GardenPlanting{}).
				Joins("JOIN users ON users.telegram_id = garden_plantings.user_id").
				Where("garden_plantings.status = ? AND garden_plantings.harvested_at IS NOT NULL AND garden_plantings.quantity > 0", "harvested")},
		{AlchemyTitle, "", "SUM(garden_alchemy_records.quantity)",
			db.Model(&model.GardenAlchemyRecord{}).
				Joins("JOIN users ON users.telegram_id = garden_alchemy_records.user_id").
				Where("garden_alchemy_records.quantity > 0")},
	}
	candidates := make([]model.RankTitleHolder, 0, len(queries))
	for _, q := range queries {
		var result struct {
			UserID   int64
			UserName string
			Score    float64
		}
		err := q.query.Select("users.telegram_id AS user_id, users.username AS user_name, " + q.score + " AS score").
			Where("users.deleted_at IS NULL AND users.telegram_id > 0").
			Group("users.telegram_id, users.username").Having(q.score + " > 0").
			Order(clause.OrderBy{Expression: clause.Expr{
				SQL:  "score DESC, CASE WHEN users.telegram_id = (SELECT user_id FROM rank_title_holders WHERE title_key = ? AND period_key = ?) THEN 0 ELSE 1 END, users.telegram_id ASC",
				Vars: []any{q.key, q.period},
			}}).Take(&result).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		candidates = append(candidates, model.RankTitleHolder{TitleKey: q.key, UserID: result.UserID, UserName: result.UserName, PeriodKey: q.period, Score: result.Score})
	}
	return candidates, nil
}

// BossTitleCandidate counts the latest two settled events by actual end time.
// Empty events still occupy a slot; older damage must not enter the window.
func BossTitleCandidate(db *gorm.DB, now time.Time) (model.RankTitleHolder, error) {
	var events []model.WorldBossEvent
	err := db.Where("status = ? AND julianday(COALESCE(listening_cutoff_at, end_at)) <= julianday(?)", "settled", now).
		Order("julianday(COALESCE(listening_cutoff_at, end_at)) DESC, id DESC").Limit(2).Find(&events).Error
	if err != nil {
		return model.RankTitleHolder{}, err
	}
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.BossID)
	}
	next := model.RankTitleHolder{TitleKey: BossTitle}
	err = db.Model(&model.WorldBossParticipant{}).
		Joins("JOIN users ON users.telegram_id = world_boss_participants.user_id AND users.deleted_at IS NULL AND users.telegram_id > 0").
		Where("world_boss_participants.boss_id IN ? AND world_boss_participants.is_rewarded = ? AND world_boss_participants.damage > 0", ids, true).
		Select("users.telegram_id AS user_id, users.username AS user_name, ROUND(SUM(world_boss_participants.damage), 2) AS score").
		Group("users.telegram_id, users.username").Having("ROUND(SUM(world_boss_participants.damage), 2) > 0").
		Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  "score DESC, CASE WHEN users.telegram_id = (SELECT user_id FROM rank_title_holders WHERE title_key = ?) THEN 0 ELSE 1 END, users.telegram_id ASC",
			Vars: []any{BossTitle},
		}}).Take(&next).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return next, err
}

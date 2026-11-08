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
	month, week := activityTitlePeriodStarts(now)
	switch titleKey {
	case ListeningTitle:
		return month.Format("2006-01")
	case BossTitle:
		return week.Format("2006-01-02")
	default:
		return ""
	}
}

// ActivityTitleCandidates reads committed business records. A zero score never
// earns a title. Ties retain only an incumbent from the same statistical period.
func ActivityTitleCandidates(db *gorm.DB, now time.Time) ([]model.RankTitleHolder, error) {
	month, week := activityTitlePeriodStarts(now)
	today := now.In(titleLocation).Format("2006-01-02")
	queries := []struct {
		key, period, score string
		query              *gorm.DB
	}{
		{ListeningTitle, TitlePeriodKey(ListeningTitle, now), "SUM(daily_listening_stats.capped_seconds) / 3600.0",
			db.Model(&model.DailyListeningStat{}).
				Joins("JOIN users ON users.telegram_id = daily_listening_stats.user_id AND users.abs_user_id = daily_listening_stats.abs_user_id").
				Where("users.abs_user_id <> '' AND users.status <> ?", "unbound").
				Where("daily_listening_stats.day_key >= ? AND daily_listening_stats.day_key <= ? AND daily_listening_stats.capped_seconds > 0", month.Format("2006-01-02"), today)},
		{BossTitle, TitlePeriodKey(BossTitle, now), "ROUND(SUM(world_boss_participants.damage), 2)",
			db.Model(&model.WorldBossParticipant{}).
				Joins("JOIN users ON users.telegram_id = world_boss_participants.user_id").
				Joins("JOIN world_boss_events e ON e.boss_id = world_boss_participants.boss_id AND e.deleted_at IS NULL").
				// SQLite 日期值可能保存 UTC 或北京时间偏移；按时间点比较，
				// 避免周一零点把不同时区的同一时间误分到上周。
				// 提前击杀按冻结的实际截止时间计榜，重试结算不改变归属周。
				Where("e.status = ? AND julianday(COALESCE(e.listening_cutoff_at, e.end_at)) >= julianday(?) AND julianday(COALESCE(e.listening_cutoff_at, e.end_at)) < julianday(?) AND julianday(COALESCE(e.listening_cutoff_at, e.end_at)) <= julianday(?)", "settled", week, week.AddDate(0, 0, 7), now).
				Where("world_boss_participants.is_rewarded = ? AND world_boss_participants.damage > 0", true)},
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

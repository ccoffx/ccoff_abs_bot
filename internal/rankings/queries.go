// Package rankings shares leaderboard eligibility and ordering with honorary titles.
package rankings

import (
	"gorm.io/gorm"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
)

const (
	WealthTitle      = "wealth_first"
	CultivationTitle = "cultivation_first"
	SectTitle        = "sect_first"
	ListeningTitle   = "listening_first"
	BossTitle        = "boss_first"
	HarvestTitle     = "harvest_first"
	AlchemyTitle     = "alchemy_first"
)

func WealthBase(db *gorm.DB) *gorm.DB {
	query := db.Model(&model.User{}).Where("role NOT IN ? AND telegram_id > 0", []string{"super_admin", "admin"})
	if config.AppConfig != nil {
		ids := make([]int64, 0, len(config.AppConfig.AdminIDs))
		for id := range config.AppConfig.AdminIDs {
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			query = query.Where("telegram_id NOT IN ?", ids)
		}
	}
	return query
}

func Wealth(db *gorm.DB) *gorm.DB {
	return WealthBase(db).Order("points DESC").
		Order("CASE WHEN telegram_id = (SELECT user_id FROM rank_title_holders WHERE title_key = 'wealth_first') THEN 0 ELSE 1 END").
		Order("telegram_id ASC")
}

func Cultivation(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Cultivation{}).
		Joins("JOIN users ON users.telegram_id = cultivations.user_id AND users.deleted_at IS NULL AND users.telegram_id > 0").
		Order("cultivations.major_realm DESC, cultivations.minor_realm DESC, (cultivations.total_audio_time + cultivations.pill_audio_time) DESC, cultivations.total_audio_time DESC").
		Order("CASE WHEN cultivations.user_id = (SELECT user_id FROM rank_title_holders WHERE title_key = 'cultivation_first') THEN 0 ELSE 1 END").
		Order("cultivations.user_id ASC")
}

func Sects(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Sect{}).Order("level DESC, prestige DESC, funds DESC, member_count DESC").
		Order("CASE WHEN id = (SELECT subject_id FROM rank_title_holders WHERE title_key = 'sect_first') THEN 0 ELSE 1 END").
		Order("id ASC")
}

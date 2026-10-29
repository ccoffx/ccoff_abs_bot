package storage

import (
	"fmt"

	"gorm.io/gorm"
)

// 当前系统的数据库约束。启动时幂等建索引，不转换或修补历史数据。
var DatabaseIndexes = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_lottery_active_join_keyword
  ON lottery_activities(join_keyword)
  WHERE join_keyword <> '' AND status IN ('active', 'drawing') AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_breakthrough_ritual_active_user
  ON breakthrough_rituals(user_id)
  WHERE status IN ('publishing', 'waiting') AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_treasure_active_chat
  ON treasure_rounds(chat_id)
  WHERE status IN ('publishing', 'active');`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_cultivations_user_unique
  ON cultivations(user_id);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_red_packet_grabs_packet_user_unique
  ON red_packet_grabs(packet_id, user_id);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sign_in_logs_user_date_unique
  ON sign_in_logs(user_id, sign_date)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sign_in_reward_claims_ref_unique
  ON sign_in_reward_claims(ref_id)
  WHERE ref_id <> '' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_daily_activation_quotas_unique
  ON referral_daily_activation_quotas(inviter_id, day_key);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_monthly_reward_quotas_unique
  ON referral_monthly_reward_quotas(inviter_id, month_key);`,
	`CREATE INDEX IF NOT EXISTS idx_book_requests_status_last_action
  ON book_requests(status, last_action_at);`,
	`CREATE INDEX IF NOT EXISTS idx_book_requests_assignee_status
  ON book_requests(assignee_id, status);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_lottery_reminders_lottery_user_unique
  ON sect_lottery_reminders(lottery_id, user_id);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_cultivation_minor_realm_configs_unique
  ON cultivation_minor_realm_configs(major_realm, minor_realm)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_cultivation_realm_configs_major_realm
  ON cultivation_realm_configs(major_realm)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_breakthrough_configs_from_major_realm
  ON breakthrough_configs(from_major_realm)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_abs_user_id_unique
  ON users(abs_user_id)
  WHERE abs_user_id <> '' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sects_name
  ON sects(name)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_members_user_id
  ON sect_members(user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_system_configs_key
  ON system_configs(key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_garden_seed_purchases_unique
  ON garden_seed_purchases(user_id, seed_key, day_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_garden_herb_market_sales_unique
  ON garden_herb_market_sales(user_id, seed_key, day_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_garden_recipe_unlocks_unique
  ON garden_recipe_unlocks(user_id, recipe_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_item_usage_quotas_unique
  ON item_usage_quotas(user_id, item_name, period_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_shop_renew_claim_slot
  ON sect_shop_renew_claims(sect_id, month_key, slot_no)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_shop_renew_claim_user_month
  ON sect_shop_renew_claims(user_id, month_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sign_in_streaks_user_unique
  ON sign_in_streaks(user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_codes_user_unique
  ON referral_codes(user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_codes_code_unique
  ON referral_codes(code)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_referral_activations_invitee_unique
  ON referral_activations(invitee_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_daily_task_claims_user_day_unique
  ON sect_daily_task_claims(user_id, day_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_weekly_task_settlements_sect_week_unique
  ON sect_weekly_task_settlements(sect_id, week_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_secret_realm_participants_realm_user_unique
  ON sect_secret_realm_participants(realm_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_horn_broadcasts_horn_id
  ON sect_horn_broadcasts(horn_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_horn_deliveries_horn_user_unique
  ON sect_horn_deliveries(horn_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_world_boss_events_boss_id
  ON world_boss_events(boss_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_world_boss_participants_boss_user_unique
  ON world_boss_participants(boss_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_lottery_participants_activity_user_unique
  ON lottery_participants(activity_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_lottery_winners_activity_user_unique
  ON lottery_winners(activity_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_listening_abuse_records_user_day_action_unique
  ON listening_abuse_records(user_id, day_key, action)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_listening_abuse_records_active_user_unique
  ON listening_abuse_records(user_id)
  WHERE deleted_at IS NULL AND action = 'freeze' AND status = 'active';`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_marketplace_purchases_secret_unique
  ON marketplace_purchases(secret_id)
  WHERE secret_id > 0 AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_lottery_entries_lottery_user_unique
  ON sect_lottery_entries(lottery_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_lottery_winners_lottery_user_unique
  ON sect_lottery_winners(lottery_id, user_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_lottery_winners_lottery_prize_unique
  ON sect_lottery_winners(lottery_id, prize_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_invite_codes_code_hash_unique
  ON invite_codes(code_hash)
  WHERE code_hash <> '' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_renew_codes_code_hash_unique
  ON renew_codes(code_hash)
  WHERE code_hash <> '' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_security_attempt_locks_user_purpose_unique
  ON security_attempt_locks(user_id, purpose)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_garden_plots_user_plot_unique
  ON garden_plots(user_id, plot_no)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_garden_plantings_active_plot_unique
  ON garden_plantings(plot_id)
  WHERE deleted_at IS NULL AND status = 'growing';`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_cave_retreats_active_user_unique
  ON sect_cave_retreats(user_id)
  WHERE deleted_at IS NULL AND status = 'active';`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_inventory_user_item_unique
  ON inventories(user_id, item_name)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_technologies_sect_key_unique
  ON sect_technologies(sect_id, tech_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_secret_realm_events_realm_id
  ON sect_secret_realm_events(realm_id)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_secret_realm_events_active_sect_unique
  ON sect_secret_realm_events(sect_id)
  WHERE status = 'active' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sect_listening_daily_progresses_user_day_unique
  ON sect_listening_daily_progresses(user_id, day_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_daily_listening_stats_user_day_unique
  ON daily_listening_stats(user_id, day_key)
  WHERE deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_marketplace_disputes_open_purchase_buyer_unique
  ON marketplace_disputes(purchase_id, buyer_id)
  WHERE status = 'open' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_marketplace_secrets_active_code_hash_unique
  ON marketplace_secrets(code_hash)
  WHERE code_hash <> '' AND status = 'available' AND deleted_at IS NULL;`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_open_registration_single_active
  ON open_registration_campaigns(status)
  WHERE status = 'active' AND deleted_at IS NULL;`,
}

func InitializeDatabaseIndexes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("DB_EMPTY")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, sql := range DatabaseIndexes {
			if err := tx.Exec(sql).Error; err != nil {
				return fmt.Errorf("create database index: %w; sql=%s", err, sql)
			}
		}
		return nil
	})
}

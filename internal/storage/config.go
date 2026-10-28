package storage

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func GetConfigIntFromDBChecked(db *gorm.DB, key string, defaultVal int) (int, error) {
	if db == nil {
		return defaultVal, nil
	}

	var cfg model.SystemConfig
	err := db.Where("key = ?", key).First(&cfg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && strings.TrimSpace(cfg.Value) == "") {
		return defaultVal, nil
	}
	if err != nil {
		return defaultVal, err
	}
	val, err := strconv.Atoi(strings.TrimSpace(cfg.Value))
	if err != nil {
		return defaultVal, fmt.Errorf("invalid integer config %s=%q: %w", key, cfg.Value, err)
	}
	return val, nil
}

func GetConfigIntChecked(key string, defaultVal int) (int, error) {
	return GetConfigIntFromDBChecked(DB, key, defaultVal)
}

const (
	DailyOperationsStartHour     = 3
	DailyFusionPoolCollectHour   = 12
	SectWeeklyTaskAutoSettleHour = 9

	AutoBackupLastSuccessDateKey = "auto_backup_last_success_date"
	AutoBackupLastSuccessAtKey   = "auto_backup_last_success_at"
	AutoBackupLastMessageIDKey   = "auto_backup_last_message_id"
	AutoBackupLastAttemptAtKey   = "auto_backup_last_attempt_at"
	AutoBackupLastErrorKey       = "auto_backup_last_error"
	AutoBackupRetryCountKey      = "auto_backup_retry_count"
	BackupLastPinnedMessageIDKey = "backup_last_pinned_message_id"
	BackupLastPinErrorKey        = "backup_last_pin_error"

	DailyLifecycleLastSuccessDateKey    = "daily_lifecycle_last_success_date"
	DailyLifecycleLastErrorKey          = "daily_lifecycle_last_error"
	DailyListeningRefreshLastAtKey      = "daily_listening_refresh_last_at"
	DailyListeningRefreshLastSuccessKey = "daily_listening_refresh_last_success"
	DailyListeningRefreshLastTotalKey   = "daily_listening_refresh_last_total"
	DailyListeningRefreshLastSkippedKey = "daily_listening_refresh_last_skipped"
	DailyListeningRefreshLastErrorKey   = "daily_listening_refresh_last_error"
	DailyFusionPoolLastSuccessDateKey   = "daily_fusion_pool_last_success_date"
	DailyFusionPoolLastSuccessAtKey     = "daily_fusion_pool_last_success_at"
	DailyFusionPoolLastAmountKey        = "daily_fusion_pool_last_amount"
	DailyFusionPoolLastErrorKey         = "daily_fusion_pool_last_error"
	SectWeeklyTaskAutoSettleLastWeekKey = "sect_weekly_task_auto_settle_last_week"

	AutoBackupMaxRetryCount           = 5
	DailyListeningRefreshInterval     = 30 * time.Minute
	DailyListeningRefreshFreshTTL     = 5 * time.Minute
	DailyListeningRefreshRequestDelay = 80 * time.Millisecond
)

var DailyOperationsLocation = time.FixedZone("CST", 8*3600)

func systemConfigKeyConflictTarget() clause.Where {
	return clause.Where{Exprs: []clause.Expression{
		clause.Eq{Column: clause.Column{Name: "deleted_at"}, Value: nil},
	}}
}

func SystemConfigKeyDoNothingClause() clause.OnConflict {
	return clause.OnConflict{
		Columns:     []clause.Column{{Name: "key"}},
		TargetWhere: systemConfigKeyConflictTarget(),
		DoNothing:   true,
	}
}

func UpsertSystemConfigValueInTx(tx *gorm.DB, key string, value string) error {
	if tx == nil || strings.TrimSpace(key) == "" {
		return fmt.Errorf("SYSTEM_CONFIG_UPSERT_INVALID")
	}

	res := tx.Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "key"}},
		TargetWhere: systemConfigKeyConflictTarget(),
		DoUpdates: clause.Assignments(map[string]interface{}{
			"value":      value,
			"updated_at": time.Now(),
		}),
	}).Create(&model.SystemConfig{
		Key:   key,
		Value: value,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SYSTEM_CONFIG_UPSERT_MISSED")
	}
	return nil
}

func DailyOperationsLocalTime(now time.Time) time.Time {
	return now.In(DailyOperationsLocation)
}

func DailyOperationsDateKey(now time.Time) string {
	return DailyOperationsLocalTime(now).Format("2006-01-02")
}

func GetSystemConfigStringForStatus(key, emptyText string) (string, bool) {
	value, err := GetSystemConfigStringChecked(key)
	if err != nil {
		log.Printf("⚠️ 系统配置状态读取失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
		return "读取失败", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return emptyText, true
	}
	return value, true
}

func FormatSystemConfigTimeForStatus(key string) (string, bool) {
	t, err := GetSystemConfigTimeChecked(key)
	if err != nil {
		log.Printf("⚠️ 系统配置时间状态读取失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
		return "读取失败", false
	}
	if t.IsZero() {
		return "无", true
	}
	return DailyOperationsLocalTime(t).Format("2006-01-02 15:04:05"), true
}

func GetSystemConfigStringChecked(key string) (string, error) {
	if DB == nil || strings.TrimSpace(key) == "" {
		return "", nil
	}

	var cfg model.SystemConfig
	if err := DB.Where("key = ?", key).First(&cfg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}

	return cfg.Value, nil
}

func setSystemConfigString(key string, value string) {
	if err := SetSystemConfigStringChecked(key, value); err != nil {
		log.Printf("⚠️ 写入系统配置失败: key=%s err=%s", textfmt.FormatPlainValue(key), textfmt.FormatPlainError(err))
	}
}

func SetSystemConfigStringChecked(key string, value string) error {
	if DB == nil || strings.TrimSpace(key) == "" {
		return nil
	}

	res := DB.Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "key"}},
		TargetWhere: systemConfigKeyConflictTarget(),
		DoUpdates: clause.Assignments(map[string]interface{}{
			"value":      value,
			"updated_at": time.Now(),
		}),
	}).Create(&model.SystemConfig{
		Key:   key,
		Value: value,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("SYSTEM_CONFIG_UPSERT_MISSED")
	}
	return nil
}

func SetSystemConfigError(key string, err error) {
	if err == nil {
		setSystemConfigString(key, "")
		return
	}
	setSystemConfigString(key, textfmt.FormatPlainError(err))
}

func GetSystemConfigTimeChecked(key string) (time.Time, error) {
	raw, err := GetSystemConfigStringChecked(key)
	if err != nil {
		return time.Time{}, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid system config time %s=%q: %w", key, raw, err)
	}

	return t, nil
}

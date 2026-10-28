package game

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

type CultivationRuleSet struct {
	Realms        map[int]model.CultivationRealmConfig
	MinorRealms   map[int][]model.CultivationMinorRealmConfig
	Breakthroughs map[int]model.BreakthroughConfig
	LoadedAt      time.Time
	Source        string
}

var cultivationRuleCache = struct {
	mu    sync.RWMutex
	rules *CultivationRuleSet
}{}

func InitCultivationRuleCache() {
	if err := ReloadCultivationRules(); err != nil {
		log.Printf("⚠️ 斗气配置首次加载失败，使用内置默认规则兜底: %s", textfmt.FormatPlainError(err))

		cultivationRuleCache.mu.Lock()
		cultivationRuleCache.rules = buildDefaultCultivationRuleSet("builtin_fallback")
		cultivationRuleCache.mu.Unlock()
	}

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			if err := ReloadCultivationRules(); err != nil {
				log.Printf("⚠️ 斗气配置自动刷新失败，继续使用上一版缓存: %s", textfmt.FormatPlainError(err))
			}
		}
	}()
}

func GetCultivationRules() *CultivationRuleSet {
	cultivationRuleCache.mu.RLock()
	defer cultivationRuleCache.mu.RUnlock()

	if cultivationRuleCache.rules == nil {
		return buildDefaultCultivationRuleSet("builtin_fallback")
	}

	return cloneCultivationRuleSet(cultivationRuleCache.rules)
}

func ReloadCultivationRules() error {
	rules, err := loadCultivationRulesFromDB()
	if err != nil {
		return err
	}

	if err := ValidateCultivationRuleSet(rules); err != nil {
		return err
	}

	cultivationRuleCache.mu.Lock()
	cultivationRuleCache.rules = rules
	cultivationRuleCache.mu.Unlock()

	log.Printf("✅ 斗气配置缓存已刷新: realms=%d breakthroughs=%d source=%s",
		len(rules.Realms),
		len(rules.Breakthroughs),
		textfmt.FormatPlainValue(rules.Source),
	)

	return nil
}

func loadCultivationRulesFromDB() (*CultivationRuleSet, error) {
	if storage.DB == nil {
		return nil, errors.New("database is not initialized")
	}

	return LoadCultivationRulesFromDBHandle(storage.DB)
}

func LoadCultivationRulesFromDBHandle(db *gorm.DB) (*CultivationRuleSet, error) {
	if db == nil {
		return nil, errors.New("database is not initialized")
	}

	var realms []model.CultivationRealmConfig
	if err := db.Where("enabled = ?", true).Order("major_realm ASC").Find(&realms).Error; err != nil {
		return nil, fmt.Errorf("load realm configs failed: %w", err)
	}

	var minorRealms []model.CultivationMinorRealmConfig
	if err := db.Where("enabled = ?", true).Order("major_realm ASC, minor_realm ASC").Find(&minorRealms).Error; err != nil {
		return nil, fmt.Errorf("load minor realm configs failed: %w", err)
	}

	var breakthroughs []model.BreakthroughConfig
	if err := db.Where("enabled = ?", true).Order("from_major_realm ASC").Find(&breakthroughs).Error; err != nil {
		return nil, fmt.Errorf("load breakthrough configs failed: %w", err)
	}

	rules := &CultivationRuleSet{
		Realms:        make(map[int]model.CultivationRealmConfig),
		MinorRealms:   make(map[int][]model.CultivationMinorRealmConfig),
		Breakthroughs: make(map[int]model.BreakthroughConfig),
		LoadedAt:      time.Now(),
		Source:        "database",
	}

	for _, realm := range realms {
		rules.Realms[realm.MajorRealm] = realm
	}

	for _, minor := range minorRealms {
		rules.MinorRealms[minor.MajorRealm] = append(rules.MinorRealms[minor.MajorRealm], minor)
	}

	for major := range rules.MinorRealms {
		sort.Slice(rules.MinorRealms[major], func(i, j int) bool {
			return rules.MinorRealms[major][i].MinorRealm < rules.MinorRealms[major][j].MinorRealm
		})
	}

	for _, breakthrough := range breakthroughs {
		rules.Breakthroughs[breakthrough.FromMajorRealm] = breakthrough
	}

	return rules, nil
}

func isInvalidRuleFloat(value float64) bool {
	return math.IsNaN(value) || math.IsInf(value, 0)
}

func ValidateCultivationRuleSet(rules *CultivationRuleSet) error {
	if rules == nil {
		return errors.New("cultivation rules are nil")
	}

	if len(rules.Realms) == 0 {
		return errors.New("realm configs are empty")
	}

	maxRealmCount := 0
	for major, realm := range rules.Realms {
		if major != realm.MajorRealm {
			return fmt.Errorf("realm map key mismatch: key=%d value=%d", major, realm.MajorRealm)
		}

		if realm.Name == "" {
			return fmt.Errorf("realm name is empty: major=%d", major)
		}

		if isInvalidRuleFloat(realm.MinTotalHours) || isInvalidRuleFloat(realm.MaxTotalHours) {
			return fmt.Errorf("realm hours must be finite numbers: major=%d", major)
		}

		if realm.MinTotalHours < 0 || realm.MaxTotalHours < 0 {
			return fmt.Errorf("realm hours cannot be negative: major=%d", major)
		}

		if realm.MaxTotalHours > 0 && realm.MaxTotalHours < realm.MinTotalHours {
			return fmt.Errorf("realm max hours smaller than min hours: major=%d", major)
		}

		if realm.IsMaxRealm {
			maxRealmCount++
		}
	}

	if maxRealmCount != 1 {
		return fmt.Errorf("there must be exactly one max realm, got %d", maxRealmCount)
	}

	for major, minors := range rules.MinorRealms {
		if _, ok := rules.Realms[major]; !ok {
			return fmt.Errorf("minor realm references missing major realm: major=%d", major)
		}

		seen := make(map[int]bool)
		lastHours := -1.0

		for _, minor := range minors {
			if minor.MinorRealm < 1 || minor.MinorRealm > 4 {
				return fmt.Errorf("minor realm must be 1-4: major=%d minor=%d", major, minor.MinorRealm)
			}

			if seen[minor.MinorRealm] {
				return fmt.Errorf("duplicate minor realm: major=%d minor=%d", major, minor.MinorRealm)
			}
			seen[minor.MinorRealm] = true

			if minor.Name == "" {
				return fmt.Errorf("minor realm name is empty: major=%d minor=%d", major, minor.MinorRealm)
			}

			if isInvalidRuleFloat(minor.RequiredHours) {
				return fmt.Errorf("minor realm required hours must be finite number: major=%d minor=%d", major, minor.MinorRealm)
			}

			if minor.RequiredHours < 0 {
				return fmt.Errorf("minor realm required hours cannot be negative: major=%d minor=%d", major, minor.MinorRealm)
			}

			if lastHours >= 0 && minor.RequiredHours < lastHours {
				return fmt.Errorf("minor realm required hours must be ascending: major=%d minor=%d", major, minor.MinorRealm)
			}
			lastHours = minor.RequiredHours
		}
	}

	for fromMajor, breakthrough := range rules.Breakthroughs {
		if fromMajor != breakthrough.FromMajorRealm {
			return fmt.Errorf("breakthrough map key mismatch: key=%d value=%d", fromMajor, breakthrough.FromMajorRealm)
		}

		if _, ok := rules.Realms[breakthrough.FromMajorRealm]; !ok {
			return fmt.Errorf("breakthrough references missing from realm: from=%d", breakthrough.FromMajorRealm)
		}

		if _, ok := rules.Realms[breakthrough.ToMajorRealm]; !ok {
			return fmt.Errorf("breakthrough references missing to realm: to=%d", breakthrough.ToMajorRealm)
		}

		if breakthrough.ToMajorRealm <= breakthrough.FromMajorRealm {
			return fmt.Errorf("breakthrough target must be greater than source: from=%d to=%d", breakthrough.FromMajorRealm, breakthrough.ToMajorRealm)
		}

		if breakthrough.PointsCost < 0 {
			return fmt.Errorf("breakthrough points cost cannot be negative: from=%d", breakthrough.FromMajorRealm)
		}

		if isInvalidRuleFloat(breakthrough.MinTotalHours) {
			return fmt.Errorf("breakthrough min hours must be finite number: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.MinTotalHours < 0 {
			return fmt.Errorf("breakthrough min hours cannot be negative: from=%d", breakthrough.FromMajorRealm)
		}

		if isInvalidRuleFloat(breakthrough.SuccessRate) {
			return fmt.Errorf("breakthrough success rate must be finite number: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.SuccessRate < 0 || breakthrough.SuccessRate > 1 {
			return fmt.Errorf("breakthrough success rate must be between 0 and 1: from=%d", breakthrough.FromMajorRealm)
		}

		if isInvalidRuleFloat(breakthrough.RefundRate) {
			return fmt.Errorf("breakthrough refund rate must be finite number: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.CooldownHours < 0 {
			return fmt.Errorf("breakthrough cooldown cannot be negative: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.GuaranteeFailCount < 0 {
			return fmt.Errorf("breakthrough guarantee fail count cannot be negative: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.RefundRate < 0 || breakthrough.RefundRate > 1 {
			return fmt.Errorf("breakthrough refund rate must be between 0 and 1: from=%d", breakthrough.FromMajorRealm)
		}

		if breakthrough.FailPenaltyPoints < 0 ||
			breakthrough.SplashMinMajorRealm < 0 ||
			breakthrough.SplashVictimCount < 0 ||
			breakthrough.SplashPenaltyPoints < 0 {
			return fmt.Errorf("breakthrough penalty settings cannot be negative: from=%d", breakthrough.FromMajorRealm)
		}
	}

	return nil
}

func cloneCultivationRuleSet(src *CultivationRuleSet) *CultivationRuleSet {
	if src == nil {
		return nil
	}

	dst := &CultivationRuleSet{
		Realms:        make(map[int]model.CultivationRealmConfig, len(src.Realms)),
		MinorRealms:   make(map[int][]model.CultivationMinorRealmConfig, len(src.MinorRealms)),
		Breakthroughs: make(map[int]model.BreakthroughConfig, len(src.Breakthroughs)),
		LoadedAt:      src.LoadedAt,
		Source:        src.Source,
	}

	for k, v := range src.Realms {
		dst.Realms[k] = v
	}

	for k, v := range src.MinorRealms {
		copied := make([]model.CultivationMinorRealmConfig, len(v))
		copy(copied, v)
		dst.MinorRealms[k] = copied
	}

	for k, v := range src.Breakthroughs {
		dst.Breakthroughs[k] = v
	}

	return dst
}

func buildDefaultCultivationRuleSet(source string) *CultivationRuleSet {
	rules := &CultivationRuleSet{
		Realms:        make(map[int]model.CultivationRealmConfig),
		MinorRealms:   make(map[int][]model.CultivationMinorRealmConfig),
		Breakthroughs: make(map[int]model.BreakthroughConfig),
		LoadedAt:      time.Now(),
		Source:        source,
	}

	for _, realm := range defaultCultivationRealmConfigs() {
		rules.Realms[realm.MajorRealm] = realm
	}

	for _, minor := range defaultCultivationMinorRealmConfigs() {
		rules.MinorRealms[minor.MajorRealm] = append(rules.MinorRealms[minor.MajorRealm], minor)
	}

	for major := range rules.MinorRealms {
		sort.Slice(rules.MinorRealms[major], func(i, j int) bool {
			return rules.MinorRealms[major][i].MinorRealm < rules.MinorRealms[major][j].MinorRealm
		})
	}

	for _, breakthrough := range defaultBreakthroughConfigs() {
		rules.Breakthroughs[breakthrough.FromMajorRealm] = breakthrough
	}

	return rules
}

func SeedDefaultCultivationConfigs() error {
	return seedDefaultCultivationConfigsWithHandle(storage.DB)
}

// seedDefaultCultivationConfigsWithHandle 幂等地补齐缺失的默认境界/小境界/突破配置行，
// 已存在的行一律不覆盖（避免覆盖超管自定义值）。启动时调用。
func seedDefaultCultivationConfigsWithHandle(db *gorm.DB) error {
	if db == nil {
		return errors.New("database is not initialized")
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := seedDefaultRealmConfigs(tx); err != nil {
			return err
		}

		if err := seedDefaultMinorRealmConfigs(tx); err != nil {
			return err
		}

		if err := seedDefaultBreakthroughConfigs(tx); err != nil {
			return err
		}

		return nil
	})
}

func seedDefaultRealmConfigs(tx *gorm.DB) error {
	defaults := defaultCultivationRealmConfigs()
	for _, cfg := range defaults {
		var existing model.CultivationRealmConfig
		err := tx.Where("major_realm = ?", cfg.MajorRealm).First(&existing).Error
		if err == nil {
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := createDefaultCultivationRealmConfigIfMissingInTx(tx, &cfg); err != nil {
			return err
		}
	}

	return nil
}

func createDefaultCultivationRealmConfigIfMissingInTx(tx *gorm.DB, cfg *model.CultivationRealmConfig) error {
	if tx == nil || cfg == nil {
		return fmt.Errorf("CULTIVATION_REALM_CONFIG_INVALID")
	}
	entry := *cfg
	if entry.MajorRealm < 0 || strings.TrimSpace(entry.Name) == "" {
		return fmt.Errorf("CULTIVATION_REALM_CONFIG_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*cfg = entry
	return nil
}

func defaultCultivationRealmConfigs() []model.CultivationRealmConfig {
	return []model.CultivationRealmConfig{
		{MajorRealm: 0, Name: "斗之气", TitleName: "斗之气", MinTotalHours: 0, MaxTotalHours: 10, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 1, Name: "斗者", TitleName: "斗者", MinTotalHours: 10, MaxTotalHours: 50, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 2, Name: "斗师", TitleName: "斗师", MinTotalHours: 50, MaxTotalHours: 150, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 3, Name: "大斗师", TitleName: "大斗师", MinTotalHours: 150, MaxTotalHours: 350, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 4, Name: "斗灵", TitleName: "斗灵", MinTotalHours: 350, MaxTotalHours: 750, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 5, Name: "斗王", TitleName: "斗王", MinTotalHours: 750, MaxTotalHours: 1500, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 6, Name: "斗皇", TitleName: "斗皇", MinTotalHours: 1500, MaxTotalHours: 2400, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 7, Name: "斗宗", TitleName: "斗宗", MinTotalHours: 2400, MaxTotalHours: 3700, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 8, Name: "斗尊", TitleName: "斗尊", MinTotalHours: 3700, MaxTotalHours: 5500, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 9, Name: "九转斗尊", TitleName: "九转斗尊", MinTotalHours: 5500, MaxTotalHours: 8100, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 10, Name: "半圣", TitleName: "半圣", MinTotalHours: 8100, MaxTotalHours: 11500, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 11, Name: "斗圣", TitleName: "斗圣", MinTotalHours: 11500, MaxTotalHours: 16100, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 12, Name: "半帝", TitleName: "半帝", MinTotalHours: 16100, MaxTotalHours: 22300, IsMaxRealm: false, Enabled: true},
		{MajorRealm: 13, Name: "斗帝", TitleName: "斗帝", MinTotalHours: 22300, MaxTotalHours: 0, IsMaxRealm: true, Enabled: true},
	}
}

func seedDefaultMinorRealmConfigs(tx *gorm.DB) error {
	defaults := defaultCultivationMinorRealmConfigs()

	for _, cfg := range defaults {
		var existing model.CultivationMinorRealmConfig
		err := tx.Where("major_realm = ? AND minor_realm = ?", cfg.MajorRealm, cfg.MinorRealm).First(&existing).Error
		if err == nil {
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := createDefaultCultivationMinorRealmConfigIfMissingInTx(tx, &cfg); err != nil {
			return err
		}
	}

	return nil
}

func createDefaultCultivationMinorRealmConfigIfMissingInTx(tx *gorm.DB, cfg *model.CultivationMinorRealmConfig) error {
	if tx == nil || cfg == nil {
		return fmt.Errorf("CULTIVATION_MINOR_REALM_CONFIG_INVALID")
	}
	entry := *cfg
	if entry.MajorRealm <= 0 || entry.MinorRealm <= 0 || strings.TrimSpace(entry.Name) == "" {
		return fmt.Errorf("CULTIVATION_MINOR_REALM_CONFIG_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*cfg = entry
	return nil
}

func defaultCultivationMinorRealmConfigs() []model.CultivationMinorRealmConfig {
	return []model.CultivationMinorRealmConfig{
		{MajorRealm: 1, MinorRealm: 1, Name: "初期", RequiredHours: 10, Enabled: true},
		{MajorRealm: 1, MinorRealm: 2, Name: "中期", RequiredHours: 20, Enabled: true},
		{MajorRealm: 1, MinorRealm: 3, Name: "后期", RequiredHours: 30, Enabled: true},
		{MajorRealm: 1, MinorRealm: 4, Name: "圆满", RequiredHours: 40, Enabled: true},

		{MajorRealm: 2, MinorRealm: 1, Name: "初期", RequiredHours: 50, Enabled: true},
		{MajorRealm: 2, MinorRealm: 2, Name: "中期", RequiredHours: 75, Enabled: true},
		{MajorRealm: 2, MinorRealm: 3, Name: "后期", RequiredHours: 100, Enabled: true},
		{MajorRealm: 2, MinorRealm: 4, Name: "圆满", RequiredHours: 125, Enabled: true},

		{MajorRealm: 3, MinorRealm: 1, Name: "初期", RequiredHours: 150, Enabled: true},
		{MajorRealm: 3, MinorRealm: 2, Name: "中期", RequiredHours: 200, Enabled: true},
		{MajorRealm: 3, MinorRealm: 3, Name: "后期", RequiredHours: 250, Enabled: true},
		{MajorRealm: 3, MinorRealm: 4, Name: "圆满", RequiredHours: 300, Enabled: true},

		{MajorRealm: 4, MinorRealm: 1, Name: "初期", RequiredHours: 350, Enabled: true},
		{MajorRealm: 4, MinorRealm: 2, Name: "中期", RequiredHours: 450, Enabled: true},
		{MajorRealm: 4, MinorRealm: 3, Name: "后期", RequiredHours: 550, Enabled: true},
		{MajorRealm: 4, MinorRealm: 4, Name: "圆满", RequiredHours: 650, Enabled: true},

		{MajorRealm: 5, MinorRealm: 1, Name: "初期", RequiredHours: 750, Enabled: true},
		{MajorRealm: 5, MinorRealm: 2, Name: "中期", RequiredHours: 900, Enabled: true},
		{MajorRealm: 5, MinorRealm: 3, Name: "后期", RequiredHours: 1050, Enabled: true},
		{MajorRealm: 5, MinorRealm: 4, Name: "圆满", RequiredHours: 1200, Enabled: true},

		{MajorRealm: 6, MinorRealm: 1, Name: "初期", RequiredHours: 1500, Enabled: true},
		{MajorRealm: 6, MinorRealm: 2, Name: "中期", RequiredHours: 1700, Enabled: true},
		{MajorRealm: 6, MinorRealm: 3, Name: "后期", RequiredHours: 1900, Enabled: true},
		{MajorRealm: 6, MinorRealm: 4, Name: "圆满", RequiredHours: 2100, Enabled: true},

		{MajorRealm: 7, MinorRealm: 1, Name: "初期", RequiredHours: 2400, Enabled: true},
		{MajorRealm: 7, MinorRealm: 2, Name: "中期", RequiredHours: 2700, Enabled: true},
		{MajorRealm: 7, MinorRealm: 3, Name: "后期", RequiredHours: 3000, Enabled: true},
		{MajorRealm: 7, MinorRealm: 4, Name: "圆满", RequiredHours: 3300, Enabled: true},

		{MajorRealm: 8, MinorRealm: 1, Name: "初期", RequiredHours: 3700, Enabled: true},
		{MajorRealm: 8, MinorRealm: 2, Name: "中期", RequiredHours: 4100, Enabled: true},
		{MajorRealm: 8, MinorRealm: 3, Name: "后期", RequiredHours: 4500, Enabled: true},
		{MajorRealm: 8, MinorRealm: 4, Name: "圆满", RequiredHours: 5000, Enabled: true},

		{MajorRealm: 9, MinorRealm: 1, Name: "初期", RequiredHours: 5500, Enabled: true},
		{MajorRealm: 9, MinorRealm: 2, Name: "中期", RequiredHours: 6100, Enabled: true},
		{MajorRealm: 9, MinorRealm: 3, Name: "后期", RequiredHours: 6700, Enabled: true},
		{MajorRealm: 9, MinorRealm: 4, Name: "圆满", RequiredHours: 7400, Enabled: true},

		{MajorRealm: 10, MinorRealm: 1, Name: "初期", RequiredHours: 8100, Enabled: true},
		{MajorRealm: 10, MinorRealm: 2, Name: "中期", RequiredHours: 8900, Enabled: true},
		{MajorRealm: 10, MinorRealm: 3, Name: "后期", RequiredHours: 9700, Enabled: true},
		{MajorRealm: 10, MinorRealm: 4, Name: "圆满", RequiredHours: 10600, Enabled: true},

		{MajorRealm: 11, MinorRealm: 1, Name: "初期", RequiredHours: 11500, Enabled: true},
		{MajorRealm: 11, MinorRealm: 2, Name: "中期", RequiredHours: 12600, Enabled: true},
		{MajorRealm: 11, MinorRealm: 3, Name: "后期", RequiredHours: 13700, Enabled: true},
		{MajorRealm: 11, MinorRealm: 4, Name: "圆满", RequiredHours: 14900, Enabled: true},

		{MajorRealm: 12, MinorRealm: 1, Name: "初期", RequiredHours: 16100, Enabled: true},
		{MajorRealm: 12, MinorRealm: 2, Name: "中期", RequiredHours: 17500, Enabled: true},
		{MajorRealm: 12, MinorRealm: 3, Name: "后期", RequiredHours: 19000, Enabled: true},
		{MajorRealm: 12, MinorRealm: 4, Name: "圆满", RequiredHours: 20600, Enabled: true},

		{MajorRealm: 13, MinorRealm: 1, Name: "初期", RequiredHours: 22300, Enabled: true},
		{MajorRealm: 13, MinorRealm: 2, Name: "中期", RequiredHours: 24400, Enabled: true},
		{MajorRealm: 13, MinorRealm: 3, Name: "后期", RequiredHours: 27800, Enabled: true},
		{MajorRealm: 13, MinorRealm: 4, Name: "圆满", RequiredHours: 31700, Enabled: true},
	}
}

func seedDefaultBreakthroughConfigs(tx *gorm.DB) error {
	defaults := defaultBreakthroughConfigs()

	for _, cfg := range defaults {
		var existing model.BreakthroughConfig
		err := tx.Where("from_major_realm = ?", cfg.FromMajorRealm).First(&existing).Error
		if err == nil {
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := createDefaultBreakthroughConfigIfMissingInTx(tx, &cfg); err != nil {
			return err
		}
	}

	return nil
}

func createDefaultBreakthroughConfigIfMissingInTx(tx *gorm.DB, cfg *model.BreakthroughConfig) error {
	if tx == nil || cfg == nil {
		return fmt.Errorf("BREAKTHROUGH_CONFIG_INVALID")
	}
	entry := *cfg
	if entry.FromMajorRealm < 0 || entry.ToMajorRealm < 0 {
		return fmt.Errorf("BREAKTHROUGH_CONFIG_INVALID")
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	*cfg = entry
	return nil
}

func defaultBreakthroughConfigs() []model.BreakthroughConfig {
	return []model.BreakthroughConfig{
		{
			FromMajorRealm:      0,
			ToMajorRealm:        1,
			PillName:            "聚气化旋",
			PointsCost:          0,
			MinTotalHours:       10,
			SuccessRate:         1.0,
			CooldownHours:       0,
			GuaranteeFailCount:  3,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      1,
			ToMajorRealm:        2,
			PillName:            "凝元丹",
			PointsCost:          100,
			MinTotalHours:       50,
			SuccessRate:         0.80,
			CooldownHours:       24,
			GuaranteeFailCount:  3,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      2,
			ToMajorRealm:        3,
			PillName:            "三纹清灵丹",
			PointsCost:          200,
			MinTotalHours:       150,
			SuccessRate:         0.60,
			CooldownHours:       48,
			GuaranteeFailCount:  3,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      3,
			ToMajorRealm:        4,
			PillName:            "斗灵丹",
			PointsCost:          500,
			MinTotalHours:       350,
			SuccessRate:         0.40,
			CooldownHours:       72,
			GuaranteeFailCount:  3,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      4,
			ToMajorRealm:        5,
			PillName:            "皇极丹",
			PointsCost:          1000,
			MinTotalHours:       750,
			SuccessRate:         0.20,
			CooldownHours:       168,
			GuaranteeFailCount:  5,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		// —— 斗王之后：废除丹药代购，改为“至宝 + 积分”突破 ——
		// 成功率统一 10%，新境界保证连续失败 10 次后下次必过（GuaranteeFailCount=10）。
		// 至宝由秘境信物兑换 / 世界 Boss 掉落，禁止上架交易行；积分渡劫费统一 1500、冷却统一 168h。
		{
			FromMajorRealm:      5,
			ToMajorRealm:        6,
			PillName:            "地心熔岩髓",
			PointsCost:          1500,
			MinTotalHours:       1500,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      6,
			ToMajorRealm:        7,
			PillName:            "破宗源晶",
			PointsCost:          1500,
			MinTotalHours:       2400,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      7,
			ToMajorRealm:        8,
			PillName:            "空间结晶",
			PointsCost:          1500,
			MinTotalHours:       3700,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      8,
			ToMajorRealm:        9,
			PillName:            "九转金髓",
			PointsCost:          1500,
			MinTotalHours:       5500,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      9,
			ToMajorRealm:        10,
			PillName:            "古龙真血",
			PointsCost:          1500,
			MinTotalHours:       8100,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      10,
			ToMajorRealm:        11,
			PillName:            "菩提心",
			PointsCost:          1500,
			MinTotalHours:       11500,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      11,
			ToMajorRealm:        12,
			PillName:            "帝境残魂",
			PointsCost:          1500,
			MinTotalHours:       16100,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
		{
			FromMajorRealm:      12,
			ToMajorRealm:        13,
			PillName:            "古帝源气",
			PointsCost:          1500,
			MinTotalHours:       22300,
			SuccessRate:         0.10,
			CooldownHours:       168,
			GuaranteeFailCount:  10,
			RefundRate:          0.2,
			FailPenaltyPoints:   50,
			SplashMinMajorRealm: 0,
			SplashVictimCount:   0,
			SplashPenaltyPoints: 0,
			Enabled:             true,
		},
	}
}

func FormatCultivationConfigSummary() string {
	rules := GetCultivationRules()
	if rules == nil {
		return "斗气配置缓存未初始化"
	}

	minorCount := 0
	for _, minors := range rules.MinorRealms {
		minorCount += len(minors)
	}

	maxRealmName := "未配置"
	for _, realm := range rules.Realms {
		if realm.IsMaxRealm {
			maxRealmName = realm.Name
			break
		}
	}

	return fmt.Sprintf(
		"【斗气配置】\n\n来源：%s\n加载时间：%s\n大境界数量：%d\n小境界配置：%d\n突破配置：%d\n当前最高境界：%s",
		rules.Source,
		rules.LoadedAt.Format("2006-01-02 15:04:05"),
		len(rules.Realms),
		minorCount,
		len(rules.Breakthroughs),
		maxRealmName,
	)
}

func FormatCultivationRealmConfigs() string {
	rules := GetCultivationRules()
	if rules == nil {
		return "斗气配置缓存未初始化"
	}

	majors := make([]int, 0, len(rules.Realms))
	for major := range rules.Realms {
		majors = append(majors, major)
	}
	sort.Ints(majors)

	var builder strings.Builder
	builder.WriteString("【境界配置】\n")

	for _, major := range majors {
		realm := rules.Realms[major]

		builder.WriteString("\n")
		builder.WriteString(realm.Name)

		if realm.TitleName != "" && realm.TitleName != realm.Name {
			builder.WriteString("（")
			builder.WriteString(realm.TitleName)
			builder.WriteString("）")
		}

		if realm.IsMaxRealm {
			builder.WriteString("【当前最高】")
		}

		builder.WriteString("\n")
		builder.WriteString(fmt.Sprintf("最低修为：%.1f 小时\n", realm.MinTotalHours))

		if realm.MaxTotalHours > 0 {
			builder.WriteString(fmt.Sprintf("阶段上限：%.1f 小时\n", realm.MaxTotalHours))
		}

		minors := rules.MinorRealms[major]
		sort.Slice(minors, func(i, j int) bool {
			return minors[i].MinorRealm < minors[j].MinorRealm
		})

		for _, minor := range minors {
			builder.WriteString(fmt.Sprintf("%s：%.1f 小时\n", minor.Name, minor.RequiredHours))
		}
	}

	return builder.String()
}

func FormatBreakthroughConfigs() string {
	rules := GetCultivationRules()
	if rules == nil {
		return "斗气配置缓存未初始化"
	}

	fromMajors := make([]int, 0, len(rules.Breakthroughs))
	for fromMajor := range rules.Breakthroughs {
		fromMajors = append(fromMajors, fromMajor)
	}
	sort.Ints(fromMajors)

	var builder strings.Builder
	builder.WriteString("【突破配置】\n")

	for _, fromMajor := range fromMajors {
		cfg := rules.Breakthroughs[fromMajor]

		fromName := getRealmConfigName(rules, cfg.FromMajorRealm)
		toName := getRealmConfigName(rules, cfg.ToMajorRealm)

		builder.WriteString("\n")
		builder.WriteString(fmt.Sprintf("%s -> %s\n", fromName, toName))

		if cfg.PillName == "" {
			builder.WriteString("方式：无\n")
		} else if cfg.PointsCost == 0 {
			builder.WriteString(fmt.Sprintf("方式：%s\n", cfg.PillName))
		} else if IsCultivationTreasureItemName(cfg.PillName) {
			builder.WriteString(fmt.Sprintf("至宝：%s\n", cfg.PillName))
		} else {
			builder.WriteString(fmt.Sprintf("丹药：%s\n", cfg.PillName))
		}

		builder.WriteString(fmt.Sprintf("最低修为：%.1f 小时\n", cfg.MinTotalHours))
		builder.WriteString(fmt.Sprintf("积分消耗：%d\n", cfg.PointsCost))
		builder.WriteString(fmt.Sprintf("成功率：%.0f%%\n", cfg.SuccessRate*100))
		builder.WriteString(fmt.Sprintf("冷却：%d 小时\n", cfg.CooldownHours))

		if cfg.GuaranteeFailCount > 0 {
			builder.WriteString(fmt.Sprintf("保底失败次数：%d\n", cfg.GuaranteeFailCount))
		}

		builder.WriteString(fmt.Sprintf("成功返还比例：%.0f%%\n", cfg.RefundRate*100))
		builder.WriteString(fmt.Sprintf("失败调养费：%d\n", cfg.FailPenaltyPoints))

		builder.WriteString("互道：60秒招募，每人+1个百分点，最多5人，成功率最高95%；参与5分，成功到账10分，失败不返还；必成/保底不开放\n")
	}

	return builder.String()
}

func getRealmConfigName(rules *CultivationRuleSet, major int) string {
	if rules == nil {
		return fmt.Sprintf("境界%d", major)
	}

	realm, ok := rules.Realms[major]
	if !ok || realm.Name == "" {
		return fmt.Sprintf("境界%d", major)
	}

	return realm.Name
}

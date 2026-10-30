// Package spiritfire owns the unique-flame asset lifecycle. It does not depend
// on game or market, so both integrations use the same transactional invariants.
package spiritfire

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/security"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ListingType = "spirit_fire"

var Location = time.FixedZone("CST", 8*3600)
var ErrChanged = errors.New("归属、价格或掌控数量已变化，请重新确认")
var ErrLocked = errors.New("异火已上架或不属于你")
var ErrIneligible = errors.New("需要有效且未暂停的斗师及以上正式听书账号")

type Rules struct {
	BossChance   int    `json:"boss_percent"`
	RealmChance  int    `json:"realm_percent"`
	ScrollChance int    `json:"scroll_percent"`
	Prices       [4]int `json:"prices"`
	Percents     [4]int `json:"percents"`
}

func DefaultRules() Rules {
	return Rules{1, 1, 10, [4]int{2000, 3000, 4000, 5000}, [4]int{5, 8, 10, 12}}
}
func Day(t time.Time) string      { return t.In(Location).Format("2006-01-02") }
func Tomorrow(t time.Time) string { return Day(t.In(Location).AddDate(0, 0, 1)) }
func Encode(v any) string         { b, _ := json.Marshal(v); return string(b) }
func ParseRules(raw string) (Rules, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return Rules{}, err
	}
	required := []string{"boss_percent", "realm_percent", "scroll_percent", "prices", "percents"}
	if len(fields) != len(required) {
		return Rules{}, errors.New("请提交完整配置，且不要包含未知字段")
	}
	for _, key := range required {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return Rules{}, fmt.Errorf("缺少配置字段 %s", key)
		}
	}
	for _, key := range []string{"prices", "percents"} {
		var values []int
		if err := json.Unmarshal(fields[key], &values); err != nil {
			return Rules{}, err
		}
		if len(values) != 4 {
			return Rules{}, errors.New("prices和percents必须恰好包含黄玄地天四项")
		}
	}
	var r Rules
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, err
	}
	if r.BossChance < 0 || r.BossChance > 100 || r.RealmChance < 0 || r.RealmChance > 100 || r.ScrollChance < 0 || r.ScrollChance > 100 {
		return r, errors.New("概率必须在0–100之间")
	}
	for i := range r.Prices {
		if r.Prices[i] < 1 || r.Prices[i] > 80000 || r.Percents[i] < 0 || r.Percents[i] > 1000 {
			return r, errors.New("品阶基价须1–80000，加成须0–1000%")
		}
	}
	return r, nil
}
func Models() []any {
	return []any{&model.SpiritFire{}, &model.SpiritFireMastery{}, &model.SpiritFireWallet{}, &model.SpiritFireRules{}, &model.SpiritFireState{}, &model.SpiritFireLoadout{}, &model.SpiritFireDaily{}, &model.SpiritFireQuote{}, &model.SpiritFireEvent{}, &model.SpiritFireNotice{}}
}

var installedPools sync.Map

func Installed(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	// Config.ConnPool is the root pool even on a transaction clone. Avoid a
	// sqlite_master read before our write lock (SQLite read-to-write upgrades
	// otherwise fail immediately under concurrent writers).
	if _, ok := installedPools.Load(db.Config.ConnPool); ok {
		return true
	}
	if db.Migrator().HasTable(&model.SpiritFireState{}) {
		installedPools.Store(db.Config.ConnPool, true)
		return true
	}
	return false
}
func Initialize(db *gorm.DB, now time.Time) error {
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SpiritFireState{ID: 1, LaunchDay: Day(now)}).Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&model.SpiritFireRules{}).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			if err := tx.Create(&model.SpiritFireRules{EffectiveDay: Day(now), JSON: Encode(DefaultRules())}).Error; err != nil {
				return err
			}
		}
		names := []string{"帝炎", "虚无吞炎", "净莲妖火", "金帝焚天炎", "生灵之焱", "八荒破灭焱", "九幽金祖火", "红莲业火", "三千焱炎火", "九幽风炎", "骨灵冷火", "九龙雷罡火", "龟灵地火", "陨落心炎", "海心焰", "火云水炎", "火山石焰", "风怒龙炎", "青莲地心火", "幽冥毒火", "阴阳双炎", "万兽灵火", "玄黄炎"}
		for i, name := range names {
			rank := i + 1
			grade := 0
			switch {
			case rank <= 5:
				grade = 3
			case rank <= 11:
				grade = 2
			case rank <= 17:
				grade = 1
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SpiritFire{ID: uint(rank), Rank: rank, Name: name, Grade: grade}).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS ux_spirit_fire_listing ON spirit_fires(listing_id) WHERE listing_id <> 0").Error; err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		installedPools.Store(db.Config.ConnPool, true)
	}
	return err
}
func Lock(tx *gorm.DB) error {
	r := tx.Model(&model.SpiritFireState{}).Where("id = 1").UpdateColumn("launch_day", gorm.Expr("launch_day"))
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return errors.New("异火系统未初始化")
	}
	return nil
}
func RulesAt(tx *gorm.DB, day string) (Rules, error) {
	var r model.SpiritFireRules
	if err := tx.Where("effective_day <= ?", day).Order("effective_day DESC").First(&r).Error; err != nil {
		return Rules{}, err
	}
	return ParseRules(r.JSON)
}
func GradeName(g int) string {
	if g < 0 || g > 3 {
		return "未知"
	}
	return []string{"黄", "玄", "地", "天"}[g]
}
func Eligible(tx *gorm.DB, id int64, now time.Time) (bool, error) {
	var u model.User
	if err := tx.Where("telegram_id = ?", id).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	usable, err := storage.UserHasUsableLocalAbsAccountTxAt(tx, u, now)
	if err != nil {
		return false, err
	}
	if !usable || !storage.IsFormalAccount(u) {
		return false, nil
	}
	var cul model.Cultivation
	if err := tx.Where("user_id = ?", id).First(&cul).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return cul.MajorRealm >= 2, nil
}
func Audit(tx *gorm.DB, user int64, action string, fire uint, detail string) error {
	return storage.WriteAuditLogInTx(tx, user, "SPIRIT_FIRE_"+action, fmt.Sprint(fire), 0, detail)
}

func RulesQuote(db *gorm.DB, user int64, r Rules, now time.Time) (model.SpiritFireQuote, error) {
	q := model.SpiritFireQuote{}
	if _, err := ParseRules(Encode(r)); err != nil {
		return q, err
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := Lock(tx); err != nil {
			return err
		}
		if err := RequireAdmin(tx, user); err != nil {
			return err
		}
		q = model.SpiritFireQuote{ID: security.GenerateRandomCode(24), UserID: user, Action: "rules", RulesJSON: Encode(r), ExpiresAt: now.Add(15 * time.Minute)}
		return tx.Create(&q).Error
	})
	return q, err
}
func confirmRules(tx *gorm.DB, q model.SpiritFireQuote, now time.Time) error {
	if err := RequireAdmin(tx, q.UserID); err != nil {
		return err
	}
	if _, err := ParseRules(q.RulesJSON); err != nil {
		return err
	}
	rec := model.SpiritFireRules{EffectiveDay: Tomorrow(now), JSON: q.RulesJSON}
	if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "effective_day"}}, DoUpdates: clause.AssignmentColumns([]string{"json"})}).Create(&rec).Error; err != nil {
		return err
	}
	if err := Audit(tx, q.UserID, "RULES", 0, q.RulesJSON); err != nil {
		return err
	}
	return tx.Model(&q).Update("done", true).Error
}

func RequireAdmin(tx *gorm.DB, user int64) error {
	if config.AppConfig != nil && config.AppConfig.AdminIDs[user] {
		return nil
	}
	var u model.User
	if err := tx.Where("telegram_id = ?", user).First(&u).Error; err != nil {
		return err
	}
	if u.Role != "super_admin" {
		return errors.New("仅超级管理员可设置异火配置")
	}
	return nil
}

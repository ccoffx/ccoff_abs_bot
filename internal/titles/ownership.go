package titles

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/realm"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const RealmKey = "realm"

// 不同来源使用不同资格规则；境界是人人随账号获得的动态称号，
// 避雷的四种称号则使用群内唯一持有者与历史记录。
type Definition struct {
	Key, Name, Emoji, Description, Source string
	Unique                                bool
}

var UniqueDefinitions = []Definition{
	{Key: "escape", Name: "临阵脱身", Emoji: "🦊", Description: "换阵后成功避雷", Source: "tribulation", Unique: true},
	{Key: "backfire", Name: "自投雷网", Emoji: "🤡", Description: "换阵后被雷击中", Source: "tribulation", Unique: true},
	{Key: "steadfast", Name: "道心坚定", Emoji: "🗿", Description: "未换阵且避雷成功", Source: "tribulation", Unique: true},
	{Key: "lone", Name: "孤胆渡劫", Emoji: "🌟", Description: "独占一边并获胜", Source: "tribulation", Unique: true},
}

var RankDefinitions = []Definition{
	{Key: rankings.WealthTitle, Name: "富甲天下", Emoji: "💰", Description: "全服财富榜第一", Source: "ranking", Unique: true},
	{Key: rankings.CultivationTitle, Name: "斗气至尊", Emoji: "🔥", Description: "全服斗气榜第一", Source: "ranking", Unique: true},
	{Key: rankings.SectTitle, Name: "万宗魁首", Emoji: "🏯", Description: "全服第一宗门的现任宗主", Source: "ranking", Unique: true},
	{Key: rankings.ListeningTitle, Name: "博览群书", Emoji: "🎧", Description: "本月有效实际听书时长第一", Source: "ranking", Unique: true},
	{Key: rankings.BossTitle, Name: "诛魔魁首", Emoji: "⚔️", Description: "本周世界 Boss 累计有效伤害第一", Source: "ranking", Unique: true},
	{Key: rankings.HarvestTitle, Name: "灵植仙师", Emoji: "🌿", Description: "药园累计成功收获药草株数第一", Source: "ranking", Unique: true},
	{Key: rankings.AlchemyTitle, Name: "丹道宗师", Emoji: "🔥", Description: "累计成功炼丹数量第一", Source: "ranking", Unique: true},
}

var ErrNotHeld = errors.New("你当前未持有该称号，不能佩戴")
var titleMutex sync.Mutex

func ByKey(key string) (Definition, bool) {
	if key == RealmKey {
		return Definition{Key: RealmKey, Name: "境界称号", Emoji: "📿", Description: "随当前境界自动更新", Source: "cultivation"}, true
	}
	for _, d := range UniqueDefinitions {
		if d.Key == key {
			return d, true
		}
	}
	for _, d := range RankDefinitions {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

func Transaction(fn func(*gorm.DB) error) error {
	titleMutex.Lock()
	defer titleMutex.Unlock()
	return storage.DB.Transaction(fn)
}

func RealmTitleInTx(tx *gorm.DB, userID int64) (string, bool, error) {
	var user model.User
	err := tx.Select("telegram_id", "abs_user_id", "status").Where("telegram_id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(user.AbsUserID) == "" || user.Status == "unbound" {
		return "", false, nil
	}
	cul := model.Cultivation{UserID: userID, MajorRealm: 0, MinorRealm: 1}
	err = tx.Where("user_id = ?", userID).First(&cul).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, err
	}
	if realm.StageCount(cul.MajorRealm) == 0 {
		return "", false, fmt.Errorf("REALM_TITLE_INVALID")
	}
	majorName, minorName := "", ""
	var major model.CultivationRealmConfig
	err = tx.Where("major_realm = ? AND enabled = ?", cul.MajorRealm, true).First(&major).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, err
	}
	if err == nil {
		majorName = strings.TrimSpace(major.TitleName)
		if majorName == "" {
			majorName = major.Name
		}
	}
	var minor model.CultivationMinorRealmConfig
	err = tx.Where("major_realm = ? AND minor_realm = ? AND enabled = ?", cul.MajorRealm, cul.MinorRealm, true).First(&minor).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, err
	}
	if err == nil {
		minorName = minor.Name
	}
	return realm.Format(cul.MajorRealm, cul.MinorRealm, majorName, minorName), true, nil
}

func TrackGroupInTx(tx *gorm.DB, chatID int64, name string) error {
	if chatID >= 0 {
		return fmt.Errorf("TITLE_GROUP_INVALID")
	}
	name = textfmt.LotteryDisplayText(name, 40, "")
	group := model.TitleGroup{ChatID: chatID, Name: name}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&group).Error; err != nil {
		return err
	}
	if name != "" {
		return tx.Model(&model.TitleGroup{}).Where("chat_id = ? AND name <> ?", chatID, name).Update("name", name).Error
	}
	return nil
}

func setDesiredInTx(tx *gorm.DB, chatID, userID int64, key, tag string, force bool) error {
	var e model.TitleEquipment
	err := tx.Where("chat_id = ? AND user_id = ?", chatID, userID).First(&e).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && !force && e.TitleKey == key && e.DesiredTag == tag {
		return nil
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "chat_id"}, {Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"title_key": key, "desired_tag": tag, "version": gorm.Expr("version + 1"), "next_sync_at": time.Time{}, "sync_hint": "等待同步群成员标签"})}).Create(&model.TitleEquipment{ChatID: chatID, UserID: userID, TitleKey: key, DesiredTag: tag, Version: 1, SyncHint: "等待同步群成员标签"}).Error
}

func Equip(chatID, userID int64, key string) error {
	if chatID >= 0 || userID <= 0 {
		return ErrNotHeld
	}
	denied := false
	err := Transaction(func(tx *gorm.DB) error {
		if err := TrackGroupInTx(tx, chatID, ""); err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TitleMember{ChatID: chatID, UserID: userID}).Error; err != nil {
			return err
		}
		if key == "" || key == RealmKey {
			tag, available, err := RealmTitleInTx(tx, userID)
			if err != nil {
				return err
			}
			if key == RealmKey && !available {
				return ErrNotHeld
			}
			return setDesiredInTx(tx, chatID, userID, RealmKey, tag, true)
		}
		d, ok := ByKey(key)
		if !ok || !d.Unique {
			return ErrNotHeld
		}
		if d.Source == "ranking" {
			if err := refreshRankTitlesInTx(tx, time.Now()); err != nil {
				return err
			}
		}
		held, err := heldInTx(tx, chatID, userID, d)
		if err != nil {
			return err
		}
		if !held {
			if d.Source == "ranking" {
				// 刷新归属和撤下旧佩戴必须提交；拒绝旧按钮不能回滚榜单事实。
				denied = true
				return nil
			}
			return ErrNotHeld
		}
		return setDesiredInTx(tx, chatID, userID, key, d.Name, true)
	})
	if err != nil {
		return err
	}
	if denied {
		return ErrNotHeld
	}
	return nil
}

func FallbackInTx(tx *gorm.DB, chatID, userID int64, lostKey string) error {
	var e model.TitleEquipment
	err := tx.Where("chat_id = ? AND user_id = ? AND title_key = ?", chatID, userID, lostKey).First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	tag, _, err := RealmTitleInTx(tx, userID)
	if err != nil {
		return err
	}
	return setDesiredInTx(tx, chatID, userID, RealmKey, tag, false)
}

func EnsureMemberInTx(tx *gorm.DB, chatID, userID int64) error {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TitleMember{ChatID: chatID, UserID: userID}).Error; err != nil {
		return err
	}
	tag, available, err := RealmTitleInTx(tx, userID)
	if err != nil {
		return err
	}
	if !available {
		return nil
	}
	var e model.TitleEquipment
	err = tx.Where("chat_id = ? AND user_id = ?", chatID, userID).First(&e).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && e.TitleKey != "" && e.TitleKey != RealmKey {
		return nil
	}
	return setDesiredInTx(tx, chatID, userID, RealmKey, tag, false)
}

func TrackMember(chatID int64, name string, userID int64) error {
	return Transaction(func(tx *gorm.DB) error {
		if err := TrackGroupInTx(tx, chatID, name); err != nil {
			return err
		}
		return EnsureMemberInTx(tx, chatID, userID)
	})
}

// 默认称号不依赖参与小游戏。新绑定账号、境界变化、解绑和称号失效
// 都会反映到持久化的期望标签，网络同步仍独立重试。
func RefreshDefaults() error {
	return Transaction(func(tx *gorm.DB) error {
		if config.AppConfig != nil && config.AppConfig.NoticeGroupID < 0 {
			groupID := config.AppConfig.NoticeGroupID
			if err := TrackGroupInTx(tx, groupID, ""); err != nil {
				return err
			}
			var users []model.User
			if err := tx.Select("telegram_id").Where("abs_user_id <> ? AND status <> ? AND telegram_id > 0", "", "unbound").Find(&users).Error; err != nil {
				return err
			}
			for _, u := range users {
				if err := EnsureMemberInTx(tx, groupID, u.TelegramID); err != nil {
					return err
				}
			}
		}
		// 先在其他群留下记录、后来才绑定账号的成员，也应自动获得默认称号。
		var members []model.TitleMember
		if err := tx.Where("NOT EXISTS (SELECT 1 FROM title_equipments e WHERE e.chat_id = title_members.chat_id AND e.user_id = title_members.user_id)").Find(&members).Error; err != nil {
			return err
		}
		for _, member := range members {
			if err := EnsureMemberInTx(tx, member.ChatID, member.UserID); err != nil {
				return err
			}
		}
		var rows []model.TitleEquipment
		if err := tx.Find(&rows).Error; err != nil {
			return err
		}
		for _, e := range rows {
			key, tag := e.TitleKey, e.DesiredTag
			d, ok := ByKey(key)
			if ok && d.Unique {
				held, err := heldInTx(tx, e.ChatID, e.UserID, d)
				if err != nil {
					return err
				}
				if held {
					continue
				}
			}
			key = RealmKey
			var err error
			tag, _, err = RealmTitleInTx(tx, e.UserID)
			if err != nil {
				return err
			}
			if err := setDesiredInTx(tx, e.ChatID, e.UserID, key, tag, false); err != nil {
				return err
			}
		}
		return nil
	})
}

func errorText(err error) string {
	if errors.Is(err, errGroupNotConfigured) {
		return errGroupNotConfigured.Error()
	}
	if errors.Is(err, ErrNotHeld) {
		return ErrNotHeld.Error()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "暂无该群称号记录"
	}
	log.Printf("⚠️ 称号操作失败: %s", textfmt.FormatPlainError(err))
	return "称号操作暂未完成，请稍后重试"
}

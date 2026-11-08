package titles

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/realm"
	"ccoff-abs-bot/internal/storage"
)

func setupTitleTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "titles.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	oldDB, oldConfig := storage.DB, config.AppConfig
	storage.DB = db
	config.AppConfig = &config.Config{NoticeGroupID: -100}
	t.Cleanup(func() { storage.DB = oldDB; config.AppConfig = oldConfig; _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.User{}, &model.Cultivation{}, &model.CultivationRealmConfig{}, &model.CultivationMinorRealmConfig{}, &model.TitleGroup{}, &model.TitleMember{}, &model.RankTitleHolder{}, &model.RankTitleHistory{}, &model.Sect{}, &model.TitleEquipment{}, &model.TribulationTitleHolder{}, &model.TribulationTitleHistory{}, &model.TribulationTitleStat{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.DailyListeningStat{}, &model.WorldBossEvent{}, &model.WorldBossParticipant{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}); err != nil {
		t.Fatal(err)
	}
	majors, minors := realm.Defaults(), realm.DefaultMinors()
	if err := db.Create(&majors).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&minors).Error; err != nil {
		t.Fatal(err)
	}
	for _, u := range []model.User{{TelegramID: 1, Username: "account", AbsUserID: "abs1"}, {TelegramID: 2, Username: "second", AbsUserID: "abs2"}, {TelegramID: 3, Username: "wallet"}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.Cultivation{UserID: 1, MajorRealm: 2, MinorRealm: 3}).Error; err != nil {
		t.Fatal(err)
	}
}

func titleTestEquipment(t *testing.T, chatID, userID int64) model.TitleEquipment {
	t.Helper()
	var e model.TitleEquipment
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", chatID, userID).Error; err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAccountRealmTitlesExistWithoutPlayingAndFollowRealm(t *testing.T) {
	setupTitleTestDB(t)
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	first := titleTestEquipment(t, -100, 1)
	if first.TitleKey != RealmKey || first.DesiredTag != "三星斗师" {
		t.Fatal("default is not current realm", first)
	}
	second := titleTestEquipment(t, -100, 2)
	if second.TitleKey != RealmKey || second.DesiredTag != "斗之气一段" {
		t.Fatal("new account has no initial title", second)
	}
	var n int64
	if err := storage.DB.Model(&model.TitleEquipment{}).Where("user_id = ?", 3).Count(&n).Error; err != nil || n != 0 {
		t.Fatal("wallet incorrectly awarded realm title", n, err)
	}
	if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]any{"major_realm": 9, "minor_realm": 10}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	changed := titleTestEquipment(t, -100, 1)
	if changed.DesiredTag != "斗尊巅峰十转" || changed.Version != first.Version+1 {
		t.Fatal("realm change not queued", changed)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if unchanged := titleTestEquipment(t, -100, 1); unchanged.Version != changed.Version {
		t.Fatal("no-op refresh queued duplicate tag update")
	}
	out, markup, err := Render(-100, 1, false)
	if err != nil || !strings.Contains(out, "斗尊巅峰十转") || !strings.Contains(out, "境界称号") {
		t.Fatal("realm title missing from profile", out, err)
	}
	found := false
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != nil && *b.CallbackData == "titles:equip:-100:realm" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("realm title cannot be worn")
	}
	root, _, err := renderGroups(1, 0)
	if err != nil || !strings.Contains(root, "境界称号") || !strings.Contains(root, "斗尊巅峰十转") {
		t.Fatal("default profile still depends on game participation", root, err)
	}
}

func TestOtherTitleSelectionPreservedThenFallsBackToLatestRealm(t *testing.T) {
	setupTitleTestDB(t)
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.TribulationTitleHolder{ChatID: -100, TitleKey: "escape", UserID: 1, Count: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", 1).Updates(map[string]any{"major_realm": 10, "minor_realm": 4}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != "escape" || e.DesiredTag != "临阵脱身" {
		t.Fatal("realm refresh overwrote explicit title", e)
	}
	if err := Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != RealmKey || e.DesiredTag != "巅峰半圣" {
		t.Fatal("reset did not select current realm", e)
	}
	if err := Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	if err := Transaction(func(tx *gorm.DB) error { return FallbackInTx(tx, -100, 1, "escape") }); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != RealmKey || e.DesiredTag != "巅峰半圣" {
		t.Fatal("lost title did not fall back", e)
	}
	if err := Equip(-100, 3, RealmKey); !errors.Is(err, ErrNotHeld) {
		t.Fatal("wallet wore unearned realm title", err)
	}
}

func TestUnbindingRemovesRealmTagAndRebindingRestoresDefault(t *testing.T) {
	setupTitleTestDB(t)
	if err := TrackMember(-200, "另一个群", 1); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -200, 1); e.DesiredTag != "" {
		t.Fatal("unbound account kept realm tag", e)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -200, 1); e.DesiredTag != "三星斗师" {
		t.Fatal("rebinding did not restore realm", e)
	}
	// 数据库读取失败必须返回错误，不可按“没有账号”清空标签。
	if err := storage.DB.Migrator().DropTable(&model.CultivationMinorRealmConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err == nil {
		t.Fatal("configuration read error treated as no title")
	}
	if e := titleTestEquipment(t, -200, 1); e.DesiredTag != "三星斗师" {
		t.Fatal("failed refresh erased tag", e)
	}
}

func TestBindingAfterGroupActivityAddsDefaultWithoutAnotherMessage(t *testing.T) {
	setupTitleTestDB(t)
	if err := TrackMember(-200, "另一个群", 3); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 3).Update("abs_user_id", "newly-bound").Error; err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if e := titleTestEquipment(t, -200, 3); e.TitleKey != RealmKey || e.DesiredTag != "斗之气一段" {
		t.Fatal("binding after group activity did not queue default", e)
	}
}

func TestGameTitleLeaderboardKeepsIncumbentFirstOnTiedCounts(t *testing.T) {
	setupTitleTestDB(t)
	if err := TrackMember(-100, "测试群", 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 两名原本并列的玩家在同局各增加一次，现任较晚入阵。
	// 归属应继续保留现任，不能因本次入阵顺序让榜单第一变人。
	createActivityTitleRows(t,
		&model.TribulationTitleStat{ChatID: -100, TitleKey: "escape", UserID: 1, UserName: "挑战者", Count: 5, LastAchievedAt: now, LastEntryID: 10},
		&model.TribulationTitleStat{ChatID: -100, TitleKey: "escape", UserID: 2, UserName: "现任道友", Count: 5, LastAchievedAt: now, LastEntryID: 11},
		&model.TribulationTitleHolder{ChatID: -100, TitleKey: "escape", UserID: 2, UserName: "现任道友", Count: 5, AcquiredAt: now.Add(-2 * time.Hour)})
	out, _, err := Render(-100, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "现任：现任道友 · 5 次\n1. 现任道友 · 5 次\n2. 挑战者 · 5 次") {
		t.Fatal("game title leaderboard disagrees with the tied incumbent", out)
	}
}

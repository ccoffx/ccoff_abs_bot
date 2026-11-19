package titles

import (
	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"testing"
	"time"
)

func TestOtherGroupTrackingIgnoredAndHistoricalDataCleaned(t *testing.T) {
	setupTitleTestDB(t)
	if err := TrackMember(-200, "其他群", 1); err != nil {
		t.Fatal(err)
	}
	var n int64
	storage.DB.Model(&model.TitleGroup{}).Count(&n)
	if n != 0 {
		t.Fatal("foreign activity registered a group")
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&model.TitleGroup{ChatID: -200}, &model.TitleMember{ChatID: -200, UserID: 1}, &model.TitleEquipment{ChatID: -200, UserID: 1, Version: 1}, &model.TitleMemberSyncRequest{ChatID: -200, UserID: 1}} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.Create(&model.RankTitleHolder{TitleKey: "global", UserID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := CleanupOtherGroups(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []any{&model.TitleGroup{}, &model.TitleMember{}, &model.TitleEquipment{}, &model.TitleMemberSyncRequest{}} {
		if err := storage.DB.Model(table).Where("chat_id <> ?", -100).Count(&n).Error; err != nil || n != 0 {
			t.Fatal("foreign data survived", n, err)
		}
	}
	titleTestEquipment(t, -100, 1)
	storage.DB.Model(&model.RankTitleHolder{}).Count(&n)
	if n != 1 {
		t.Fatal("global ownership deleted")
	}
	storage.DB.Model(&model.User{}).Count(&n)
	if n != 3 {
		t.Fatal("accounts deleted")
	}
	config.AppConfig.NoticeGroupID = 0
	if err := CleanupOtherGroups(); err == nil {
		t.Fatal("unconfigured group allowed cleanup")
	}
	titleTestEquipment(t, -100, 1)
}

func TestSyncTagsSkipsHistoricalForeignGroup(t *testing.T) {
	setupTitleTestDB(t)
	if err := storage.DB.Create(&model.TitleEquipment{ChatID: -200, UserID: 1, Version: 1, DesiredTag: "旧称号"}).Error; err != nil {
		t.Fatal(err)
	}
	// A foreign pending row must not reach Telegram at all; nil bot would panic.
	SyncTags(nil, time.Now())
	if e := titleTestEquipment(t, -200, 1); e.SyncedVersion != 0 {
		t.Fatal("foreign task acknowledged as synced")
	}
	config.AppConfig.NoticeGroupID = 0
	SyncTags(nil, time.Now())
}

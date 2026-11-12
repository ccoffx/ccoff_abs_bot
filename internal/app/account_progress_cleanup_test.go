package app

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/account"
	"ccoff-abs-bot/internal/game"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/titles"
)

func seedDeletedAccountProgressTest(t *testing.T) {
	t.Helper()
	now := time.Now()
	if err := storage.DB.AutoMigrate(&model.Sect{}, &model.WorldBossEvent{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{2, 3} {
		for _, row := range []any{
			&model.Cultivation{UserID: id, MajorRealm: 5, MinorRealm: 8, TotalAudioTime: 1000, PillAudioTime: 100},
			&model.DailyListeningStat{UserID: id, AbsUserID: fmt.Sprintf("abs-%d", id), DayKey: "2026-11-01", CappedSeconds: 3600, EffectiveHours: 1},
			&model.ABSCrossDaySessionScanCursor{UserID: id, AbsUserID: fmt.Sprintf("abs-%d", id)},
			&model.ABSCrossDaySessionAdjustment{UserID: id, AbsUserID: fmt.Sprintf("abs-%d", id), SessionKey: "old"},
			&model.GardenPlanting{UserID: id, Status: "harvested", HarvestedAt: &now, Quantity: 100},
			&model.GardenPlanting{UserID: id, Status: "growing", Quantity: 0},
			&model.GardenAlchemyRecord{UserID: id, Quantity: 100},
			&model.WorldBossParticipant{BossID: "old-boss", UserID: id, AbsUserID: fmt.Sprintf("abs-%d", id), Damage: 100, IsRewarded: true},
			&model.GardenPlot{UserID: id, PlotNo: 1, UnlockedAt: now},
			&model.GardenRecipeUnlock{UserID: id, RecipeKey: "old-recipe", UnlockedAt: now},
			&model.Inventory{UserID: id, ItemName: "old-pill", Quantity: 100},
			&model.GardenSeedPurchase{UserID: id, SeedKey: "old-seed", DayKey: "2026-11-01", Quantity: 1},
			&model.GardenHerbMarketSale{UserID: id, SeedKey: "old-seed", DayKey: "2026-11-01", Quantity: 1},
			&model.SectCaveRetreat{UserID: id, Status: "active"},
			&model.SectOwnerBonusRevision{UserID: id, DayKey: "2026-11-01", Enabled: true},
		} {
			if err := storage.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := storage.DB.Create(&model.WorldBossEvent{BossID: "old-boss", Status: "settled", EndAt: now.Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{rankings.CultivationTitle, rankings.ListeningTitle, rankings.HarvestTitle, rankings.AlchemyTitle, rankings.BossTitle} {
		for _, row := range []any{
			&model.RankTitleHolder{TitleKey: key, UserID: 2},
			&model.RankTitleHistory{TitleKey: key, UserID: 2, AcquiredAt: now, NoticeState: "pending"},
		} {
			if err := storage.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := storage.DB.Create(&model.TitleEquipment{ChatID: -100, UserID: 2, TitleKey: rankings.BossTitle, DesiredTag: "诛魔魁首", Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestAccountDeletionPreventsProgressInheritance(t *testing.T) {
	for _, action := range []string{"SELF_DELETE_USER", "FORCE_DELETE_USER", "AUTO_DELETE_EXPIRED_USER"} {
		t.Run(action, func(t *testing.T) {
			bot := lifecycleFixture(t, time.Now().AddDate(0, 0, -8), true, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
			seedDeletedAccountProgressTest(t)
			if action == "AUTO_DELETE_EXPIRED_USER" {
				if err := runDailyLifecycleOperations(bot); err != nil {
					t.Fatal(err)
				}
			} else {
				actor := int64(2)
				if action == "FORCE_DELETE_USER" {
					actor = 1
				}
				if err := deleteAccountWithAudit(actor, 2, "lifecycle_abs", action, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, source := range []any{&model.Cultivation{}, &model.DailyListeningStat{}, &model.ABSCrossDaySessionScanCursor{}, &model.ABSCrossDaySessionAdjustment{}, &model.GardenPlanting{}, &model.GardenAlchemyRecord{}, &model.WorldBossParticipant{}, &model.GardenPlot{}, &model.GardenRecipeUnlock{}, &model.Inventory{}, &model.GardenSeedPurchase{}, &model.GardenHerbMarketSale{}, &model.SectCaveRetreat{}, &model.SectOwnerBonusRevision{}} {
				var count int64
				if err := storage.DB.Model(source).Where("user_id = ?", 2).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("deleted account source %T remains: %d %v", source, count, err)
				}
				if err := storage.DB.Model(source).Where("user_id = ?", 3).Count(&count).Error; err != nil || count == 0 {
					t.Fatalf("other account source %T was changed: %d %v", source, count, err)
				}
			}
			var history []model.RankTitleHistory
			if err := storage.DB.Where("user_id = ?", 2).Find(&history).Error; err != nil || len(history) != 5 {
				t.Fatal("title history lost", history, err)
			}
			for _, h := range history {
				if h.ReleasedAt == nil || h.NoticeState != "obsolete" {
					t.Fatal("title was not released", h)
				}
			}
			var equipment model.TitleEquipment
			if err := storage.DB.First(&equipment, "chat_id = ? AND user_id = ?", -100, 2).Error; err != nil || equipment.TitleKey != titles.RealmKey || equipment.DesiredTag != "" {
				t.Fatal("deleted account tag was not cleared", equipment, err)
			}
			if err := account.BindLocalUserWithAudit(2, "new-account", "brand-new-abs", "new-security-hash", nil); err != nil {
				t.Fatal(err)
			}
			if c := game.GetOrCreateCultivation(2); c == nil || c.MajorRealm != 0 || c.TotalAudioTime != 0 || c.PillAudioTime != 0 {
				t.Fatal("new account inherited old cultivation", c)
			}
			if err := titles.RefreshRankTitlesAfterListeningBatch(time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Transaction(func(tx *gorm.DB) error { return titles.RefreshBossTitleInTx(tx, time.Now()) }); err != nil {
				t.Fatal(err)
			}
			var holders int64
			if err := storage.DB.Model(&model.RankTitleHolder{}).Where("user_id = ?", 2).Count(&holders).Error; err != nil || holders != 0 {
				t.Fatal("new account inherited old titles", holders, err)
			}
		})
	}
}

func TestAccountProgressCleanupFailureRollsBackDeletion(t *testing.T) {
	lifecycleFixture(t, time.Now(), false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	seedDeletedAccountProgressTest(t)
	if err := storage.DB.Exec("CREATE TRIGGER reject_progress_cleanup BEFORE DELETE ON garden_alchemy_records BEGIN SELECT RAISE(FAIL, 'cleanup unavailable'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountWithAudit(2, 2, "lifecycle_abs", "SELF_DELETE_USER", nil); err == nil {
		t.Fatal("cleanup failure was ignored")
	}
	var user model.User
	if err := storage.DB.First(&user, "telegram_id = ?", 2).Error; err != nil {
		t.Fatal("local user deleted despite rollback", err)
	}
	var cultivation model.Cultivation
	if err := storage.DB.First(&cultivation, "user_id = ?", 2).Error; err != nil {
		t.Fatal("progress partially deleted", err)
	}
	var holder model.RankTitleHolder
	if err := storage.DB.First(&holder, "title_key = ?", rankings.BossTitle).Error; err != nil || holder.UserID != 2 {
		t.Fatal("title partially revoked", holder, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER reject_progress_cleanup").Error; err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountWithAudit(2, 2, "lifecycle_abs", "SELF_DELETE_USER", nil); err != nil {
		t.Fatal("failed deletion cannot be retried", err)
	}
}

func TestAccountDeletionRemovesSectIdentity(t *testing.T) {
	for _, scenario := range []string{"member", "owner", "empty", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			lifecycleFixture(t, time.Now(), false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
			seedDeletedAccountProgressTest(t)
			ownerID := int64(2)
			if scenario == "member" {
				ownerID = 3
			}
			sect := model.Sect{Name: "old-sect", OwnerID: ownerID, Level: 9, MemberCount: 99}
			if err := storage.DB.Create(&sect).Error; err != nil {
				t.Fatal(err)
			}
			members := []model.SectMember{{SectID: int64(sect.ID), UserID: 2, Role: "member"}}
			if scenario != "empty" {
				members = append(members,
					model.SectMember{SectID: int64(sect.ID), UserID: 3, UserName: "successor", Role: "elder", Contribution: 1},
					model.SectMember{SectID: int64(sect.ID), UserID: 4, Role: "member", Contribution: 999})
			}
			if err := storage.DB.Create(&members).Error; err != nil {
				t.Fatal(err)
			}
			if scenario == "rollback" {
				if err := storage.DB.Exec("CREATE TRIGGER reject_sect_cleanup BEFORE UPDATE ON sects BEGIN SELECT RAISE(FAIL, 'sect unavailable'); END").Error; err != nil {
					t.Fatal(err)
				}
				if err := deleteAccountWithAudit(2, 2, "lifecycle_abs", "SELF_DELETE_USER", nil); err == nil {
					t.Fatal("expected cleanup failure")
				}
				var user model.User
				if err := storage.DB.First(&user, "telegram_id = ?", 2).Error; err != nil {
					t.Fatal("user deletion not rolled back", err)
				}
				var member model.SectMember
				if err := storage.DB.First(&member, "user_id = ?", 2).Error; err != nil {
					t.Fatal("membership deletion not rolled back", err)
				}
				var successor model.SectMember
				if err := storage.DB.First(&successor, "user_id = ?", 3).Error; err != nil || successor.Role != "elder" {
					t.Fatal("successor promotion not rolled back", successor, err)
				}
				return
			}
			if err := deleteAccountWithAudit(2, 2, "lifecycle_abs", "SELF_DELETE_USER", nil); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := storage.DB.Model(&model.SectMember{}).Where("user_id = ?", 2).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("membership survived deletion", count, err)
			}
			var saved model.Sect
			err := storage.DB.First(&saved, sect.ID).Error
			if scenario == "empty" {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					t.Fatal("empty sect not dissolved", err)
				}
			} else {
				if err != nil || saved.OwnerID != 3 || saved.MemberCount != 2 {
					t.Fatal("incorrect sect after deletion", saved, err)
				}
				if scenario == "owner" {
					var successor model.SectMember
					if err := storage.DB.First(&successor, "user_id = ?", 3).Error; err != nil || successor.Role != "owner" {
						t.Fatal("elder not promoted", successor, err)
					}
					var bonus model.SectOwnerBonusRevision
					day := time.Now().In(time.FixedZone("CST", 8*3600)).AddDate(0, 0, 1).Format("2006-01-02")
					if err := storage.DB.First(&bonus, "user_id = ? AND day_key = ?", 3, day).Error; err != nil || !bonus.Enabled {
						t.Fatal("successor bonus not scheduled", bonus, err)
					}
				}
			}
			if err := account.BindLocalUserWithAudit(2, "new-account", "brand-new-abs", "hash", nil); err != nil {
				t.Fatal(err)
			}
			if err := titles.RefreshRankTitles(time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := storage.DB.Model(&model.RankTitleHolder{}).Where("title_key = ? AND user_id = ?", rankings.SectTitle, 2).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("new account inherited sect title", count, err)
			}
		})
	}
}

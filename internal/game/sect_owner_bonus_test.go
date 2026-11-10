package game

import (
	"math"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/sectbonus"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestOwnerBonusAddsCultivationWithoutContributionOrCompounding(t *testing.T) {
	db, now, stat := setupListeningContributionTest(t)
	// Schedule before the historical day to exercise late refreshes as well.
	day, err := time.ParseInLocation("2006-01-02", stat.DayKey, time.FixedZone("CST", 8*3600))
	contributionMust(t, err)
	contributionMust(t, sectbonus.Schedule(db, 1, true, day.Add(-time.Hour)))
	addContributionRetreat(t, db, stat.DayKey)
	assertListeningSync(t, now, 7.2+stat.EffectiveHours*.03+1.6*5.90)
	assertContribution(t, db, 7)
	assertListeningSync(t, now, 7.2+stat.EffectiveHours*.03+1.6*5.90)
	assertContribution(t, db, 7)
	var saved model.DailyListeningStat
	contributionMust(t, db.First(&saved, stat.ID).Error)
	if math.Abs(saved.SectOwnerBonusHours-.192) > 1e-9 || saved.EffectiveHours != 6.4 || math.Abs(saved.BaseCultivationHours-7.2) > 1e-9 {
		t.Fatalf("owner bonus changed other components: %+v", saved)
	}
	out := formatDailyListeningStat("今日净修为", saved, 10.24)
	if !strings.Contains(out, "宗主专享 +3%") || !strings.Contains(out, "+0.192") {
		t.Fatalf("owner bonus not visible: %s", out)
	}
}

func TestCreateAndTransferSectScheduleOwnerBonus(t *testing.T) {
	bot := setupSectRegressionTest(t)
	db := storage.DB
	contributionMust(t, db.AutoMigrate(&model.PointTransaction{}))
	contributionMust(t, db.Create(&model.User{TelegramID: 2, Points: 200}).Error)
	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, From: &tgbotapi.User{ID: 2, FirstName: "宗主"}}
	handleConfirmCreateSect(bot, msg, "新宗门")
	var sect model.Sect
	contributionMust(t, db.Where("owner_id = ?", 2).First(&sect).Error)
	var revision model.SectOwnerBonusRevision
	contributionMust(t, db.Where("user_id = ?", 2).First(&revision).Error)
	tomorrow := SectDayKey(time.Now().In(time.FixedZone("CST", 8*3600)).AddDate(0, 0, 1))
	if revision.DayKey != tomorrow || !revision.Enabled {
		t.Fatalf("creation did not schedule perk: %+v", revision)
	}
	contributionMust(t, db.Create(&model.SectMember{SectID: int64(sect.ID), UserID: 3, Role: sectRoleMember}).Error)
	handleTransferSectOwner(bot, msg, "3")
	contributionMust(t, db.Where("user_id = ?", 2).First(&revision).Error)
	if revision.Enabled {
		t.Fatal("old owner kept next-day bonus")
	}
	var next model.SectOwnerBonusRevision
	contributionMust(t, db.Where("user_id = ?", 3).First(&next).Error)
	if !next.Enabled || next.DayKey != tomorrow {
		t.Fatalf("new owner did not get next-day bonus: %+v", next)
	}
}

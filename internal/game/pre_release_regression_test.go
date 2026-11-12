package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/lifecycle"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func setupSectRegressionTest(t *testing.T) *tgbotapi.BotAPI {
	t.Helper()
	setupListeningAbuseScheduleDB(t)
	old := config.AppConfig
	config.AppConfig = &config.Config{SecurityPepper: strings.Repeat("a", 64)}
	t.Cleanup(func() { config.AppConfig = old })
	if err := storage.DB.AutoMigrate(&model.Sect{}, &model.SectMember{}, &model.SectShopPurchase{}, &model.SectContributionLog{}, &model.SectShopRenewClaim{}, &model.AutoDeleteMsg{}, &model.SectLottery{}, &model.SectLotteryEntry{}, &model.SectLotteryPrize{}, &model.SectLotteryWinner{}, &model.SectLotteryReminder{}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Hour)
	for _, row := range []any{
		&model.User{TelegramID: 1, Username: "user1", AbsUserID: "abs1", ExpireAt: &end},
		&model.Sect{Name: "audit", OwnerID: 1, Level: 1},
		&model.SectMember{SectID: 1, UserID: 1, Contribution: 500, JoinedAt: time.Now().AddDate(0, 0, -8)},
	} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Test"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(srv.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestSectRenewMustNotEnableAdministratorBannedABS(t *testing.T) {
	bot := setupSectRegressionTest(t)
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Updates(map[string]any{"is_suspended": true, "admin_suspended": true}).Error; err != nil {
		t.Fatal(err)
	}
	var activations atomic.Int64
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			activations.Add(1)
		}
		fmt.Fprint(w, `{"id":"abs1","isActive":false}`)
	})
	handleExchangeSectRenew(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, true)
	if activations.Load() != 0 {
		t.Fatalf("administrator-banned ABS was enabled %d times before local rejection", activations.Load())
	}
}

func TestSectRenewPreservesFreezeAndRetriesABSFailure(t *testing.T) {
	for _, freeze := range []bool{false, true} {
		t.Run(fmt.Sprintf("freeze=%t", freeze), func(t *testing.T) {
			bot := setupSectRegressionTest(t)
			if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("is_suspended", true).Error; err != nil {
				t.Fatal(err)
			}
			if freeze {
				end := time.Now().Add(time.Hour)
				if err := storage.DB.Create(&model.ListeningAbuseRecord{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-01", Action: storage.ListeningAbuseActionFreeze, Status: storage.ListeningAbuseStatusActive, FreezeEndAt: &end}).Error; err != nil {
					t.Fatal(err)
				}
			}
			var fail, activated atomic.Bool
			fail.Store(true)
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					w.WriteHeader(503)
					return
				}
				if r.Method == http.MethodPatch {
					activated.Store(true)
				}
				fmt.Fprint(w, `{"id":"abs1","isActive":false}`)
			})
			msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}
			handleExchangeSectRenew(bot, msg, true)
			fail.Store(false)
			err := lifecycle.ReactivateExpiredAccount(1)
			if (err != nil) != freeze || activated.Load() == freeze {
				t.Fatalf("restore ignored freeze or lost retry: activated=%t error=%v", activated.Load(), err)
			}
			// A retry of the command must not charge again or extend again.
			var before, after model.User
			if err := storage.DB.First(&before, "telegram_id = ?", 1).Error; err != nil {
				t.Fatal(err)
			}
			handleExchangeSectRenew(bot, msg, true)
			if err := storage.DB.First(&after, "telegram_id = ?", 1).Error; err != nil || !before.ExpireAt.Equal(*after.ExpireAt) || after.IsSuspended != freeze {
				t.Fatalf("renewal retry changed extension or suspension: %+v error=%v", after, err)
			}
			var member model.SectMember
			if err := storage.DB.First(&member, "user_id = ?", 1).Error; err != nil || member.Contribution != 500-sectShopRenewContributionCost {
				t.Fatalf("contribution charged more than once: %+v error=%v", member, err)
			}
		})
	}
}

func TestAbuseReleaseSerializesAdministratorBan(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	u := model.User{TelegramID: 1, Username: "user1", AbsUserID: "abs1"}
	if err := storage.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-time.Minute)
	r := model.ListeningAbuseRecord{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-01", Action: storage.ListeningAbuseActionFreeze, Status: storage.ListeningAbuseStatusActive, FreezeEndAt: &end}
	if err := storage.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	var activated, banRejected atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			err := storage.DB.Model(&u).Updates(map[string]any{"is_suspended": true, "admin_suspended": true}).Error
			banRejected.Store(err != nil && strings.Contains(err.Error(), "生命周期"))
		} else if req.Method == http.MethodPatch {
			activated.Store(true)
		}
		fmt.Fprint(w, `{"id":"abs1","isActive":false}`)
	})
	if err := releaseListeningAbuseFreeze(nil, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !activated.Load() || !banRejected.Load() {
		t.Fatalf("release and administrator ban were not serialized: activated=%t ban_rejected=%t", activated.Load(), banRejected.Load())
	}
	if err := storage.DB.Model(&u).Updates(map[string]any{"is_suspended": true, "admin_suspended": true}).Error; err != nil {
		t.Fatalf("completed restoration kept blocking administrator ban: %v", err)
	}
	if err := releaseListeningAbuseFreeze(nil, r, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestFullSectLotteryMustRecoverAfterRestart(t *testing.T) {
	bot := setupSectRegressionTest(t)
	lot := model.SectLottery{SectID: 1, CreatorID: 1, Title: "full", Mode: sectLotteryModeCount, TargetEntryCount: 1, EntryCount: 1, PrizeCount: 1, Status: sectLotteryStatusActive}
	if err := storage.DB.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	code, err := encryptSectLotterySecret("audit-prize")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&model.SectLotteryEntry{LotteryID: lot.ID, SectID: 1, UserID: 1},
		&model.SectLotteryPrize{LotteryID: lot.ID, CodeEnc: code, Status: sectLotteryPrizePending},
	} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DB.Exec(`CREATE TRIGGER reject_lottery_winner BEFORE INSERT ON sect_lottery_winners BEGIN SELECT RAISE(ABORT, 'forced draw failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	runSectLotterySchedulerTick(bot)
	if err := storage.DB.First(&lot, lot.ID).Error; err != nil || lot.Status != sectLotteryStatusActive {
		t.Fatalf("failed draw did not roll back: status=%s error=%v", lot.Status, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER reject_lottery_winner").Error; err != nil {
		t.Fatal(err)
	}
	runSectLotterySchedulerTick(bot)
	runSectLotterySchedulerTick(bot)
	if err := storage.DB.First(&lot, lot.ID).Error; err != nil || lot.Status != sectLotteryStatusDrawn {
		t.Fatalf("persisted full lottery never drawn by scheduler: status=%s err=%v", lot.Status, err)
	}
	var winners int64
	if err := storage.DB.Model(&model.SectLotteryWinner{}).Where("lottery_id = ?", lot.ID).Count(&winners).Error; err != nil || winners != 1 {
		t.Fatalf("retry duplicated or lost winner: count=%d error=%v", winners, err)
	}
}

func TestBossChangedBindingMustNotBlockOtherParticipants(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	old := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = old })
	if err := storage.DB.AutoMigrate(&model.SectMember{}, &model.Sect{}, &model.Cultivation{}, &model.Inventory{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: p.UserID, Username: "new-account", AbsUserID: "replacement-abs"}).Error; err != nil {
		t.Fatal(err)
	}
	valid := model.WorldBossParticipant{Model: gorm.Model{CreatedAt: event.StartAt}, BossID: event.BossID, UserID: 2, AbsUserID: "abs2"}
	for _, row := range []any{&model.User{TelegramID: 2, Username: "valid-account", AbsUserID: "abs2"}, &valid} {
		if err := storage.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		session := bossSession("valid-playback", event.StartAt, event.EndAt, 3600)
		session["userId"] = "abs2"
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{session}})
	})
	for range 2 {
		settleWorldBoss(nil, event.BossID)
	}
	if err := storage.DB.First(&event, event.ID).Error; err != nil || event.Status != "settled" {
		t.Fatalf("changed ABS binding indefinitely blocks boss settlement: status=%s err=%v", event.Status, err)
	}
	if err := storage.DB.First(&valid, valid.ID).Error; err != nil || !valid.IsRewarded || valid.Damage < worldBossValidDamage {
		t.Fatalf("valid participant lost reward: %+v error=%v", valid, err)
	}
	if err := storage.DB.First(&p, p.ID).Error; err != nil || p.IsRewarded || p.Damage != 0 {
		t.Fatalf("changed participant retained damage or reward: %+v error=%v", p, err)
	}
}

func TestBossBindingChangedBeforePayoutCannotContributeToKill(t *testing.T) {
	event, p := bossEvidenceFixture(t)
	event.MaxHP, event.Status = 100, "settling"
	p.Damage = 200
	if err := storage.DB.Model(&event).Updates(map[string]any{"max_hp": event.MaxHP, "status": event.Status}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Create(&model.User{TelegramID: p.UserID, Username: "replacement", AbsUserID: "replacement-abs"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Migrator().DropTable(&model.RankTitleHolder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := grantWorldBossRewards(&event, []model.WorldBossParticipant{p}); err == nil {
		t.Fatal("Boss settlement committed despite title update failure")
	}
	var persisted model.WorldBossEvent
	if err := storage.DB.First(&persisted, event.ID).Error; err != nil || persisted.Status != "settling" {
		t.Fatal("title failure did not roll back settlement", persisted, err)
	}
	if err := storage.DB.AutoMigrate(&model.RankTitleHolder{}); err != nil {
		t.Fatal(err)
	}
	damage, err := grantWorldBossRewards(&event, []model.WorldBossParticipant{p})
	if err != nil || damage != 0 || event.IsKilled || event.CurrentHP != 100 {
		t.Fatalf("stale identity caused a kill: damage=%v event=%+v error=%v", damage, event, err)
	}
}

func TestPermanentWhitelistMustBeEligibleForSectLottery(t *testing.T) {
	setupSectRegressionTest(t)
	past := time.Now().Add(-time.Hour)
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Updates(map[string]any{"is_whitelist": true, "expire_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	var member model.SectMember
	if err := storage.DB.First(&member, "user_id = ?", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateSectLotteryUserEligibleTx(storage.DB, member); err != nil {
		t.Fatalf("valid permanent whitelist rejected due to nominal expiry: %v", err)
	}
}

func TestSectLotteryDetailsAndSummaryRespectTelegramMessageLimit(t *testing.T) {
	for _, name := range []string{"长", "😀"} {
		t.Run(name, func(t *testing.T) {
			setupSectRegressionTest(t)
			if err := storage.DB.Model(&model.SectMember{}).Where("user_id = ?", 1).Update("role", SectRoleOwner).Error; err != nil {
				t.Fatal(err)
			}
			lot := model.SectLottery{SectID: 1, CreatorID: 1, Title: "large", Mode: sectLotteryModeCount, Status: sectLotteryStatusDrawn}
			if err := storage.DB.Create(&lot).Error; err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 100; i++ {
				if err := storage.DB.Create(&model.SectLotteryWinner{LotteryID: lot.ID, SectID: 1, PrizeID: uint(i + 1), UserID: int64(1000 + i), UserName: strings.Repeat(name, 40), Status: sectLotteryWinnerAssigned}).Error; err != nil {
					t.Fatal(err)
				}
			}
			var longest atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Test"}}`)
					return
				}
				_ = r.ParseForm()
				if n := int64(len(utf16.Encode([]rune(r.Form.Get("text"))))); n > longest.Load() {
					longest.Store(n)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 1, "type": "private"}}})
			}))
			defer srv.Close()
			bot, err := tgbotapi.NewBotAPIWithClient("test", srv.URL+"/bot%s/%s", srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			handleSectLotteryDetailCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, fmt.Sprint(lot.ID))
			notifySectLotteryCreatorSummary(bot, 1, lot, 100, 0, 0)
			if n := longest.Load(); n > 4096 {
				t.Fatalf("100-winner detail sent as one oversized message: %d characters", n)
			}
		})
	}
}

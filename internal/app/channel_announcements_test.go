package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/telegram"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

func channelOpsFixture(t *testing.T) *tgbotapi.BotAPI {
	t.Helper()
	setupTreasureTestDB(t)
	if err := storage.DB.AutoMigrate(&model.AnnouncementDelivery{}, &model.AuditLog{}, &model.OpenRegistrationCampaign{}, &model.OpenRegistrationReservation{}, &model.CompensationCampaign{}, &model.CompensationGrant{}); err != nil {
		t.Fatal(err)
	}
	config.AppConfig.AdminIDs = map[int64]bool{1: true}
	config.AppConfig.AnnouncementChannelID = -100200
	config.AppConfig.NoticeGroupID = -100300
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot
}
func TestRegistrationAnnouncementsFreezeTargetAndEndOnSuccessfulQuota(t *testing.T) {
	bot := channelOpsFixture(t)
	c, err := createOpenRegistrationCampaign(1, openRegistrationModeQuota, 1, "internal-audit-secret", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config.AppConfig.AnnouncementChannelID = -100999
	reconcileChannelAnnouncements(bot)
	var root model.AnnouncementDelivery
	if err = storage.DB.Where("key = ?", c.CampaignID+":announcement").First(&root).Error; err != nil {
		t.Fatal(err)
	}
	if root.TargetChatID != -100200 || strings.Contains(root.Text, "internal-audit-secret") {
		t.Fatal("registration destination/privacy mismatch")
	}
	reservation, err := reserveOpenRegistrationSlot(2, c.CampaignID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reconcileChannelAnnouncements(bot)
	var n int64
	storage.DB.Model(&model.AnnouncementDelivery{}).Where("parent_key = ?", root.Key).Count(&n)
	if n != 0 {
		t.Fatal("temporary reservation prematurely ended announcement")
	}
	if err = releaseOpenRegistrationReservation(reservation.ID, 2, "simulated failure"); err != nil {
		t.Fatal(err)
	}
	reservation, err = reserveOpenRegistrationSlot(2, c.CampaignID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.DB.Transaction(func(tx *gorm.DB) error {
		return completeOpenRegistrationReservationInTx(tx, reservation.ID, 2, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	reconcileChannelAnnouncements(bot)
	reconcileChannelAnnouncements(bot)
	var ended model.AnnouncementDelivery
	if err = storage.DB.Where("parent_key = ?", root.Key).First(&ended).Error; err != nil {
		t.Fatal(err)
	}
	storage.DB.Model(&model.AnnouncementDelivery{}).Where("parent_key = ?", root.Key).Count(&n)
	if n != 1 || ended.TargetChatID != -100200 || ended.ReminderGroupID != -100300 || !strings.Contains(ended.Text, "已结束") {
		t.Fatal("quota completion must queue one edit and short reminder")
	}
}
func TestRegistrationTimeoutAndHistoricalCampaignExclusion(t *testing.T) {
	bot := channelOpsFixture(t)
	c, err := createOpenRegistrationCampaign(1, openRegistrationModeDuration, 1, "audit-only", time.Now().Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	historic := model.OpenRegistrationCampaign{CampaignID: "historical", Mode: openRegistrationModeDuration, Status: openRegistrationStatusClosed, CreatedBy: 1}
	if err = storage.DB.Create(&historic).Error; err != nil {
		t.Fatal(err)
	}
	reconcileChannelAnnouncements(bot)
	var root model.AnnouncementDelivery
	if err = storage.DB.Where("key = ?", c.CampaignID+":announcement").First(&root).Error; err != nil || !strings.Contains(root.Text, "已结束") {
		t.Fatal("overdue round announced as newly open")
	}
	var n int64
	storage.DB.Model(&model.AnnouncementDelivery{}).Where("key LIKE ?", "historical%").Count(&n)
	if n != 0 {
		t.Fatal("historical campaign broadcast")
	}
}
func TestCompensationChannelTextAndCompletionHaveNoPersonalDetails(t *testing.T) {
	bot := channelOpsFixture(t)
	c, err := createCompensationCampaign(1, 5, 3, "本次服务故障补偿", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config.AppConfig.AnnouncementChannelID = -100999
	reconcileChannelAnnouncements(bot)
	var root model.AnnouncementDelivery
	if err = storage.DB.Where("key = ?", c.CampaignID+":announcement").First(&root).Error; err != nil {
		t.Fatal(err)
	}
	if root.TargetChatID != -100200 || root.ReminderGroupID != 0 || strings.Contains(root.Text, "player") || !strings.Contains(root.Text, "每人 5 积分") {
		t.Fatal("public compensation content or destination incorrect")
	}
	if err = storage.DB.Model(&c).Updates(map[string]any{"status": compensationStatusCompleted, "last_error": "internal-password-secret"}).Error; err != nil {
		t.Fatal(err)
	}
	reconcileChannelAnnouncements(bot)
	var done model.AnnouncementDelivery
	if err = storage.DB.Where("parent_key = ?", root.Key).First(&done).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(done.Text, "secret") || !strings.Contains(done.Text, "已处理") || done.ReminderGroupID != 0 {
		t.Fatal("public completion leaks details or sends group reminder")
	}
}
func TestServiceAnnouncementRequiresSuperAdminPrivatePreviewAndConfirmation(t *testing.T) {
	bot := channelOpsFixture(t)
	defer telegram.ClearSession(1)
	defer telegram.ClearSession(2)
	send := func(user int64, kind, text string) {
		t.Helper()
		HandleChannelAnnouncementCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: user}, Chat: &tgbotapi.Chat{ID: user, Type: kind}}, text)
	}
	send(2, "private", "发布服务公告")
	send(1, "supergroup", "发布服务公告")
	var n int64
	storage.DB.Model(&model.AnnouncementDelivery{}).Count(&n)
	if n != 0 {
		t.Fatal("unauthorized service announcement")
	}
	send(1, "private", "发布服务公告")
	send(1, "private", "维护")
	send(1, "private", "今晚例行维护，预计持续 10 分钟。")
	storage.DB.Model(&model.AnnouncementDelivery{}).Count(&n)
	if n != 0 {
		t.Fatal("preview published without confirmation")
	}
	config.AppConfig.AnnouncementChannelID = -100999
	send(1, "private", "确认发布服务公告")
	var d model.AnnouncementDelivery
	if err := storage.DB.First(&d).Error; err != nil || d.TargetChatID != -100200 || d.Kind != "service" || d.ReminderGroupID != -100300 {
		t.Fatal("service confirmation lost preview snapshot")
	}
	send(1, "private", "确认发布服务公告")
	storage.DB.Model(&model.AnnouncementDelivery{}).Count(&n)
	if n != 1 {
		t.Fatal("duplicate confirmation published twice")
	}
	if command, superOnly, ok := adminMenuCommandText("service_announcement"); !ok || !superOnly || command != "发布服务公告" {
		t.Fatal("missing admin menu entry")
	}
}
func TestLotteryStartBypassesFastPathButNotMembershipGate(t *testing.T) {
	bot := channelOpsFixture(t)
	if handlePrivateStartFastPath(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "/start lottery_1"}) {
		t.Fatal("lottery deep link consumed by generic welcome")
	}
	queriedLottery := false
	if err := storage.DB.Callback().Query().Before("gorm:query").Register("test_forbid_unverified_lottery", func(tx *gorm.DB) {
		if tx.Statement.Table == "lottery_activities" {
			queriedLottery = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer storage.DB.Callback().Query().Remove("test_forbid_unverified_lottery")
	// The fake membership response has no member status and must be denied.
	handleInteractiveMessage(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}, Text: "/start lottery_1"})
	if queriedLottery {
		t.Fatal("unverified deep link reached lottery business logic")
	}
	if _, ok := telegram.UserSessions.Load(int64(2)); ok {
		telegram.ClearSession(2)
	}
}

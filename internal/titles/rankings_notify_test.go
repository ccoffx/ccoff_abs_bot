package titles

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/rankings"
	"ccoff-abs-bot/internal/storage"
)

type rankNoticeTestClient struct {
	sends          int
	response       string
	transportError error
	afterSend      func()
}

func (c *rankNoticeTestClient) Do(req *http.Request) (*http.Response, error) {
	body := `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"TestBot"}}`
	if strings.HasSuffix(req.URL.Path, "/sendMessage") {
		c.sends++
		if c.afterSend != nil {
			c.afterSend()
		}
		if c.transportError != nil {
			return nil, c.transportError
		}
		body = c.response
		if body == "" {
			body = `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func newRankNoticeTestBot(t *testing.T, client *rankNoticeTestClient) *tgbotapi.BotAPI {
	t.Helper()
	bot, err := tgbotapi.NewBotAPIWithClient("test", "https://example.invalid/bot%s/%s", client)
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestListeningBatchKeepsPublishedTitlesUntilAllUsersAreRefreshed(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Now()
	day := now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
	first := model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: day, CappedSeconds: 10000}
	second := model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: day, CappedSeconds: 9900}
	createActivityTitleRows(t, &first, &second, &model.Cultivation{UserID: 2, MajorRealm: 2, MinorRealm: 3, TotalAudioTime: 9900.0 / 3600})
	setScore := func(userID int64, seconds float64) {
		t.Helper()
		if err := storage.DB.Model(&model.DailyListeningStat{}).Where("user_id = ?", userID).Update("capped_seconds", seconds).Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.DB.Model(&model.Cultivation{}).Where("user_id = ?", userID).Update("total_audio_time", seconds/3600).Error; err != nil {
			t.Fatal(err)
		}
	}
	setScore(1, 10000)
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	if err := RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := Equip(-100, 1, rankings.ListeningTitle); err != nil {
		t.Fatal(err)
	}
	worn := titleTestEquipment(t, -100, 1)
	client := &rankNoticeTestClient{}
	bot := newRankNoticeTestBot(t, client)
	notify := func() {
		t.Helper()
		if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key <> ?", rankings.ListeningTitle).Update("notice_state", "sent").Error; err != nil {
			t.Fatal(err)
		}
		NotifyRankTitles(bot, now)
	}
	notify()
	for i := 0; i < 3; i++ {
		// 用户 2 的新数据暂时超过用户 1 的旧数据，后台/页面/佩戴
		// 都不能提前发布榜首、撤下佩戴或生成提醒。
		setScore(2, float64(10500+i*600))
		if err := RefreshRankTitles(now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Render(-100, 1, false); err != nil {
			t.Fatal(err)
		}
		if err := Equip(-100, 2, rankings.ListeningTitle); err != ErrNotHeld {
			t.Fatal("partial refresh allowed challenger to equip title", err)
		}
		for _, key := range []string{rankings.ListeningTitle, rankings.CultivationTitle} {
			rankHolderTest(t, key, 1)
		}
		notify()
		setScore(1, float64(10600+i*600))
		if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
			t.Fatal(err)
		}
		notify()
	}
	if client.sends != 1 {
		t.Fatal("intermediate snapshots generated acquisition notices", client.sends)
	}
	if e := titleTestEquipment(t, -100, 1); e.TitleKey != worn.TitleKey || e.Version != worn.Version {
		t.Fatal("intermediate snapshots changed equipment", e)
	}
	// 真正完整刷新后易主及同月夺回，均应正常提醒。
	for _, step := range []struct {
		userID  int64
		seconds float64
	}{{2, 13000}, {1, 14000}} {
		setScore(step.userID, step.seconds)
		if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{rankings.ListeningTitle, rankings.CultivationTitle} {
			rankHolderTest(t, key, step.userID)
		}
		notify()
	}
	if client.sends != 3 {
		t.Fatal("genuine same-month reacquisition was suppressed", client.sends)
	}
}

func TestListeningTitleExpiresBeforeNextCompletedBatch(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Date(2026, 11, 30, 15, 59, 0, 0, time.UTC)
	createActivityTitleRows(t,
		&model.DailyListeningStat{UserID: 1, AbsUserID: "abs1", DayKey: "2026-11-30", CappedSeconds: 3600},
		&model.DailyListeningStat{UserID: 2, AbsUserID: "abs2", DayKey: "2026-12-01", CappedSeconds: 7200})
	if err := RefreshRankTitlesAfterListeningBatch(now, now); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 1)
	if err := RefreshRankTitles(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 0)
	if err := RefreshRankTitlesAfterListeningBatch(now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rankHolderTest(t, rankings.ListeningTitle, 2)
}

func seedRankNoticeTest(t *testing.T) model.RankTitleHistory {
	t.Helper()
	refreshRankTest(t)
	// 本测试只观察财富榜的一条提醒。
	if err := storage.DB.Model(&model.RankTitleHistory{}).Where("title_key <> ?", rankings.WealthTitle).Update("notice_state", "sent").Error; err != nil {
		t.Fatal(err)
	}
	var h model.RankTitleHistory
	if err := storage.DB.Where("title_key = ?", rankings.WealthTitle).Take(&h).Error; err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRankNoticeRetriesConfirmedFailureButNotUnknownDelivery(t *testing.T) {
	setupTitleTestDB(t)
	h := seedRankNoticeTest(t)
	client := &rankNoticeTestClient{response: `{"ok":false,"error_code":429,"description":"retry later","parameters":{"retry_after":120}}`}
	bot := newRankNoticeTestBot(t, client)
	now := time.Now()
	notify := func(at time.Time) { notifyRankTitles(bot, func() time.Time { return at }) }
	notify(now)
	if err := storage.DB.First(&h, h.ID).Error; err != nil || h.NoticeState != "pending" || !h.NextNoticeAt.Equal(now.Add(120*time.Second)) {
		t.Fatal("confirmed rate limit lost retry", h, err)
	}
	client.response = ""
	notify(now.Add(time.Minute))
	if client.sends != 1 {
		t.Fatal("retry ignored Telegram delay")
	}
	notify(now.Add(2 * time.Minute))
	notify(now.Add(3 * time.Minute))
	if client.sends != 2 {
		t.Fatal("successful notification duplicated", client.sends)
	}
	// 抢回后又有一条新提醒。传输中断不能证明 Telegram 未收到，不能自动重发。
	updateRankUserTest(t, 2, map[string]any{"points": 1})
	refreshRankTest(t)
	client.transportError = io.ErrUnexpectedEOF
	notify(now.Add(4 * time.Minute))
	notify(now.Add(5 * time.Minute))
	if client.sends != 3 {
		t.Fatal("ambiguous delivery was retried", client.sends)
	}
	var latest model.RankTitleHistory
	if err := storage.DB.Where("title_key = ? AND user_id = ?", rankings.WealthTitle, 2).Take(&latest).Error; err != nil || latest.NoticeState != "uncertain" {
		t.Fatal("ambiguous delivery not recorded", latest, err)
	}
}

func TestRankNoticeLostAcknowledgementAndReleasedAwardsDoNotResend(t *testing.T) {
	setupTitleTestDB(t)
	h := seedRankNoticeTest(t)
	client := &rankNoticeTestClient{}
	bot := newRankNoticeTestBot(t, client)
	client.afterSend = func() {
		if err := storage.DB.Exec(`CREATE TRIGGER fail_notice_ack BEFORE UPDATE ON rank_title_histories WHEN NEW.notice_state = 'sent' BEGIN SELECT RAISE(FAIL, 'ack unavailable'); END`).Error; err != nil {
			t.Error(err)
		}
	}
	NotifyRankTitles(bot, time.Now())
	if err := storage.DB.First(&h, h.ID).Error; err != nil || h.NoticeState != "sending" {
		t.Fatal("lost local acknowledgement fixture failed", h, err)
	}
	if err := storage.DB.Exec("DROP TRIGGER fail_notice_ack").Error; err != nil {
		t.Fatal(err)
	}
	client.afterSend = nil
	NotifyRankTitles(bot, time.Now().Add(time.Minute))
	if client.sends != 1 {
		t.Fatal("restart resent an already delivered award", client.sends)
	}
	if err := storage.DB.First(&h, h.ID).Error; err != nil || h.NoticeState != "uncertain" {
		t.Fatal("in-flight recovery not durable", h, err)
	}
	updateRankUserTest(t, 2, map[string]any{"points": 1})
	refreshRankTest(t)
	updateRankUserTest(t, 3, map[string]any{"points": 2})
	refreshRankTest(t)
	NotifyRankTitles(bot, time.Now().Add(2*time.Minute))
	if client.sends != 2 {
		t.Fatal("released award sent stale notice", client.sends)
	}
}

func TestRankNoticeSkipsExpiredPeriodsBeforeOwnershipRefresh(t *testing.T) {
	setupTitleTestDB(t)
	// 北京时间 2026-12-01，当前周始于 11-30。
	now := time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC)
	rows := []model.RankTitleHistory{
		{TitleKey: rankings.ListeningTitle, PeriodKey: "2026-11", UserID: 1, NoticeState: "pending"},
		{TitleKey: rankings.BossTitle, PeriodKey: "2026-11-23", UserID: 1, NoticeState: "pending"},
		{TitleKey: rankings.ListeningTitle, PeriodKey: "2026-12", UserID: 2, NoticeState: "pending"},
		{TitleKey: rankings.BossTitle, PeriodKey: "2026-11-30", UserID: 2, NoticeState: "pending"},
		{TitleKey: rankings.AlchemyTitle, UserID: 1, NoticeState: "pending"},
	}
	for i := range rows {
		createActivityTitleRows(t, &rows[i])
	}
	client := &rankNoticeTestClient{}
	NotifyRankTitles(newRankNoticeTestBot(t, client), now)
	if client.sends != 3 {
		t.Fatal("expired monthly/weekly awards were sent", client.sends)
	}
	for i, row := range rows {
		if err := storage.DB.First(&row, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		want := "sent"
		if i < 2 {
			want = "obsolete"
		}
		if row.NoticeState != want {
			t.Fatal("wrong period notice state", row)
		}
	}
}

func TestRankNoticeRechecksPeriodAfterPreviousNetworkRequest(t *testing.T) {
	setupTitleTestDB(t)
	now := time.Date(2026, 11, 30, 15, 59, 59, 0, time.UTC)
	first := model.RankTitleHistory{TitleKey: rankings.WealthTitle, UserID: 1, NoticeState: "pending"}
	monthly := model.RankTitleHistory{TitleKey: rankings.ListeningTitle, PeriodKey: "2026-11", UserID: 1, NoticeState: "pending"}
	createActivityTitleRows(t, &first, &monthly)
	client := &rankNoticeTestClient{afterSend: func() { now = now.Add(2 * time.Second) }}
	notifyRankTitles(newRankNoticeTestBot(t, client), func() time.Time { return now })
	if client.sends != 1 {
		t.Fatal("previous request crossed midnight but an expired award was still sent", client.sends)
	}
	if err := storage.DB.First(&monthly, monthly.ID).Error; err != nil || monthly.NoticeState != "obsolete" {
		t.Fatal("expired award was not withdrawn", monthly, err)
	}
}

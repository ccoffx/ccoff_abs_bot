package activity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/titles"
)

type tribulationTelegramFixture struct {
	mu                   sync.Mutex
	bot                  *tgbotapi.BotAPI
	failPublish, failTag bool
	editError            string
	tags                 map[int64]string
	statuses             map[int64]string
	edits, answers       []string
	setTags              int
	tagAttempts          int
	tagRetryAfter        int
	afterTagRateLimit    func()
	afterSet             func()
	afterPublish         func()
	beforeEdit           func()
}

func newTribulationTelegramFixture(t *testing.T) *tribulationTelegramFixture {
	t.Helper()
	f := &tribulationTelegramFixture{tags: map[int64]string{}, statuses: map[int64]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		var result any = true
		var hook func()
		switch method {
		case "getMe":
			result = map[string]any{"id": 99, "is_bot": true, "first_name": "TestBot", "username": "test_bot"}
		case "sendMessage":
			if f.failPublish && r.Form.Get("reply_markup") != "" {
				f.mu.Unlock()
				fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"forbidden"}`)
				return
			}
			chatID, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
			kind := "supergroup"
			if chatID > 0 {
				kind = "private"
			}
			result = map[string]any{"message_id": 123, "chat": map[string]any{"id": chatID, "type": kind}, "text": r.Form.Get("text")}
			if r.Form.Get("reply_markup") != "" {
				hook = f.afterPublish
				f.afterPublish = nil
			}
		case "editMessageText":
			hook = f.beforeEdit
			f.beforeEdit = nil
			if f.editError != "" {
				description := f.editError
				f.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": description})
				return
			}
			f.edits = append(f.edits, r.Form.Get("text"))
		case "answerCallbackQuery":
			f.answers = append(f.answers, r.Form.Get("text"))
		case "getChatMember":
			id, _ := strconv.ParseInt(r.Form.Get("user_id"), 10, 64)
			status := f.statuses[id]
			if status == "" {
				status = "member"
			}
			result = map[string]any{"user": map[string]any{"id": id, "first_name": "user"}, "status": status, "tag": f.tags[id]}
		case "setChatMemberTag":
			f.tagAttempts++
			if f.tagRetryAfter > 0 {
				retryAfter := f.tagRetryAfter
				hook = f.afterTagRateLimit
				f.afterTagRateLimit = nil
				f.mu.Unlock()
				if hook != nil {
					hook()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": retryAfter}})
				return
			}
			if f.failTag {
				f.mu.Unlock()
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"not enough rights to manage tags"}`)
				return
			}
			id, _ := strconv.ParseInt(r.Form.Get("user_id"), 10, 64)
			f.tags[id] = r.Form.Get("tag")
			f.setTags++
			hook = f.afterSet
			f.afterSet = nil
		default:
			t.Errorf("unexpected Telegram method %s", method)
		}
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.bot = bot
	return f
}

func TestTribulationFinalPanelRetryAndDeletedPanel(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	r := newTribulationTestRound(t, -100, time.Now())
	tribulationTestChoose(t, r, 1, TribulationYang, false, r.CreatedAt)
	settleTribulationTestRound(t, r, TribulationYang)
	f.mu.Lock()
	f.editError = "temporary server error"
	f.mu.Unlock()
	ProcessTribulationRounds(f.bot, r.ExpiresAt)
	if err := storage.DB.First(&r, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !r.PanelDirty {
		t.Fatal("transient failure lost final panel retry")
	}
	f.mu.Lock()
	f.editError = "Bad Request: message to edit not found"
	f.mu.Unlock()
	ProcessTribulationRounds(f.bot, r.ExpiresAt)
	if err := storage.DB.First(&r, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if r.PanelDirty {
		t.Fatal("deleted final panel retried forever")
	}
	if tribulationTestBalance(t, 1) != 105 {
		t.Fatal("panel failure affected payout")
	}
}

func TestTribulationSettlesEveryGroupBeforeEditingAnyPanel(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	now := time.Now()
	a := newTribulationTestRound(t, -100, now)
	b := newTribulationTestRound(t, -200, now)
	tribulationTestChoose(t, a, 1, TribulationYang, false, now)
	tribulationTestChoose(t, b, 2, TribulationYang, false, now)
	for _, round := range []model.TribulationRound{a, b} {
		if err := storage.DB.Model(&round).Updates(map[string]any{"status": TribulationSettling, "winning_side": TribulationYang}).Error; err != nil {
			t.Fatal(err)
		}
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeEdit = func() { close(started); <-release }
	f.mu.Unlock()
	go func() { defer close(done); ProcessTribulationRounds(f.bot, a.ExpiresAt) }()
	t.Cleanup(func() { close(release); <-done })
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("panel request not started")
	}
	if got := tribulationTestBalance(t, 2); got != 105 {
		t.Fatalf("another group's slow panel request blocked reward: %d", got)
	}
}

func TestTribulationPanelUsesCurrentPhaseAfterSlowEarlierRequest(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	now := time.Now()
	a := newTribulationTestRound(t, -100, now)
	newTribulationTestRound(t, -200, now)
	var clockMu sync.Mutex
	at := now
	f.beforeEdit = func() {
		clockMu.Lock()
		at = a.JoinUntil
		clockMu.Unlock()
	}
	refreshTribulationPanels(f.bot, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return at
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) != 2 || !strings.Contains(f.edits[1], "已停止入阵") {
		t.Fatal("later group's panel used the timestamp from before the slow request", f.edits)
	}
}

func TestTribulationNewPanelOpensWhileAnotherPanelIsSlow(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	r := newTribulationTestRound(t, -100, time.Now())
	started, release, refreshed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var done chan struct{}
	f.beforeEdit = func() { close(started); <-release }
	go func() {
		defer close(refreshed)
		refreshTribulationPanel(f.bot, r.ID, time.Now)
	}()
	t.Cleanup(func() {
		close(release)
		<-refreshed
		if done != nil {
			<-done
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first panel request did not start")
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		msg := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: -200, Type: "supergroup"}, From: &tgbotapi.User{ID: 2}}
		HandleTribulationCommand(f.bot, msg, "猜阵")
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("another group's slow edit blocked the new panel from opening")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) < 2 || !strings.Contains(f.edits[1], "秒可以入阵") {
		t.Fatal("new panel was left preparing instead of opening immediately", f.edits)
	}
}

func TestTribulationCallbackDoesNotWaitForPanelRequest(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	now := time.Now()
	r := newTribulationTestRound(t, -100, now)
	cb := &tgbotapi.CallbackQuery{ID: "join", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: r.MessageID, Chat: &tgbotapi.Chat{ID: r.ChatID, Type: "supergroup"}}, Data: fmt.Sprintf("trib:join:%d:yang", r.ID)}
	HandleTribulationCallback(f.bot, cb)
	f.mu.Lock()
	edits := len(f.edits)
	f.mu.Unlock()
	if edits != 0 || tribulationTestBalance(t, 1) != 95 {
		t.Fatal("callback waited for panel I/O or lost entry")
	}
	ProcessTribulationRounds(f.bot, now)
	f.mu.Lock()
	out := strings.Join(f.edits, "\n")
	f.mu.Unlock()
	if !strings.Contains(out, "阳阵：1 人") {
		t.Fatal("background panel refresh lost callback changes", out)
	}
}

func TestTribulationTelegramFlowAndFailedPublication(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	msg := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup", Title: "避雷群"}, From: &tgbotapi.User{ID: 1}, Text: "猜阵"}
	f.mu.Lock()
	f.failPublish = true
	f.mu.Unlock()
	if !HandleTribulationCommand(f.bot, msg, msg.Text) {
		t.Fatal("command not handled")
	}
	var failed model.TribulationRound
	if err := storage.DB.Order("id DESC").First(&failed).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != TribulationCancelled || tribulationTestBalance(t, 1) != 100 {
		t.Fatal("publication failure left active round or charged user")
	}
	f.mu.Lock()
	f.failPublish = false
	f.mu.Unlock()
	HandleTribulationCommand(f.bot, msg, msg.Text)
	var r model.TribulationRound
	if err := storage.DB.Order("id DESC").First(&r).Error; err != nil {
		t.Fatal(err)
	}
	if r.Status != TribulationActive || r.MessageID != 123 {
		t.Fatal("panel not activated")
	}
	callback := func(id int64, action, side string, messageID int) {
		cb := &tgbotapi.CallbackQuery{ID: "cb", From: &tgbotapi.User{ID: id, FirstName: fmt.Sprintf("玩家%d", id)}, Message: &tgbotapi.Message{MessageID: messageID, Chat: msg.Chat}, Data: fmt.Sprintf("trib:%s:%d:%s", action, r.ID, side)}
		if !HandleTribulationCallback(f.bot, cb) {
			t.Fatal("callback not handled")
		}
	}
	callback(1, "join", TribulationYang, r.MessageID)
	callback(1, "join", TribulationYang, r.MessageID)
	callback(2, "join", TribulationYin, r.MessageID)
	callback(3, "join", TribulationYang, r.MessageID+1)
	if tribulationTestBalance(t, 1) != 95 || tribulationTestBalance(t, 2) != 95 || tribulationTestBalance(t, 3) != 100 {
		t.Fatal("callback charged twice or accepted forged panel")
	}
	if err := storage.DB.Model(&r).Update("join_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	callback(1, "switch", TribulationYin, r.MessageID)
	callback(1, "switch", TribulationYang, r.MessageID)
	ProcessTribulationRounds(f.bot, r.SwitchUntil)
	ProcessTribulationRounds(f.bot, r.ExpiresAt)
	if err := storage.DB.First(&r, r.ID).Error; err != nil {
		t.Fatal(err)
	}
	if r.Status != TribulationClosed || r.TitleEligible {
		t.Fatal("wrong settlement or single-side title eligibility")
	}
	want := 95
	if r.WinningSide == TribulationYin {
		want = 105
	}
	if tribulationTestBalance(t, 1) != want || tribulationTestBalance(t, 2) != want {
		t.Fatal("wrong callback settlement")
	}
	f.mu.Lock()
	allEdits := strings.Join(f.edits, "\n")
	f.mu.Unlock()
	if !strings.Contains(allEdits, "两阵已封锁") || !strings.Contains(allEdits, "避雷成功") || !strings.Contains(allEdits, "玩家1：☀️ 阳阵 → 🌙 阴阵") {
		t.Fatal("phase, switch or result missing", allEdits)
	}
}

func TestTribulationPublishedPanelActivationFailureCanRecover(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			setupTribulationTestDB(t)
			f := newTribulationTelegramFixture(t)
			if expired {
				// 发布请求尚未返回时，巡检已取消了超时的准备中场次。
				f.afterPublish = func() { settleTribulationRounds(time.Now().Add(time.Minute)) }
			} else if err := storage.DB.Exec(`CREATE TRIGGER fail_activation BEFORE UPDATE ON tribulation_rounds WHEN NEW.status = 'active' BEGIN SELECT RAISE(ABORT,'activation failure'); END`).Error; err != nil {
				t.Fatal(err)
			}
			f.editError = "temporary server error"
			msg := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup"}, From: &tgbotapi.User{ID: 1}}
			HandleTribulationCommand(f.bot, msg, "猜阵")
			var r model.TribulationRound
			if err := storage.DB.Order("id DESC").First(&r).Error; err != nil {
				t.Fatal(err)
			}
			if r.Status != TribulationCancelled || r.MessageID != 123 || !r.PanelDirty {
				t.Fatalf("published panel cannot be recovered after activation failure: %+v", r)
			}
			if tribulationTestBalance(t, 1) != 100 {
				t.Fatal("failed activation charged points")
			}
			ProcessTribulationRounds(f.bot, time.Now())
			if err := storage.DB.First(&r, r.ID).Error; err != nil || !r.PanelDirty {
				t.Fatal("temporary edit failure lost cancellation retry", err)
			}
			f.mu.Lock()
			f.editError = ""
			f.mu.Unlock()
			ProcessTribulationRounds(f.bot, time.Now())
			if err := storage.DB.First(&r, r.ID).Error; err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			out := strings.Join(f.edits, "\n")
			f.mu.Unlock()
			if r.PanelDirty || !strings.Contains(out, "本局已取消") {
				t.Fatal("background retry did not cancel the published panel", out)
			}
		})
	}
}

func TestTribulationPublishingPanelHasNoEntryButtons(t *testing.T) {
	setupTribulationTestDB(t)
	r, err := CreateTribulationRound(-100, "测试群", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out, markup := RenderTribulationPanel(r, nil, r.CreatedAt)
	if len(markup.InlineKeyboard) != 0 || !strings.Contains(out, "准备中") {
		t.Fatal("unactivated panel invited users into an unavailable round", out)
	}
}

func seedTribulationTestTitle(t *testing.T, id int64, key string) {
	t.Helper()
	if err := storage.DB.Create(&model.TribulationTitleHolder{ChatID: -100, TitleKey: key, UserID: id, UserName: "玩家", Count: 1, AcquiredAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := titles.TrackMember(-100, "测试群", id); err != nil {
		t.Fatal(err)
	}
}

func TestTitleCallbacksRejectForeignGroup(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	if err := titles.TrackMember(-200, "其他群", 2); err != nil {
		t.Fatal(err)
	}
	cb := &tgbotapi.CallbackQuery{ID: "foreign-group", From: &tgbotapi.User{ID: 1}, Message: &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: 1, Type: "private"}}, Data: "titles:view:-200"}
	if !titles.HandleCallback(f.bot, cb) {
		t.Fatal("title callback not handled")
	}
	f.mu.Lock()
	edits := len(f.edits)
	f.mu.Unlock()
	if edits != 0 {
		t.Fatal("forged callback exposed another group's private title panel")
	}
	cb.Data = "titles:clear:-200"
	titles.HandleCallback(f.bot, cb)
	var members int64
	if err := storage.DB.Model(&model.TitleMember{}).Where("chat_id = ? AND user_id = ?", -200, 1).Count(&members).Error; err != nil || members != 0 {
		t.Fatal("forged callback enrolled user into another group", members, err)
	}
}

func TestRealmTitleNativeTagLifecycleWithoutPlaying(t *testing.T) {
	setupTribulationTestDB(t)
	previous := config.AppConfig
	config.AppConfig = &config.Config{}
	t.Cleanup(func() { config.AppConfig = previous })
	if err := storage.DB.AutoMigrate(&model.Cultivation{}, &model.CultivationRealmConfig{}, &model.CultivationMinorRealmConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("abs_user_id", "bound-account").Error; err != nil {
		t.Fatal(err)
	}
	f := newTribulationTelegramFixture(t)
	msg := &tgbotapi.Message{From: &tgbotapi.User{ID: 1}, Chat: &tgbotapi.Chat{ID: -100, Type: "supergroup", Title: "新群"}}
	if !titles.HandleCommand(f.bot, msg, "称号榜") {
		t.Fatal("first-use title command not handled")
	}
	if _, _, err := titles.Render(-100, 1, false); err != nil {
		t.Fatal("group titles still require game participation", err)
	}
	assertTag := func(want string) {
		t.Helper()
		titles.SyncTags(f.bot, time.Now())
		f.mu.Lock()
		got := f.tags[1]
		f.mu.Unlock()
		if got != want {
			t.Fatalf("native tag = %q, want %q", got, want)
		}
	}
	assertTag("斗之气一段")
	if err := storage.DB.Create(&model.Cultivation{UserID: 1, MajorRealm: 9, MinorRealm: 10}).Error; err != nil {
		t.Fatal(err)
	}
	if err := titles.RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	assertTag("斗尊巅峰十转")
	seedTribulationTestTitle(t, 1, "escape")
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	assertTag("临阵脱身")
	if err := titles.Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	assertTag("斗尊巅峰十转")
	if err := storage.DB.Model(&model.User{}).Where("telegram_id = ?", 1).Update("status", "unbound").Error; err != nil {
		t.Fatal(err)
	}
	if err := titles.RefreshDefaults(); err != nil {
		t.Fatal(err)
	}
	assertTag("")
	if tribulationTestCount(t, &model.TribulationRound{}) != 0 {
		t.Fatal("default titles created game records")
	}
}

func TestTribulationTagRetriesRevocationAndAdministratorPreservation(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	seedTribulationTestTitle(t, 1, "escape")
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failTag = true
	f.mu.Unlock()
	now := time.Now()
	titles.SyncTags(f.bot, now)
	var e model.TitleEquipment
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
		t.Fatal(err)
	}
	if e.SyncedVersion >= e.Version || e.SyncHint == "" {
		t.Fatal("permission failure lost retry")
	}
	f.mu.Lock()
	f.failTag = false
	f.mu.Unlock()
	titles.SyncTags(f.bot, now.Add(2*time.Minute))
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	tag := f.tags[1]
	f.mu.Unlock()
	if tag != "临阵脱身" || e.SyncedVersion != e.Version {
		t.Fatal("tag retry not synchronized", e, tag)
	}
	if err := titles.Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(3*time.Minute))
	f.mu.Lock()
	tag = f.tags[1]
	f.mu.Unlock()
	if tag != "" {
		t.Fatal("lost title tag not cleared")
	}
	seedTribulationTestTitle(t, 2, "steadfast")
	f.mu.Lock()
	f.statuses[2] = "administrator"
	f.tags[2] = "群管理"
	before := f.setTags
	f.mu.Unlock()
	if err := titles.Equip(-100, 2, "steadfast"); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(4*time.Minute))
	f.mu.Lock()
	tag = f.tags[2]
	after := f.setTags
	f.mu.Unlock()
	if tag != "群管理" || after != before {
		t.Fatal("administrator title overwritten")
	}
	var adminEquipment model.TitleEquipment
	if err := storage.DB.First(&adminEquipment, "chat_id = ? AND user_id = ?", -100, 2).Error; err != nil {
		t.Fatal(err)
	}
	if adminEquipment.TitleKey != "steadfast" || !strings.Contains(adminEquipment.SyncHint, "管理员保留") {
		t.Fatal("admin profile title unavailable")
	}
}

func TestTitleTagSyncHonorsTelegramRetryAfter(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	seedTribulationTestTitle(t, 1, "escape")
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.tagRetryAfter = 180
	f.mu.Unlock()
	now := time.Now()
	titles.SyncTags(f.bot, now)
	var e model.TitleEquipment
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
		t.Fatal(err)
	}
	if e.NextSyncAt.Before(now.Add(180*time.Second)) || e.SyncedVersion >= e.Version {
		t.Fatal("tag flood wait was ignored or marked successful", e)
	}
	titles.SyncTags(f.bot, now.Add(2*time.Minute))
	f.mu.Lock()
	attempts := f.tagAttempts
	f.tagRetryAfter = 0
	f.mu.Unlock()
	if attempts != 1 {
		t.Fatal("tag sync retried before Telegram's deadline", attempts)
	}
	titles.SyncTags(f.bot, now.Add(4*time.Minute))
	f.mu.Lock()
	got, attempts := f.tags[1], f.tagAttempts
	f.mu.Unlock()
	if got != "临阵脱身" || attempts != 2 {
		t.Fatal("tag sync did not recover after flood wait", got, attempts)
	}
}

func TestTitleTagRateLimitSurvivesEquipmentChangesAndRestart(t *testing.T) {
	for _, duringRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("during_request_%t", duringRequest), func(t *testing.T) {
			path := setupTribulationTestDB(t)
			f := newTribulationTelegramFixture(t)
			seedTribulationTestTitle(t, 1, "escape")
			seedTribulationTestTitle(t, 1, "steadfast")
			if err := titles.Equip(-100, 1, "escape"); err != nil {
				t.Fatal(err)
			}
			changeTitle := func() {
				if err := titles.Equip(-100, 1, "steadfast"); err != nil {
					t.Error(err)
				}
			}
			f.mu.Lock()
			f.tagRetryAfter = 180
			if duringRequest {
				f.afterTagRateLimit = changeTitle
			}
			f.mu.Unlock()
			now := time.Now()
			titles.SyncTags(f.bot, now)
			if !duringRequest {
				changeTitle()
			}
			f.mu.Lock()
			f.tagRetryAfter = 0
			f.mu.Unlock()
			// 重新打开数据库，限流截止时间必须随任务一起恢复。
			sqlDB, err := storage.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlDB.Close(); err != nil {
				t.Fatal(err)
			}
			storage.DB = openTribulationTestDB(t, path)
			titles.SyncTags(f.bot, now.Add(time.Minute))
			f.mu.Lock()
			attempts := f.tagAttempts
			f.mu.Unlock()
			if attempts != 1 {
				t.Fatal("equipment change or restart bypassed the server's flood wait", attempts)
			}
			titles.SyncTags(f.bot, now.Add(4*time.Minute))
			f.mu.Lock()
			got, attempts := f.tags[1], f.tagAttempts
			f.mu.Unlock()
			var e model.TitleEquipment
			if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
				t.Fatal(err)
			}
			if got != "道心坚定" || attempts != 2 || e.Version != e.SyncedVersion {
				t.Fatal("latest equipment did not synchronize after the flood wait", got, attempts, e)
			}
		})
	}
}

func TestTribulationTagNewVersionAndLostLocalAcknowledgement(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	seedTribulationTestTitle(t, 1, "escape")
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	// Telegram 已应用标签，但保存本地确认时失败。
	if err := storage.DB.Exec(`CREATE TRIGGER fail_tag_ack BEFORE UPDATE ON title_equipments WHEN NEW.synced_version > OLD.synced_version BEGIN SELECT RAISE(ABORT,'ack failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	titles.SyncTags(f.bot, now)
	if err := storage.DB.Exec("DROP TRIGGER fail_tag_ack").Error; err != nil {
		t.Fatal(err)
	}
	if err := titles.Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(time.Minute))
	f.mu.Lock()
	tag := f.tags[1]
	f.mu.Unlock()
	if tag != "" {
		t.Fatal("unacknowledged external tag was orphaned")
	}
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.afterSet = func() {
		if err := titles.Equip(-100, 1, ""); err != nil {
			t.Error(err)
		}
	}
	f.mu.Unlock()
	titles.SyncTags(f.bot, now.Add(2*time.Minute))
	var e model.TitleEquipment
	if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 1).Error; err != nil {
		t.Fatal(err)
	}
	if e.Version <= e.SyncedVersion {
		t.Fatal("new version incorrectly marked synchronized")
	}
	titles.SyncTags(f.bot, now.Add(3*time.Minute))
	f.mu.Lock()
	tag = f.tags[1]
	f.mu.Unlock()
	if tag != "" {
		t.Fatal("old external request overrode new unequip")
	}
	// 撤下时保留管理员后来手动设置的其他标签。
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(4*time.Minute))
	f.mu.Lock()
	f.tags[1] = "手动标签"
	f.mu.Unlock()
	if err := titles.Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(5*time.Minute))
	f.mu.Lock()
	tag = f.tags[1]
	f.mu.Unlock()
	if tag != "手动标签" {
		t.Fatal("manual tag was erased")
	}
}

func TestTagBatchReloadsLaterMemberAfterEarlierNetworkRequest(t *testing.T) {
	for _, key := range []string{"", "backfire"} {
		t.Run("new_title_"+key, func(t *testing.T) {
			setupTribulationTestDB(t)
			f := newTribulationTelegramFixture(t)
			seedTribulationTestTitle(t, 1, "escape")
			seedTribulationTestTitle(t, 2, "steadfast")
			seedTribulationTestTitle(t, 2, "backfire")
			if err := titles.Equip(-100, 1, "escape"); err != nil {
				t.Fatal(err)
			}
			if err := titles.Equip(-100, 2, "steadfast"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.tags[2] = "手动标签"
			f.afterSet = func() {
				// 第一名成员的网络请求尚未返回时，第二名已切换或撤下佩戴。
				if err := titles.Equip(-100, 2, key); err != nil {
					t.Error(err)
				}
			}
			f.mu.Unlock()
			now := time.Now()
			titles.SyncTags(f.bot, now)
			f.mu.Lock()
			got := f.tags[2]
			f.mu.Unlock()
			want := "手动标签"
			if key == "backfire" {
				want = "自投雷网"
			}
			if got != want {
				t.Fatalf("queued snapshot overwrote newer equipment/manual tag: got %q, want %q", got, want)
			}
			var e model.TitleEquipment
			if err := storage.DB.First(&e, "chat_id = ? AND user_id = ?", -100, 2).Error; err != nil || e.Version != e.SyncedVersion {
				t.Fatal("latest queued equipment was not synchronized", e, err)
			}
		})
	}
}

func TestTagCleanupRetainsEarlierUnacknowledgedAttempt(t *testing.T) {
	setupTribulationTestDB(t)
	f := newTribulationTelegramFixture(t)
	seedTribulationTestTitle(t, 1, "escape")
	seedTribulationTestTitle(t, 1, "steadfast")
	if err := titles.Equip(-100, 1, "escape"); err != nil {
		t.Fatal(err)
	}
	if err := storage.DB.Exec(`CREATE TRIGGER fail_tag_ack BEFORE UPDATE ON title_equipments WHEN NEW.synced_version > OLD.synced_version BEGIN SELECT RAISE(ABORT,'ack failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	titles.SyncTags(f.bot, now)
	if err := storage.DB.Exec("DROP TRIGGER fail_tag_ack").Error; err != nil {
		t.Fatal(err)
	}
	// 上个标签已写入 Telegram，但本地未确认；新标签的请求明确失败。
	f.mu.Lock()
	f.failTag = true
	f.mu.Unlock()
	if err := titles.Equip(-100, 1, "steadfast"); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(time.Minute))
	f.mu.Lock()
	f.failTag = false
	f.mu.Unlock()
	if err := titles.Equip(-100, 1, ""); err != nil {
		t.Fatal(err)
	}
	titles.SyncTags(f.bot, now.Add(2*time.Minute))
	f.mu.Lock()
	got := f.tags[1]
	f.mu.Unlock()
	if got != "" {
		t.Fatalf("earlier unacknowledged tag was orphaned: %q", got)
	}
}

func TestTribulationPanelsBoundedAndPrivateTitleControls(t *testing.T) {
	setupTribulationTestDB(t)
	r := newTribulationTestRound(t, -100, time.Now())
	var entries []model.TribulationEntry
	for i := range 1000 {
		entries = append(entries, model.TribulationEntry{UserID: int64(i + 1), UserName: strings.Repeat("名", 30), Side: TribulationYang, InitialSide: TribulationYin, Cost: 5, Reward: 10, SwitchedAt: &r.JoinUntil})
	}
	for _, at := range []time.Time{r.CreatedAt, r.JoinUntil, r.SwitchUntil, r.ExpiresAt} {
		r.WinningSide = TribulationYin
		first, _ := RenderTribulationPanel(r, entries, at)
		r.WinningSide = TribulationYang
		second, _ := RenderTribulationPanel(r, entries, at)
		if first != second {
			t.Fatal("unsettled panel leaked result")
		}
		if len(utf16.Encode([]rune(first))) > 4096 {
			t.Fatal("active panel too large")
		}
	}
	r.Status = TribulationClosed
	r.TitleEligible = true
	closed, markup := RenderTribulationPanel(r, entries, r.ExpiresAt)
	if len(utf16.Encode([]rune(closed))) > 4096 || len(markup.InlineKeyboard) != 1 {
		t.Fatal("closed panel cannot handle unlimited players")
	}
	f := newTribulationTelegramFixture(t)
	seedTribulationTestTitle(t, 1, "escape")
	cb := &tgbotapi.CallbackQuery{ID: "title-cb", From: &tgbotapi.User{ID: 2}, Message: &tgbotapi.Message{MessageID: 123, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}, Data: "titles:equip:-100:escape"}
	titles.HandleCallback(f.bot, cb)
	if tribulationTestCount(t, &model.TitleEquipment{}) != 0 {
		t.Fatal("another user equipped title")
	}
	cb.From.ID = 1 // 同一面板属于另一个人的私聊。
	titles.HandleCallback(f.bot, cb)
	if tribulationTestCount(t, &model.TitleEquipment{}) != 0 {
		t.Fatal("foreign private panel accepted")
	}
	cb.Message.Chat.ID = 1
	titles.HandleCallback(f.bot, cb)
	if tribulationTestCount(t, &model.TitleEquipment{}) != 1 {
		t.Fatal("owner could not equip")
	}
}

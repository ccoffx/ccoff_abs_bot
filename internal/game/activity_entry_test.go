package game

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func waitActivityEntries(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		for _, q := range []interface{ WaitIdle(context.Context) error }{activityEntryDispatch, activityEntryWorkers, activityEntryDispatch, activityEntryReceipts, activityEntryMessages} {
			if err := q.WaitIdle(ctx); err != nil {
				t.Fatal("activity entry queue did not drain", err)
			}
		}
		if storage.DB == nil || !storage.DB.Migrator().HasTable(&model.ActivityEntryRequest{}) {
			return
		}
		var due int64
		contributionMust(t, storage.DB.Model(&model.ActivityEntryRequest{}).Where("status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ? OR deadline_at <= ?)", "pending", time.Now(), time.Now()).Count(&due).Error)
		if due == 0 {
			return
		}
		time.Sleep(time.Millisecond) // Allow the finishing worker's dispatch signal to run.
		select {
		case <-ctx.Done():
			t.Fatal("entry requests remained due")
		default:
		}
	}
}

func entryTestFixture(t *testing.T, kind string) (string, int64, *tgbotapi.Message, func(*tgbotapi.BotAPI)) {
	t.Helper()
	userID := int64(1)
	eventID := ""
	if kind == activityEntryBoss {
		event := worldBossLiveFixture(t, 1)
		userID, eventID = 2, event.BossID
		expires := time.Now().Add(time.Hour)
		contributionMust(t, storage.DB.Create(&model.User{TelegramID: userID, Username: "entry-user", AbsUserID: "abs2", Status: "active", ExpireAt: &expires}).Error)
	} else {
		event := secretRealmLiveFixture(t, 1)
		eventID = event.RealmID
		contributionMust(t, storage.DB.Unscoped().Where("realm_id = ?", eventID).Delete(&model.SectSecretRealmParticipant{}).Error)
	}
	msg := &tgbotapi.Message{MessageID: 42, From: &tgbotapi.User{ID: userID, FirstName: "入场道友"}, Chat: &tgbotapi.Chat{ID: userID, Type: "private"}}
	return eventID, userID, msg, func(bot *tgbotapi.BotAPI) {
		if kind == activityEntryBoss {
			HandleWorldBossCommand(bot, msg, "参加Boss")
		} else {
			HandleSectSecretRealmCommand(bot, msg, "进入宗门秘境")
		}
	}
}

func TestActivityEntryReturnsBeforeABSAndDeduplicatesPendingAndJoined(t *testing.T) {
	for _, kind := range []string{activityEntryBoss, activityEntryRealm} {
		t.Run(kind, func(t *testing.T) {
			eventID, userID, msg, submit := entryTestFixture(t, kind)
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var once sync.Once
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				once.Do(func() { close(started) })
				<-release
				_ = json.NewEncoder(w).Encode(map[string]any{"days": map[string]float64{SectDayKey(time.Now()): 7200}})
			})
			bot, replies := secretRealmQueryBot(t)
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); waitActivityEntries(t) })
			done := make(chan struct{})
			go func() { defer close(done); submit(bot) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("entry command waited for ABS")
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("entry was not sampled")
			}
			var duplicates sync.WaitGroup
			for range 10 {
				duplicates.Go(func() { submit(bot) })
			}
			duplicates.Wait()
			var requests []model.ActivityEntryRequest
			contributionMust(t, storage.DB.Where("kind = ? AND event_id = ? AND user_id = ?", kind, eventID, userID).Find(&requests).Error)
			if len(requests) != 1 || requests[0].Status != "pending" || calls.Load() != 1 {
				t.Fatalf("duplicate entry work: %+v calls=%d", requests, calls.Load())
			}
			if kind == activityEntryBoss {
				var event model.WorldBossEvent
				contributionMust(t, storage.DB.First(&event, "boss_id = ?", eventID).Error)
				if event.ParticipantCount != 1 {
					t.Fatal("pending request increased Boss participant count")
				}
			}
			status, found, err := activityEntryStatus(kind, eventID, userID)
			contributionMust(t, err)
			if !found || !strings.Contains(status, "尚未正式入场") {
				t.Fatal("pending entry was not queryable")
			}
			// A slow baseline request must not hold SQLite's writer.
			contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", userID).UpdateColumn("points", 17).Error)
			if kind == activityEntryBoss {
				handleWorldBossDetail(bot, msg)
			} else {
				handleSectSecretRealmDetail(bot, msg, nil)
			}
			if !strings.Contains(replies(), "尚未正式入场") {
				t.Fatal("detail omitted pending entry")
			}
			enteredAfter := time.Now()
			releaseOnce.Do(func() { close(release) })
			waitActivityEntries(t)
			var request model.ActivityEntryRequest
			contributionMust(t, storage.DB.First(&request, requests[0].ID).Error)
			if request.Status != "joined" || request.Attempts != 1 || request.NotifiedAt == nil {
				t.Fatalf("entry did not complete: %+v", request)
			}
			var createdAt time.Time
			if kind == activityEntryBoss {
				var p model.WorldBossParticipant
				contributionMust(t, storage.DB.First(&p, "boss_id = ? AND user_id = ?", eventID, userID).Error)
				createdAt = p.CreatedAt
				if p.DeltaHours != 0 || p.IsRewarded {
					t.Fatal("entry awarded pre-join progress")
				}
			} else {
				var p model.SectSecretRealmParticipant
				contributionMust(t, storage.DB.First(&p, "realm_id = ? AND user_id = ?", eventID, userID).Error)
				createdAt = p.CreatedAt
				if p.RawDeltaSeconds != 0 || p.IsRewarded {
					t.Fatal("entry awarded pre-join progress")
				}
			}
			if createdAt.Before(enteredAfter) {
				t.Fatal("entry was backdated to request time")
			}
			submit(bot)
			waitActivityEntries(t)
			if calls.Load() != 1 {
				t.Fatal("already-joined participant fetched another baseline")
			}
		})
	}
}

func TestActivityEntryCommitsWhileTelegramReceiptIsBlocked(t *testing.T) {
	eventID, userID, _, submit := entryTestFixture(t, activityEntryBoss)
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
	})
	started, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
			return
		}
		once.Do(func() { close(started) })
		<-release
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); waitActivityEntries(t) })
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	contributionMust(t, err)
	done := make(chan struct{})
	go func() { defer close(done); submit(bot) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("entry command waited for Telegram")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no entry receipt")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var request model.ActivityEntryRequest
		contributionMust(t, storage.DB.First(&request, "kind = ? AND event_id = ? AND user_id = ?", activityEntryBoss, eventID, userID).Error)
		if request.Status == "joined" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow Telegram blocked entry commit")
		}
		time.Sleep(time.Millisecond)
	}
	contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", userID).UpdateColumn("points", 19).Error)
}

func TestActivityEntryResultSurvivesDeletedGroupCommand(t *testing.T) {
	eventID, userID, _, _ := entryTestFixture(t, activityEntryBoss)
	request := model.ActivityEntryRequest{Kind: activityEntryBoss, EventID: eventID, UserID: userID, AbsUserID: "abs2", Status: "joined", Generation: 1,
		ChatID: -100, MessageID: 42, ResultText: "✅ 道友：`入场道友` 已加入世界Boss讨伐。"}
	contributionMust(t, storage.DB.Create(&request).Error)
	var delivered atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test"}}`)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("reply_to_message_id") != "42" || r.Form.Get("allow_sending_without_reply") != "true" {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to be replied not found"}`)
			return
		}
		delivered.Store(true)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":123,"chat":{"id":1,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	contributionMust(t, err)
	enqueueActivityEntryResult(bot, request)
	waitActivityEntries(t)
	contributionMust(t, storage.DB.First(&request, request.ID).Error)
	if !delivered.Load() || request.NotifiedAt == nil || request.NotificationAttempts != 1 {
		t.Fatalf("deleted command stranded the entry result: %+v", request)
	}
}

func TestRealmEntryDuringRefreshKeepsCommittedParticipantCount(t *testing.T) {
	event := secretRealmLiveFixture(t, 2)
	contributionMust(t, storage.DB.Unscoped().Where("realm_id = ? AND user_id = ?", event.RealmID, 2).Delete(&model.SectSecretRealmParticipant{}).Error)
	contributionMust(t, storage.DB.Model(&event).Update("participant_count", 1).Error)
	started, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/abs1/") {
			once.Do(func() { close(started) })
			<-release
		}
		fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
	})
	bot, _ := secretRealmQueryBot(t)
	done := make(chan error, 1)
	go func() { _, err := refreshSectSecretRealmLiveProgress(event); done <- err }()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		waitActivityEntries(t)
		select {
		case err := <-done:
			contributionMust(t, err)
		case <-time.After(5 * time.Second):
			t.Error("realm refresh did not finish")
		}
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("realm refresh did not start")
	}
	HandleSectSecretRealmCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: 2}, Chat: &tgbotapi.Chat{ID: 2, Type: "private"}}, "进入宗门秘境")
	waitActivityEntries(t)
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.ParticipantCount != 2 {
		t.Fatalf("entry did not publish committed count: %d", event.ParticipantCount)
	}
	releaseOnce.Do(func() { close(release) })
	// Leave the result in the channel for cleanup, including on failure.
	select {
	case err := <-done:
		done <- err
		contributionMust(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("realm refresh did not finish")
	}
	contributionMust(t, storage.DB.First(&event, event.ID).Error)
	if event.ParticipantCount != 2 {
		t.Fatalf("refresh overwrote new entry count: %d", event.ParticipantCount)
	}
}

func TestActivityEntryRechecksEligibilityAfterSampling(t *testing.T) {
	for _, change := range []string{"boss/closed", "boss/binding", "boss/suspended", "realm/closed", "realm/binding", "realm/member", "realm/suspended"} {
		t.Run(change, func(t *testing.T) {
			kind := strings.Split(change, "/")[0]
			eventID, userID, _, submit := entryTestFixture(t, kind)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(started) })
				<-release
				fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
			})
			bot, _ := secretRealmQueryBot(t)
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); waitActivityEntries(t) })
			submit(bot)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("no baseline request")
			}
			switch {
			case strings.HasSuffix(change, "suspended"):
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", userID).Update("is_suspended", true).Error)
			case strings.HasSuffix(change, "binding"):
				contributionMust(t, storage.DB.Model(&model.User{}).Where("telegram_id = ?", userID).Update("abs_user_id", "replacement").Error)
			case strings.HasSuffix(change, "member"):
				contributionMust(t, storage.DB.Where("user_id = ?", userID).Delete(&model.SectMember{}).Error)
			case kind == activityEntryBoss:
				contributionMust(t, storage.DB.Model(&model.WorldBossEvent{}).Where("boss_id = ?", eventID).Update("is_killed", true).Error)
			default:
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmEvent{}).Where("realm_id = ?", eventID).Update("end_at", time.Now().Add(-time.Second)).Error)
			}
			releaseOnce.Do(func() { close(release) })
			waitActivityEntries(t)
			var request model.ActivityEntryRequest
			contributionMust(t, storage.DB.First(&request, "kind = ? AND event_id = ? AND user_id = ?", kind, eventID, userID).Error)
			if request.Status != "failed" {
				t.Fatalf("invalid entry was not rejected: %+v", request)
			}
			var count int64
			if kind == activityEntryBoss {
				contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", eventID, userID).Count(&count).Error)
			} else {
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = ?", eventID, userID).Count(&count).Error)
			}
			if count != 0 {
				t.Fatal("invalid participant was created")
			}
		})
	}
}

func TestActivityEntryRecoveryRetryAndExpiry(t *testing.T) {
	for _, kind := range []string{activityEntryBoss, activityEntryRealm} {
		t.Run(kind, func(t *testing.T) {
			eventID, userID, _, submit := entryTestFixture(t, kind)
			var calls atomic.Int32
			var healthy atomic.Bool
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !healthy.Load() {
					w.WriteHeader(503)
					return
				}
				fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
			})
			bot, _ := secretRealmQueryBot(t)
			t.Cleanup(func() { waitActivityEntries(t) })
			submit(bot)
			waitActivityEntries(t)
			var request model.ActivityEntryRequest
			contributionMust(t, storage.DB.First(&request, "kind = ? AND event_id = ? AND user_id = ?", kind, eventID, userID).Error)
			if request.Status != "pending" || request.Attempts != 1 || request.NextAttemptAt == nil {
				t.Fatalf("transient failure was not durable: %+v", request)
			}
			dispatchActivityEntries(bot, time.Now())
			waitActivityEntries(t)
			if calls.Load() != 1 {
				t.Fatal("retry backoff ignored")
			}
			healthy.Store(true)
			contributionMust(t, storage.DB.Model(&request).Update("next_attempt_at", nil).Error)
			// Recovery is driven by persisted rows, without another user command.
			dispatchActivityEntries(bot, time.Now())
			waitActivityEntries(t)
			contributionMust(t, storage.DB.First(&request, request.ID).Error)
			if request.Status != "joined" || request.Attempts != 2 || calls.Load() != 2 {
				t.Fatalf("pending request did not recover: %+v calls=%d", request, calls.Load())
			}
		})
	}
	t.Run("expired after restart", func(t *testing.T) {
		eventID, userID, _, _ := entryTestFixture(t, activityEntryBoss)
		request := model.ActivityEntryRequest{Kind: activityEntryBoss, EventID: eventID, UserID: userID, AbsUserID: "abs2", Status: "pending", DeadlineAt: time.Now().Add(-time.Second)}
		contributionMust(t, storage.DB.Create(&request).Error)
		var calls atomic.Int32
		bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
		bot, _ := secretRealmQueryBot(t)
		t.Cleanup(func() { waitActivityEntries(t) })
		dispatchActivityEntries(bot, time.Now())
		waitActivityEntries(t)
		contributionMust(t, storage.DB.First(&request, request.ID).Error)
		if request.Status != "failed" || calls.Load() != 0 {
			t.Fatal("expired pending request fetched a new baseline")
		}
	})
}

func TestActivityEntryCrossMidnightSampleDoesNotCommitZeroBaseline(t *testing.T) {
	for _, kind := range []string{activityEntryBoss, activityEntryRealm} {
		t.Run(kind, func(t *testing.T) {
			eventID, userID, _, _ := entryTestFixture(t, kind)
			var user model.User
			contributionMust(t, storage.DB.First(&user, "telegram_id = ?", userID).Error)
			now := time.Now()
			request := model.ActivityEntryRequest{Kind: kind, EventID: eventID, UserID: userID, AbsUserID: user.AbsUserID, Status: "pending", Generation: 1, DeadlineAt: now.Add(time.Minute)}
			if kind == activityEntryRealm {
				var member model.SectMember
				contributionMust(t, storage.DB.First(&member, "user_id = ?", userID).Error)
				request.SectID, request.MemberID = member.SectID, member.ID
			}
			contributionMust(t, storage.DB.Create(&request).Error)
			local := now.In(storage.DailyOperationsLocation)
			requestedAt := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, storage.DailyOperationsLocation).Add(-time.Second)
			// A response captured yesterday can omit today's entry baseline.
			baseline := newActivityListeningSnapshot(map[string]float64{SectDayKey(requestedAt): 3600}, now)
			if _, err := commitActivityEntry(request, baseline, 0, requestedAt); err == nil {
				t.Fatal("cross-midnight response confirmed entry with an unknown baseline")
			}
			var saved model.ActivityEntryRequest
			contributionMust(t, storage.DB.First(&saved, request.ID).Error)
			var count int64
			if kind == activityEntryBoss {
				contributionMust(t, storage.DB.Model(&model.WorldBossParticipant{}).Where("boss_id = ? AND user_id = ?", eventID, userID).Count(&count).Error)
			} else {
				contributionMust(t, storage.DB.Model(&model.SectSecretRealmParticipant{}).Where("realm_id = ? AND user_id = ?", eventID, userID).Count(&count).Error)
			}
			if saved.Status != "pending" || count != 0 {
				t.Fatal("cross-midnight attempt partially committed entry")
			}
			// A new same-day sample can finish the same durable request.
			baseline = newActivityListeningSnapshot(map[string]float64{SectDayKey(now): 600}, now)
			_, err := commitActivityEntry(request, baseline, 0, now)
			contributionMust(t, err)
			contributionMust(t, storage.DB.First(&saved, request.ID).Error)
			if saved.Status != "joined" {
				t.Fatal("fresh retry could not complete entry")
			}
		})
	}
}

func TestBossAndRealmEntryBurstsShareFourSlotsAndThreePerEvent(t *testing.T) {
	realm := secretRealmLiveFixture(t, 6)
	contributionMust(t, storage.DB.Unscoped().Where("realm_id = ?", realm.RealmID).Delete(&model.SectSecretRealmParticipant{}).Error)
	contributionMust(t, storage.DB.AutoMigrate(&model.WorldBossEvent{}, &model.WorldBossParticipant{}))
	boss := model.WorldBossEvent{BossID: "entry-burst", Status: "active", StartAt: time.Now().Add(-time.Minute), EndAt: time.Now().Add(time.Hour)}
	contributionMust(t, storage.DB.Create(&boss).Error)
	expires := time.Now().Add(time.Hour)
	for id := int64(101); id <= 106; id++ {
		contributionMust(t, storage.DB.Create(&model.User{TelegramID: id, Username: fmt.Sprint("boss", id), AbsUserID: fmt.Sprint("boss", id), Status: "active", ExpireAt: &expires}).Error)
	}
	var active, maximum, bossActive, realmActive, bossMaximum, realmMaximum, calls atomic.Int32
	started, release := make(chan struct{}, 12), make(chan struct{})
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		a, m := &realmActive, &realmMaximum
		if strings.Contains(r.URL.Path, "/boss") {
			a, m = &bossActive, &bossMaximum
		}
		n = a.Add(1)
		defer a.Add(-1)
		for old := m.Load(); n > old && !m.CompareAndSwap(old, n); old = m.Load() {
		}
		started <- struct{}{}
		<-release
		fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
	})
	bot, _ := secretRealmQueryBot(t)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); waitActivityEntries(t) })
	for i := int64(1); i <= 6; i++ {
		HandleWorldBossCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: i + 100}, Chat: &tgbotapi.Chat{ID: i + 100, Type: "private"}}, "参加Boss")
		HandleSectSecretRealmCommand(bot, &tgbotapi.Message{From: &tgbotapi.User{ID: i}, Chat: &tgbotapi.Chat{ID: i, Type: "private"}}, "进入宗门秘境")
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("both events did not share request capacity")
		}
	}
	once.Do(func() { close(release) })
	waitActivityEntries(t)
	var joined int64
	contributionMust(t, storage.DB.Model(&model.ActivityEntryRequest{}).Where("status = ?", "joined").Count(&joined).Error)
	if joined != 12 || calls.Load() != 12 || maximum.Load() > 4 || bossMaximum.Load() > 3 || realmMaximum.Load() > 3 {
		t.Fatalf("joined=%d requests=%d total=%d boss=%d realm=%d", joined, calls.Load(), maximum.Load(), bossMaximum.Load(), realmMaximum.Load())
	}
}

func TestActivityEntryLimitsRetriesAndAllowsFreshSubmission(t *testing.T) {
	_, userID, _, submit := entryTestFixture(t, activityEntryBoss)
	var calls atomic.Int32
	var healthy atomic.Bool
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"days":{"%s":7200}}`, SectDayKey(time.Now()))
	})
	bot, _ := secretRealmQueryBot(t)
	t.Cleanup(func() { waitActivityEntries(t) })
	submit(bot)
	waitActivityEntries(t)
	var request model.ActivityEntryRequest
	contributionMust(t, storage.DB.First(&request, "user_id = ?", userID).Error)
	for range 2 {
		contributionMust(t, storage.DB.Model(&request).Update("next_attempt_at", nil).Error)
		dispatchActivityEntries(bot, time.Now())
		waitActivityEntries(t)
	}
	contributionMust(t, storage.DB.First(&request, request.ID).Error)
	if request.Status != "failed" || request.Attempts != 3 || calls.Load() != 3 {
		t.Fatalf("retries were not bounded: %+v calls=%d", request, calls.Load())
	}
	healthy.Store(true)
	submit(bot)
	waitActivityEntries(t)
	contributionMust(t, storage.DB.First(&request, request.ID).Error)
	if request.Status != "joined" || request.Generation != 2 || request.Attempts != 1 || calls.Load() != 4 {
		t.Fatalf("fresh submission did not replace the failed generation: %+v calls=%d", request, calls.Load())
	}
}

func TestActivityEntryPendingCapacityIsBounded(t *testing.T) {
	for _, scope := range []string{"event", "global"} {
		t.Run(scope, func(t *testing.T) {
			eventID, userID, _, submit := entryTestFixture(t, activityEntryBoss)
			count := activityEntryEventLimit
			if scope == "global" {
				count = activityEntryPendingLimit
			}
			var pending []model.ActivityEntryRequest
			later := time.Now().Add(time.Hour)
			for i := range count {
				id := eventID
				if scope == "global" {
					id = fmt.Sprint("other-event-", i)
				}
				pending = append(pending, model.ActivityEntryRequest{Kind: activityEntryBoss, EventID: id, UserID: int64(1000 + i), Status: "pending", DeadlineAt: later, NextAttemptAt: &later})
			}
			contributionMust(t, storage.DB.CreateInBatches(&pending, 50).Error)
			var calls atomic.Int32
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
			bot, replies := secretRealmQueryBot(t)
			t.Cleanup(func() { waitActivityEntries(t) })
			submit(bot)
			waitActivityEntries(t)
			var own int64
			contributionMust(t, storage.DB.Model(&model.ActivityEntryRequest{}).Where("user_id = ?", userID).Count(&own).Error)
			if own != 0 || calls.Load() != 0 || !strings.Contains(replies(), "报名队列繁忙") {
				t.Fatalf("full queue accepted work: own=%d calls=%d replies=%s", own, calls.Load(), replies())
			}
		})
	}
}

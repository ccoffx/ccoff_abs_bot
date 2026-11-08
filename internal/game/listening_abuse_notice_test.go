package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ccoff-abs-bot/internal/config"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
)

func TestListeningAbuseSlowScanStartsNoticeThrottleWhenQueued(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	started := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
	current := started
	scanDuration := 65 * time.Minute
	const callback = "test:slow_failed_abuse_scan"
	if err := storage.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			current = current.Add(scanDuration)
			tx.AddError(errors.New("scan unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.DB.Callback().Query().Remove(callback) })
	queued := 0
	enqueue := func(int64, string) bool { queued++; return true }
	clock := func() time.Time { return current }
	runListeningAbuseMonitorIfNeeded(nil, clock, enqueue)
	raw, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey)
	if err != nil {
		t.Fatal(err)
	}
	var state listeningAbuseFailureNotices
	if err := json.Unmarshal([]byte(raw), &state); err != nil || len(state.Notices) != 1 || !state.Notices[0].Recipients[1].Equal(current) {
		t.Fatalf("notice throttle starts before a slow scan finishes: state=%s err=%v", raw, err)
	}
	finished := current
	scanDuration = 0
	current = finished.Add(5 * time.Minute)
	runListeningAbuseMonitorIfNeeded(nil, clock, enqueue)
	if queued != 1 {
		t.Fatalf("slow scan caused duplicate notices less than an hour apart: %d", queued)
	}
	current = finished.Add(time.Hour)
	runListeningAbuseMonitorIfNeeded(nil, clock, enqueue)
	if queued != 2 {
		t.Fatalf("notice did not resume an hour after enqueue: %d", queued)
	}
}

func TestListeningAbuseFailureNoticePersistsThrottleAndResetsOnRecovery(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	sends := make(chan string, 16)
	enqueue := func(_ int64, text string) bool { sends <- text; return true }
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
	failure := errors.New("LISTENING_ABUSE_SCAN_PENDING: 1 users require retry")
	readNotice := func() string {
		t.Helper()
		raw, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	assertSent := func(day string) {
		t.Helper()
		select {
		case text := <-sends:
			if !strings.Contains(text, day) || !strings.Contains(text, "每小时最多提醒一次") {
				t.Fatalf("incorrect notice: %s", text)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("notice was not sent")
		}
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now, func(int64, string) bool { return false })
	if raw := readNotice(); raw != "" {
		t.Fatal("failed enqueue consumed the alert interval")
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now, enqueue)
	assertSent("2026-11-08")
	first := readNotice()
	var stored listeningAbuseFailureNotices
	if err := json.Unmarshal([]byte(first), &stored); err != nil || len(stored.Notices) != 1 || !stored.Notices[0].Recipients[1].Equal(now) {
		t.Fatalf("missing durable throttle: %q %v", first, err)
	}
	// There is no process-local throttle: rereading this row after a restart
	// must give the same decision, without pushing the hourly deadline forward.
	for _, age := range []time.Duration{5 * time.Minute, 55 * time.Minute, time.Hour - time.Nanosecond} {
		notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(age), enqueue)
		if readNotice() != first {
			t.Fatal("suppressed retry changed the last-notified timestamp")
		}
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(time.Hour), enqueue)
	assertSent("2026-11-08")
	notifyListeningAbuseScanFailure("2026-11-07", failure, now.Add(65*time.Minute), enqueue)
	assertSent("2026-11-07")
	notifyListeningAbuseScanFailure("2026-11-07", errors.New("database unavailable"), now.Add(70*time.Minute), enqueue)
	assertSent("2026-11-07")
	// A completed scan ends the incident; a later failure must alert at once.
	RunListeningAbuseMonitorIfNeeded(nil, now.Add(75*time.Minute))
	if readNotice() != "" {
		t.Fatal("successful scan did not reset the incident")
	}
	notifyListeningAbuseScanFailure("2026-11-07", errors.New("database unavailable"), now.Add(80*time.Minute), enqueue)
	assertSent("2026-11-07")
	select {
	case text := <-sends:
		t.Fatalf("unexpected duplicate notice: %s", text)
	default:
	}
}

func TestListeningAbuseFailureNoticeRetriesOnlyUnqueuedRecipients(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true, 2: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
	failure := errors.New("LISTENING_ABUSE_SCAN_PENDING: 1 users require retry")
	queued := map[int64]int{}
	allowSecond := false
	enqueue := func(adminID int64, _ string) bool {
		if adminID == 2 && !allowSecond {
			return false
		}
		queued[adminID]++
		return true
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now, enqueue)
	allowSecond = true
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(5*time.Minute), enqueue)
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(55*time.Minute), enqueue)
	if queued[1] != 1 || queued[2] != 1 {
		t.Fatalf("partial enqueue retried successful recipients: %v", queued)
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(time.Hour), enqueue)
	if queued[1] != 2 || queued[2] != 1 {
		t.Fatalf("recipients must have independent hourly deadlines: %v", queued)
	}
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(65*time.Minute), enqueue)
	if queued[1] != 2 || queued[2] != 2 {
		t.Fatalf("failed recipient did not get its next hourly notice: %v", queued)
	}
	config.AppConfig.AdminIDs[3] = true
	notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(70*time.Minute), enqueue)
	if queued[1] != 2 || queued[2] != 2 || queued[3] != 1 {
		t.Fatalf("new administrator was not notified independently: %v", queued)
	}
}

func TestListeningAbuseFailureNoticeKeepsLegacyThrottle(t *testing.T) {
	for _, legacy := range []string{
		`{"day":"2026-11-08","error":"pending","at":"2026-11-09T01:00:00+08:00"}`,
		`{"day":"2026-11-08","error":"pending","recipients":{"1":"2026-11-09T01:00:00+08:00","2":"2026-11-09T01:00:00+08:00"}}`,
	} {
		t.Run(legacy, func(t *testing.T) {
			setupListeningAbuseScheduleDB(t)
			previousConfig := config.AppConfig
			config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true, 2: true}}
			t.Cleanup(func() { config.AppConfig = previousConfig })
			now := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
			failure := errors.New("pending")
			if err := storage.SetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey, legacy); err != nil {
				t.Fatal(err)
			}
			queued := 0
			enqueue := func(int64, string) bool { queued++; return true }
			notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(5*time.Minute), enqueue)
			if queued != 0 {
				t.Fatal("legacy receipt was ignored")
			}
			notifyListeningAbuseScanFailure("2026-11-08", errors.New("another failure"), now.Add(10*time.Minute), enqueue)
			notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(15*time.Minute), enqueue)
			if queued != 2 {
				t.Fatal("legacy receipt was lost when another error was recorded")
			}
			notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(time.Hour), enqueue)
			if queued != 4 {
				t.Fatalf("legacy receipt did not expire: queued=%d", queued)
			}
		})
	}
}

func TestListeningAbuseFailureNoticeThrottlesAlternatingErrors(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
	sends := 0
	enqueue := func(int64, string) bool { sends++; return true }
	onePending := errors.New("LISTENING_ABUSE_SCAN_PENDING: 1 users require retry")
	twoPending := errors.New("LISTENING_ABUSE_SCAN_PENDING: 2 users require retry")
	for i, failure := range []error{onePending, twoPending, onePending, twoPending} {
		notifyListeningAbuseScanFailure("2026-11-08", failure, now.Add(time.Duration(i)*5*time.Minute), enqueue)
	}
	if sends != 2 {
		t.Fatalf("alternating errors bypassed the hourly throttle: sent=%d, want 2", sends)
	}
	// Each error keeps its own deadline, including after the other error was sent.
	notifyListeningAbuseScanFailure("2026-11-08", onePending, now.Add(time.Hour), enqueue)
	notifyListeningAbuseScanFailure("2026-11-08", twoPending, now.Add(time.Hour), enqueue)
	if sends != 3 {
		t.Fatalf("independent hourly deadlines were lost: sent=%d, want 3", sends)
	}
	notifyListeningAbuseScanFailure("2026-11-08", twoPending, now.Add(65*time.Minute), enqueue)
	if sends != 4 {
		t.Fatalf("second error was not resent after its deadline: sent=%d", sends)
	}
}

func TestListeningAbuseFailureNoticePrunesExpiredErrors(t *testing.T) {
	setupListeningAbuseScheduleDB(t)
	previousConfig := config.AppConfig
	config.AppConfig = &config.Config{AdminIDs: map[int64]bool{1: true}}
	t.Cleanup(func() { config.AppConfig = previousConfig })
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, storage.DailyOperationsLocation)
	for i := 0; i < 15; i++ {
		notifyListeningAbuseScanFailure("2026-11-08", fmt.Errorf("pending %d", i), now.Add(time.Duration(i)*5*time.Minute), func(int64, string) bool { return true })
	}
	raw, err := storage.GetSystemConfigStringChecked(storage.ListeningAbuseFailureNoticeKey)
	if err != nil {
		t.Fatal(err)
	}
	var state listeningAbuseFailureNotices
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Notices) != 12 {
		t.Fatalf("stored throttle retained expired errors: entries=%d, want 12", len(state.Notices))
	}
	for _, notice := range state.Notices {
		if !notice.Recipients[1].After(now.Add(10 * time.Minute)) {
			t.Fatalf("expired error was retained: %+v", notice)
		}
	}
}

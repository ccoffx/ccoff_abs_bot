package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

// Scheduling and bounded requests may finish slightly after the cutoff.
// A restart or a later retry must never include arbitrary post-event listening.
const (
	activityFinalSnapshotGrace = 2 * time.Minute
	activitySchedulerInterval  = 15 * time.Second
	activityFinalRetryInterval = 15 * time.Second
)

var ErrActivitySnapshotPending = errors.New("ACTIVITY_FINAL_SNAPSHOT_PENDING")

// Capture the whole activity near one cutoff, rather than waiting serially for
// every account's network timeout. The per-account input is durable before pay.
func captureActivitySnapshotBatch(count int, work func(int) error) []error {
	errors := make([]error, count)
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(listeningSyncWorkers-1, count) {
		workers.Go(func() {
			for i := range jobs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							errors[i] = fmt.Errorf("activity snapshot panic: %s", textfmt.FormatPlainValue(r))
						}
					}()
					errors[i] = work(i)
				}()
			}
		})
	}
	for i := range count {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return errors
}

func fetchActivityListeningStats(absID string) (officialListeningStats, time.Time, time.Time, error) {
	startedAt := time.Now()
	if abs.DefaultClient == nil || absID == "" {
		return officialListeningStats{}, time.Time{}, time.Time{}, fmt.Errorf("ACTIVITY_LISTENING_NOT_READY")
	}
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(absID), nil)
	if err != nil {
		return officialListeningStats{}, time.Time{}, time.Time{}, err
	}
	if code != 200 {
		return officialListeningStats{}, time.Time{}, time.Time{}, fmt.Errorf("ABS_STATUS_%d", code)
	}
	stats, err := parseOfficialListeningStats(body)
	return stats, startedAt, time.Now(), err
}

type activityDailyListeningInput struct {
	Seconds  float64 `json:"s"`
	Archived bool    `json:"a"`
}

func newActivityListeningSnapshot(days map[string]float64, at time.Time) model.ActivityListeningSnapshot {
	day := SectDayKey(at)
	return model.ActivityListeningSnapshot{BaselineDay: day, BaselineSeconds: dailyListeningCreditSeconds(day, days[day], at), FetchedAt: &at}
}

// Activities retain only their own date window. Daily elapsed caps, retained
// decreases, missing closing dates and immutable archived dates follow the same
// rules as ordinary daily credit, without settling cultivation or rewards here.
func advanceActivityListeningDays(days map[string]float64, requestedAt, capAt, start time.Time, savedSeconds float64, snapshot *model.ActivityListeningSnapshot) (float64, error) {
	rebased := snapshot.BaselineDay == ""
	if rebased {
		// An old cumulative baseline cannot be converted into a daily baseline.
		// Preserve published progress and measure only future growth from now on.
		baseline := newActivityListeningSnapshot(days, capAt)
		snapshot.BaselineDay, snapshot.BaselineSeconds = baseline.BaselineDay, baseline.BaselineSeconds
		snapshot.CarriedSeconds = savedSeconds
	}
	if !validDailyListeningDayKey(snapshot.BaselineDay) || snapshot.BaselineDay > SectDayKey(capAt) {
		return 0, fmt.Errorf("ACTIVITY_DAILY_BASELINE_INVALID")
	}
	// Earlier versions stored an uncapped day baseline. Adopt the daily clock
	// limit at entry without importing any account-wide playback history.
	if !rebased && snapshot.DailyInputJSON == "" {
		snapshot.BaselineSeconds = dailyListeningCreditSeconds(snapshot.BaselineDay, snapshot.BaselineSeconds, start)
	}
	inputs := make(map[string]activityDailyListeningInput)
	if snapshot.DailyInputJSON != "" {
		if err := json.Unmarshal([]byte(snapshot.DailyInputJSON), &inputs); err != nil || inputs == nil {
			return 0, fmt.Errorf("ACTIVITY_DAILY_INPUT_INVALID")
		}
	}
	if _, ok := inputs[snapshot.BaselineDay]; !ok {
		inputs[snapshot.BaselineDay] = activityDailyListeningInput{Seconds: snapshot.BaselineSeconds}
	}
	for day, raw := range days {
		if day < snapshot.BaselineDay || day > SectDayKey(capAt) {
			continue
		}
		old := inputs[day]
		if old.Archived {
			continue
		}
		inputs[day] = activityDailyListeningInput{
			Seconds: dailyListeningCreditSeconds(day, math.Max(raw, old.Seconds), capAt), Archived: day < SectDayKey(requestedAt),
		}
	}
	seconds := 0.0
	for day, input := range inputs {
		if !validDailyListeningDayKey(day) || input.Seconds < 0 || math.IsNaN(input.Seconds) || math.IsInf(input.Seconds, 0) || day < snapshot.BaselineDay || day > SectDayKey(capAt) {
			return 0, fmt.Errorf("ACTIVITY_DAILY_INPUT_INVALID")
		}
		seconds += dailyListeningCreditSeconds(day, input.Seconds, capAt)
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return 0, err
	}
	snapshot.DailyInputJSON = string(encoded)
	return math.Max(savedSeconds, math.Max(0, snapshot.CarriedSeconds+seconds-snapshot.BaselineSeconds)), nil
}

func validateActivityFinalDailyInput(days map[string]float64, snapshot model.ActivityListeningSnapshot) error {
	var inputs map[string]activityDailyListeningInput
	if err := json.Unmarshal([]byte(snapshot.DailyInputJSON), &inputs); err != nil {
		return err
	}
	for day, input := range inputs {
		// Missing dates are unknown, not zero. Previously observed, unarchived
		// dates still need a closing sample; archived dates are already frozen.
		// ABS may omit days with no playback, so zero-only dates need no retry.
		if _, present := days[day]; !present && !input.Archived && input.Seconds > 0 {
			return fmt.Errorf("ACTIVITY_FINAL_DAY_MISSING: %s", day)
		}
	}
	return nil
}

// Persist each participant's final snapshot before payout. Retries can fail at
// any subsequent database step without fetching a different reward input.
func captureActivityListeningSnapshot(table, eventTable, eventColumn, eventID string, id uint, absID string,
	start, cutoff, now time.Time, savedSeconds float64,
	snapshot *model.ActivityListeningSnapshot, final bool) (float64, error) {
	if snapshot.Finalized {
		return snapshot.Seconds, nil
	}
	if !final && !time.Now().Before(cutoff) {
		return 0, fmt.Errorf("ACTIVITY_WINDOW_CLOSED")
	}
	seconds := math.Max(0, math.Max(savedSeconds, snapshot.Seconds))
	limit := math.Max(0, cutoff.Sub(start).Seconds())
	if !final && now.Before(cutoff) {
		limit = math.Max(0, now.Sub(start).Seconds())
	}
	deadline := cutoff.Add(activityFinalSnapshotGrace)
	if final && now.Before(deadline) && snapshot.FinalNextRetryAt != nil && now.Before(*snapshot.FinalNextRetryAt) {
		return 0, ErrActivitySnapshotPending
	}
	next := *snapshot
	finalized := false
	group := eventTable + ":" + eventID
	priority, wait := listeningRequestLive, min(listeningQueryWait, time.Until(cutoff))
	if final {
		priority, wait = listeningRequestFinal, min(5*time.Second, time.Until(deadline))
	}
	shouldFetch := !final || now.Before(deadline)
	failure := ""
	finishedAt := now
	if shouldFetch {
		waitingAt := time.Now()
		acquired := listeningSyncSlots.acquire(priority, group, wait)
		waitDuration := time.Since(waitingAt)
		if final {
			next.FinalAttempts++
			next.FinalAttemptedAt = &waitingAt
		}
		requestDuration := time.Duration(0)
		if !acquired {
			failure = "capacity_timeout"
		} else {
			var stats officialListeningStats
			var requestedAt, completedAt time.Time
			var fetchErr error
			func() {
				defer listeningSyncSlots.release(group)
				// Capacity may have become available after the closing window.
				if (final && !time.Now().Before(deadline)) || (!final && !time.Now().Before(cutoff)) {
					failure = "deadline_exceeded"
					return
				}
				requestStarted := time.Now()
				stats, requestedAt, completedAt, fetchErr = fetchActivityListeningStats(absID)
				requestDuration = time.Since(requestStarted)
			}()
			if failure == "" {
				if fetchErr != nil {
					failure = "abs_request_failed"
					log.Printf("活动听书读取失败: event=%s participant=%d err=%s", textfmt.FormatPlainValue(group), id, textfmt.FormatPlainError(fetchErr))
				} else if final && completedAt.After(deadline) {
					failure = "deadline_exceeded"
				} else {
					capAt := completedAt
					if cutoff.Before(capAt) {
						capAt = cutoff
					}
					var err error
					projected := next
					seconds, err = advanceActivityListeningDays(stats.Days, requestedAt, capAt, start, seconds, &projected)
					if err == nil && final {
						err = validateActivityFinalDailyInput(stats.Days, projected)
					}
					if err != nil {
						failure = "invalid_input"
						log.Printf("活动听书校验失败: event=%s participant=%d err=%s", textfmt.FormatPlainValue(group), id, textfmt.FormatPlainError(err))
						// Never replace saved valid progress with an unsuccessful projection.
						seconds = math.Max(0, math.Max(savedSeconds, snapshot.Seconds))
					} else {
						next = projected
						next.FetchedAt = &completedAt
						finalized = final
					}
				}
			}
		}
		finishedAt = time.Now()
		result := failure
		if result == "" {
			result = "success"
		}
		log.Printf("活动听书同步: event=%s participant=%d final=%t wait_ms=%d request_ms=%d result=%s attempts=%d",
			textfmt.FormatPlainValue(group), id, final, waitDuration.Milliseconds(), requestDuration.Milliseconds(),
			textfmt.FormatPlainValue(result), next.FinalAttempts)
		if !final && failure != "" {
			return 0, fmt.Errorf("ACTIVITY_REFRESH_FAILED: %s", failure)
		}
	}
	if final {
		if finalized {
			next.FinalLastError, next.FinalFallbackReason = "", ""
			next.FinalNextRetryAt = nil
		} else if now.Before(deadline) && finishedAt.Before(deadline) {
			next.FinalLastError = failure
			retryAt := minTime(finishedAt.Add(activityFinalRetryInterval), deadline)
			next.FinalNextRetryAt = &retryAt
		} else {
			finalized = true
			if failure != "" {
				next.FinalLastError = failure
			}
			next.FinalFallbackReason = next.FinalLastError
			if next.FinalFallbackReason == "" {
				next.FinalFallbackReason = "deadline_exceeded"
			}
			next.FinalNextRetryAt = nil
			log.Printf("活动最终同步窗口耗尽，冻结已保存进度: event=%s participant=%d seconds=%.0f reason=%s attempts=%d",
				textfmt.FormatPlainValue(group), id, seconds, textfmt.FormatPlainValue(next.FinalFallbackReason), next.FinalAttempts)
		}
	}
	seconds = math.Min(seconds, limit)
	status := "active"
	if final {
		status = "settling"
	}
	res := storage.DB.Table(table).Where("id = ? AND abs_user_id = ? AND listening_snapshot_finalized = ?", id, absID, false).
		Where(fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE %s = ? AND status = ?)", eventTable, eventColumn), eventID, status).
		Updates(map[string]any{"listening_snapshot_seconds": seconds, "listening_snapshot_fetched_at": next.FetchedAt, "listening_snapshot_finalized": finalized,
			"listening_snapshot_baseline_day": next.BaselineDay, "listening_snapshot_baseline_seconds": next.BaselineSeconds,
			"listening_snapshot_carried_seconds": next.CarriedSeconds, "listening_snapshot_daily_input_json": next.DailyInputJSON,
			"listening_snapshot_final_attempts": next.FinalAttempts, "listening_snapshot_final_attempted_at": next.FinalAttemptedAt,
			"listening_snapshot_final_next_retry_at": next.FinalNextRetryAt, "listening_snapshot_final_last_error": next.FinalLastError,
			"listening_snapshot_final_fallback_reason": next.FinalFallbackReason})
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected != 1 {
		return 0, fmt.Errorf("ACTIVITY_SNAPSHOT_STATE_CHANGED")
	}
	next.Seconds, next.Finalized = seconds, finalized
	*snapshot = next
	if final && !finalized {
		return 0, ErrActivitySnapshotPending
	}
	return seconds, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func activityListeningSnapshotNotice(snapshot model.ActivityListeningSnapshot) string {
	notice := ""
	if snapshot.FetchedAt != nil {
		notice = "\n听书采样时间：`" + snapshot.FetchedAt.In(storage.DailyOperationsLocation).Format("01-02 15:04:05") + "`（北京时间）"
	}
	if snapshot.FinalFallbackReason != "" {
		reason := "最终同步时限已到"
		switch snapshot.FinalFallbackReason {
		case "capacity_timeout":
			reason = "同步排队超时"
		case "abs_request_failed":
			reason = "ABS 读取失败"
		case "invalid_input":
			reason = "统计数据校验失败"
		}
		notice += "\n⚠️ 最终同步未成功，本次按最近保存的进度结算（" + reason + "）。"
	} else if !snapshot.Finalized && snapshot.FinalNextRetryAt != nil {
		notice += "\n⏳ 最终同步尚未成功，正在等待后台重试。"
	}
	if snapshot.FinalAttempts > 0 {
		notice += fmt.Sprintf("\n最终同步尝试：`%d` 次", snapshot.FinalAttempts)
	}
	return notice
}

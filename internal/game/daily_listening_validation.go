package game

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

const dailyListeningValidationVersion = 2

type dailyListeningValidationDetail struct {
	Version               int            `json:"version"`
	OfficialSeconds       float64        `json:"official_seconds"`
	AdjustedSeconds       float64        `json:"adjusted_seconds"`
	CreditedSeconds       float64        `json:"credited_seconds"`
	OverlapSeconds        float64        `json:"overlap_seconds"`
	ElapsedLimitSeconds   float64        `json:"elapsed_limit_seconds"`
	EstimatedAllocation   bool           `json:"estimated_allocation"`
	ObservedSourceSeconds *float64       `json:"observed_source_seconds,omitempty"`
	Anomalies             map[string]int `json:"anomalies,omitempty"`
}

type dailyListeningValidation struct {
	Official map[string]float64
	Seconds  map[string]float64
	Details  map[string]dailyListeningValidationDetail
}

// Unobserved history retains its official budget. Known sessions can only
// replace that budget, never add playback on top of listening-stats.days.
// This also works while the resumable historical scanner is still running.
func validateDailyListeningCredit(official, adjusted map[string]float64, sessions []absListeningSession, now time.Time) *dailyListeningValidation {
	result := &dailyListeningValidation{Official: official, Seconds: make(map[string]float64), Details: make(map[string]dailyListeningValidationDetail)}
	for day, seconds := range adjusted {
		if validDailyListeningDayKey(day) && day <= SectDayKey(now) {
			result.Seconds[day] = abs.PositiveListeningSeconds(seconds)
		}
	}
	result.Seconds[SectDayKey(now)] = abs.PositiveListeningSeconds(adjusted[SectDayKey(now)])
	ordered := latestDailyListeningSessions(sessions)
	observedBySource := make(map[string]float64)
	for _, session := range ordered {
		observedBySource[session.DateKey] += session.ListeningSeconds
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StartedAt.Equal(ordered[j].StartedAt) {
			return ordered[i].SessionKey < ordered[j].SessionKey
		}
		return ordered[i].StartedAt.Before(ordered[j].StartedAt)
	})

	// Match the source-day budget already consumed by cross-day reconciliation.
	// Prefer those allocations when a non-atomic ABS response contains more
	// observed session seconds than its earlier official statistics response.
	remaining := make(map[string]float64)
	for day, seconds := range official {
		remaining[day] = abs.PositiveListeningSeconds(seconds)
	}
	baselines := make(map[string]map[string]float64)
	for _, crossOnly := range []bool{true, false} {
		for _, session := range ordered {
			isCross := session.CrossDayStatus == crossDayAdjustmentStatusAccepted || session.CrossDayStatus == crossDayAdjustmentStatusBounded
			if isCross != crossOnly || remaining[session.DateKey] <= 0 {
				continue
			}
			budget := math.Min(session.ListeningSeconds, remaining[session.DateKey])
			remaining[session.DateKey] -= budget
			baseline := map[string]float64{session.DateKey: budget}
			if isCross && budget == session.ListeningSeconds {
				baseline, _ = rebalanceABSDaysForCrossDaySessions(map[string]float64{session.DateKey: budget}, []absListeningSession{session})
			}
			baselines[session.SessionKey] = baseline
			for day, amount := range baseline {
				result.Seconds[day] -= amount
			}
		}
	}

	type sessionWindow struct {
		session        absListeningSession
		end            time.Time
		wall, listened float64
	}
	windows := make([]sessionWindow, 0, len(ordered))
	for _, session := range ordered {
		baseline := baselines[session.SessionKey]
		budget := 0.0
		for _, amount := range baseline {
			budget += amount
		}
		if budget <= 0 {
			continue
		}
		if session.StartedAt.IsZero() || session.UpdatedAt.IsZero() || !session.UpdatedAt.After(session.StartedAt) {
			result.anomaly(session.DateKey, "invalid_session_time")
			continue
		}
		wall := session.UpdatedAt.Sub(session.StartedAt).Seconds()
		listened := math.Min(budget, wall)
		if session.ListeningSeconds > wall+dailyListeningCrossDayClockTolerance.Seconds() {
			result.anomaly(session.DateKey, "listening_exceeds_wall")
		}
		if session.UpdatedAt.After(now.Add(dailyListeningCrossDayClockTolerance)) {
			result.anomaly(session.DateKey, "future_session_time")
		}
		end := session.UpdatedAt
		if end.After(now) {
			end = now
		}
		if !end.After(session.StartedAt) {
			continue
		}
		// A source date can differ from both physical dates (including an
		// uncertain or overstated cross-day session). Include its boundaries
		// without treating that source-day assignment as playback evidence.
		for _, day := range []string{SectDayKey(session.StartedAt), SectDayKey(end.Add(-time.Nanosecond))} {
			if day <= SectDayKey(now) {
				if _, exists := result.Seconds[day]; !exists {
					result.Seconds[day] = 0
				}
			}
		}
		windows = append(windows, sessionWindow{session: session, end: end, wall: wall, listened: listened})
	}

	// Collect all session boundary dates before allocating any playback. A date
	// introduced by a later session must also receive earlier overlapping playback
	// on the first refresh, without depending on previously cached daily rows.
	segments := make(map[string][]dailyListeningSegment)
	for _, window := range windows {
		session, end, wall, listened := window.session, window.end, window.wall, window.listened
		// Bound the work by the collected dates instead of traversing an
		// arbitrarily long or malicious session timestamp range.
		for day := range result.Seconds {
			dayStart, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
			if err != nil {
				continue
			}
			a, b := session.StartedAt, end
			if a.Before(dayStart) {
				a = dayStart
			}
			if next := dayStart.AddDate(0, 0, 1); b.After(next) {
				b = next
			}
			if !b.After(a) {
				continue
			}
			density := listened / wall
			if session.CrossDayStatus == crossDayAdjustmentStatusBounded {
				// With large unknown pauses, only the playback that must lie in
				// this day's window is spendable; hold the uncertain remainder.
				minimum := math.Max(0, listened-(wall-b.Sub(a).Seconds()))
				density = minimum / b.Sub(a).Seconds()
				result.anomaly(day, "allocation_uncertain")
			}
			if density > 0 {
				segments[day] = append(segments[day], dailyListeningSegment{start: a, end: b, density: density, uncertain: session.CrossDayStatus == crossDayAdjustmentStatusBounded})
			}
			if listened < wall && session.CrossDayStatus != crossDayAdjustmentStatusBounded {
				detail := result.Details[day]
				detail.EstimatedAllocation = true
				result.Details[day] = detail
			}
		}
	}
	for day := range result.Seconds {
		unique, sum := unionDailyListeningSegments(segments[day])
		detail := result.Details[day]
		detail.OverlapSeconds = math.Max(0, sum-unique)
		result.Details[day] = detail
		if detail.OverlapSeconds > 1e-6 {
			result.anomaly(day, "overlapping_sessions")
		}
		seconds := math.Max(0, result.Seconds[day]) + unique
		limit := dailyListeningElapsedLimit(day, now)
		if seconds > limit+1e-6 {
			result.anomaly(day, "daily_elapsed_limit")
		}
		seconds = math.Min(seconds, limit)
		result.Seconds[day] = seconds
		detail = result.Details[day]
		detail.Version, detail.OfficialSeconds, detail.AdjustedSeconds = dailyListeningValidationVersion, abs.PositiveListeningSeconds(official[day]), abs.PositiveListeningSeconds(adjusted[day])
		detail.CreditedSeconds, detail.ElapsedLimitSeconds = seconds, limit
		observed := observedBySource[day]
		detail.ObservedSourceSeconds = &observed
		result.Details[day] = detail
	}
	return result
}

func latestDailyListeningSessions(sessions []absListeningSession) []absListeningSession {
	byKey := make(map[string]absListeningSession)
	for _, session := range sessions {
		if session.SessionKey == "" {
			continue
		}
		previous, exists := byKey[session.SessionKey]
		if !exists || previous.UpdatedAt.Before(session.UpdatedAt) ||
			(previous.UpdatedAt.Equal(session.UpdatedAt) && (session.CrossDayStatus == crossDayAdjustmentStatusDeleted || session.CrossDayStatus == crossDayAdjustmentStatusInvalidated)) {
			byKey[session.SessionKey] = session
		}
	}
	result := make([]absListeningSession, 0, len(byKey))
	for _, session := range byKey {
		// Select the newest version before filtering out explicit zero/deleted
		// observations, otherwise an older positive version can win by default.
		if session.CrossDayStatus != crossDayAdjustmentStatusDeleted && session.CrossDayStatus != crossDayAdjustmentStatusInvalidated && session.HasListeningTime && validDailyListeningDayKey(session.DateKey) && finitePositiveListeningSeconds(session.ListeningSeconds) {
			result = append(result, session)
		}
	}
	return result
}

func finitePositiveListeningSeconds(seconds float64) bool {
	return seconds > 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}

func dailyListeningElapsedLimit(day string, now time.Time) float64 {
	start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
	if err != nil || !now.After(start) {
		return 0
	}
	return math.Min(now.Sub(start).Seconds(), start.AddDate(0, 0, 1).Sub(start).Seconds())
}

func (v *dailyListeningValidation) anomaly(day, reason string) {
	detail := v.Details[day]
	if detail.Anomalies == nil {
		detail.Anomalies = make(map[string]int)
	}
	detail.Anomalies[reason]++
	v.Details[day] = detail
}

type dailyListeningSegment struct {
	start, end time.Time
	density    float64
	uncertain  bool
}

// Only components with uncertain pause placement need the interval lower-bound
// solver. Keep the existing proportional estimates for all other components.
func unionDailyListeningSegments(segments []dailyListeningSegment) (unique, sum float64) {
	ordered := append([]dailyListeningSegment(nil), segments...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start.Before(ordered[j].start) })
	for first := 0; first < len(ordered); {
		end, uncertain := ordered[first].end, ordered[first].uncertain
		last := first + 1
		for last < len(ordered) && ordered[last].start.Before(end) {
			if ordered[last].end.After(end) {
				end = ordered[last].end
			}
			uncertain = uncertain || ordered[last].uncertain
			last++
		}
		component := ordered[first:last]
		amount, total := unionEstimatedDailyListeningSegments(component)
		if uncertain {
			amount = minimumDailyListeningCoverage(component)
		}
		unique += amount
		sum += total
		first = last
	}
	return unique, sum
}

// Each interval requires a total amount, without assuming where its pauses
// occurred. In end-time order, add only missing coverage, as far right as
// possible so later intervals can reuse it. This gives the minimum union
// satisfying all interval requirements, rather than inventing playback density.
// A Fenwick tree tracks covered seconds; a disjoint set skips full cells.
func minimumDailyListeningCoverage(segments []dailyListeningSegment) float64 {
	boundaries := make([]time.Time, 0, 2*len(segments))
	for _, segment := range segments {
		boundaries = append(boundaries, segment.start, segment.end)
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].Before(boundaries[j]) })
	times := make([]time.Time, 0, len(boundaries))
	for _, at := range boundaries {
		if len(times) == 0 || !times[len(times)-1].Equal(at) {
			times = append(times, at)
		}
	}
	if len(times) < 2 {
		return 0
	}
	n := len(times) - 1
	covered, tree, parent := make([]float64, n+1), make([]float64, n+1), make([]int, n+1)
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		root := i
		for parent[root] != root {
			root = parent[root]
		}
		for parent[i] != i {
			next := parent[i]
			parent[i] = root
			i = next
		}
		return root
	}
	prefix := func(i int) float64 {
		seconds := 0.0
		for ; i > 0; i -= i & -i {
			seconds += tree[i]
		}
		return seconds
	}
	ordered := append([]dailyListeningSegment(nil), segments...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].end.Before(ordered[j].end) })
	for _, segment := range ordered {
		left := sort.Search(len(times), func(i int) bool { return !times[i].Before(segment.start) })
		right := sort.Search(len(times), func(i int) bool { return !times[i].Before(segment.end) })
		missing := segment.end.Sub(segment.start).Seconds()*segment.density - (prefix(right) - prefix(left))
		for i := find(right); i > left && missing > 1e-9; i = find(i) {
			capacity := times[i].Sub(times[i-1]).Seconds() - covered[i]
			added := math.Min(missing, capacity)
			covered[i] += added
			missing -= added
			for j := i; j <= n; j += j & -j {
				tree[j] += added
			}
			if added >= capacity {
				parent[i] = find(i - 1)
			}
		}
	}
	return prefix(n)
}

// A paused session keeps its playback/wall-time ratio for reward estimates.
// Overlap uses the largest active ratio, never the sum; pause time therefore
// cannot become additional playback. These estimates are not penalty evidence.
func unionEstimatedDailyListeningSegments(segments []dailyListeningSegment) (unique, sum float64) {
	type boundary struct {
		at    time.Time
		index int
		start bool
	}
	boundaries := make([]boundary, 0, 2*len(segments))
	for i, segment := range segments {
		boundaries = append(boundaries, boundary{segment.start, i, true}, boundary{segment.end, i, false})
		sum += segment.end.Sub(segment.start).Seconds() * segment.density
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].at.Before(boundaries[j].at) })
	active := make([]bool, len(segments))
	queue := &dailyListeningDensityHeap{segments: segments}
	var previous time.Time
	for i := 0; i < len(boundaries); {
		at := boundaries[i].at
		if queue.Len() > 0 {
			unique += at.Sub(previous).Seconds() * segments[queue.indices[0]].density
		}
		for i < len(boundaries) && boundaries[i].at.Equal(at) {
			b := boundaries[i]
			active[b.index] = b.start
			if b.start {
				heap.Push(queue, b.index)
			}
			i++
		}
		for queue.Len() > 0 && !active[queue.indices[0]] {
			heap.Pop(queue)
		}
		previous = at
	}
	return unique, sum
}

type dailyListeningDensityHeap struct {
	segments []dailyListeningSegment
	indices  []int
}

func (h dailyListeningDensityHeap) Len() int { return len(h.indices) }
func (h dailyListeningDensityHeap) Less(i, j int) bool {
	return h.segments[h.indices[i]].density > h.segments[h.indices[j]].density
}
func (h dailyListeningDensityHeap) Swap(i, j int) {
	h.indices[i], h.indices[j] = h.indices[j], h.indices[i]
}
func (h *dailyListeningDensityHeap) Push(v any) { h.indices = append(h.indices, v.(int)) }
func (h *dailyListeningDensityHeap) Pop() any {
	last := h.indices[len(h.indices)-1]
	h.indices = h.indices[:len(h.indices)-1]
	return last
}

// Read only persisted, version-accepted observations. Re-appending raw HTTP
// sessions here would bypass stale-response checks (including tombstones).
// cross retains allocations and retired dates from the same accepted ledger.
func loadDailyListeningValidationSessions(userID int64, absID string, _ absListeningSessionSnapshot, cross []absListeningSession) ([]absListeningSession, error) {
	result := append([]absListeningSession(nil), cross...)
	var rows []model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where("user_id = ? AND abs_user_id = ? AND status IN ? AND algorithm_version = ?", userID, strings.TrimSpace(absID), []string{crossDayAdjustmentStatusSameDay, crossDayAdjustmentStatusUncertain}, dailyListeningCrossDayAlgorithmVersion).Find(&rows).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(cross))
	for _, session := range cross {
		seen[session.SessionKey] = true
	}
	for _, row := range rows {
		if seen[row.SessionKey] {
			continue
		}
		result = append(result, absListeningSession{SessionKey: row.SessionKey, DateKey: row.SourceDay, StartedAt: row.StartedAt, UpdatedAt: row.SessionUpdatedAt, ListeningSeconds: row.ListeningSeconds, HasListeningTime: true, CrossDayStatus: row.Status})
	}
	return result, nil
}

func applyDailyListeningValidation(record *model.DailyListeningStat, validation *dailyListeningValidation) {
	if validation == nil {
		return
	}
	record.OfficialRawSeconds = abs.PositiveListeningSeconds(validation.Official[record.DayKey])
	record.CappedSeconds = validation.Seconds[record.DayKey]
	record.EffectiveHours = calculateSectEffectiveHoursFromSeconds(record.CappedSeconds)
	detail := validation.Details[record.DayKey]
	detail.CreditedSeconds = record.CappedSeconds
	encoded, _ := json.Marshal(detail)
	record.ValidationJSON = string(encoded)
	if len(detail.Anomalies) > 0 {
		record.RefreshReason = "abs_daily_validation"
	}
}

func dailyListeningValidationNotice(stat model.DailyListeningStat) string {
	var detail dailyListeningValidationDetail
	if json.Unmarshal([]byte(stat.ValidationJSON), &detail) != nil {
		return ""
	}
	parts := []string{}
	if detail.OverlapSeconds > 1e-6 {
		parts = append(parts, fmt.Sprintf("已去除重叠听书 %.2f 小时", detail.OverlapSeconds/3600))
	}
	if detail.Anomalies["daily_elapsed_limit"] > 0 || detail.Anomalies["future_session_time"] > 0 || detail.Anomalies["listening_exceeds_wall"] > 0 {
		parts = append(parts, "超出实际时间范围的部分未计入")
	}
	if detail.Anomalies["invalid_session_time"] > 0 || detail.Anomalies["allocation_uncertain"] > 0 {
		parts = append(parts, "无法确认归属的部分暂未计入")
	}
	if detail.EstimatedAllocation {
		parts = append(parts, "含暂停会话的时间分配为估算")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n听书校验：" + strings.Join(parts, "；") + "。"
}

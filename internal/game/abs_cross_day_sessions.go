package game

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	crossDayAdjustmentStatusSameDay        = "same_day"
	dailyListeningCrossDaySource           = "abs_sessions_cross_day"
	dailyListeningCrossDayStatus           = "corrected"
	dailyListeningCrossDayClockTolerance   = 2 * time.Minute
	dailyListeningCrossDayMinIdleAllowance = 15 * time.Minute
	dailyListeningSessionPageSize          = 100
	dailyListeningSessionMaxPages          = 5
	dailyListeningCrossDayAlgorithmVersion = 2
)

const (
	crossDayAdjustmentStatusAccepted         = "accepted"
	crossDayAdjustmentStatusBounded          = "bounded"
	crossDayAdjustmentStatusUncertain        = "uncertain"
	crossDayAdjustmentStatusInvalidated      = "invalidated"
	crossDayAdjustmentStatusDeleted          = "deleted"
	crossDayAdjustmentConfidenceEstimated    = "estimated"
	crossDayAdjustmentConfidenceConservative = "conservative"
	crossDayAdjustmentConfidenceUncertain    = "uncertain"
)

type absListeningSession struct {
	SessionKey       string
	ListeningSeconds float64
	HasListeningTime bool
	DateKey          string
	StartedAt        time.Time
	UpdatedAt        time.Time

	// Persisted cross-day metadata is populated only when loading an observed
	// adjustment from SQLite. Bounded allocations contain mathematically
	// guaranteed per-day minimums, never guessed pause placement.
	CrossDayStatus   string
	StoredAllocation map[string]float64
	RetiredDays      []string
}

func parseABSSessionsPayload(body []byte) ([]map[string]interface{}, error) {
	var response struct {
		Sessions []map[string]interface{} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Sessions == nil {
		return nil, fmt.Errorf("ABS_SESSIONS_FIELD_MISSING")
	}
	return response.Sessions, nil
}

func parseABSListeningSession(raw map[string]interface{}, absUserID string) (absListeningSession, bool) {
	sessionKey, _ := raw["id"].(string)
	if strings.TrimSpace(sessionKey) == "" {
		return absListeningSession{}, false
	}
	startedAt, hasStart := timeValue(raw["startedAt"])
	updatedAt, hasUpdate := timeValue(raw["updatedAt"])
	listeningSeconds, hasListeningTime := raw["timeListening"].(float64)
	dateKey, _ := raw["date"].(string)

	if !hasListeningTime && !hasStart && !hasUpdate {
		return absListeningSession{}, false
	}

	return absListeningSession{
		SessionKey:       textfmt.FormatPlainValue(sessionKey),
		ListeningSeconds: listeningSeconds,
		HasListeningTime: hasListeningTime,
		DateKey:          dateKey,
		StartedAt:        startedAt,
		UpdatedAt:        updatedAt,
	}, true
}

type absListeningSessionSnapshot struct {
	Sessions  []absListeningSession
	StartedAt time.Time
	Complete  bool
}

func fetchABSCrossDayListeningSessions(absUserID string) (absListeningSessionSnapshot, error) {
	snapshot := absListeningSessionSnapshot{StartedAt: time.Now()}
	if abs.DefaultClient == nil || strings.TrimSpace(absUserID) == "" {
		return snapshot, fmt.Errorf("ABS \u4f1a\u8bdd\u8bfb\u53d6\u53c2\u6570\u4e0d\u5b8c\u6574")
	}

	sessions := make([]absListeningSession, 0, dailyListeningSessionPageSize)
	seenSessionKeys := make(map[string]struct{}, dailyListeningSessionPageSize)
	valid := true
	for page := 0; page < dailyListeningSessionMaxPages; page++ {
		body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningSessionsPath(absUserID, page, dailyListeningSessionPageSize), nil)
		if err != nil {
			return snapshot, err
		}
		if code != 200 {
			return snapshot, &abs.AbsAPIError{Operation: "\u8bfb\u53d6 ABS \u7528\u6237\u4f1a\u8bdd\u5931\u8d25", StatusCode: code, Message: "\u54cd\u5e94: " + abs.AbsResponseSnippet(body)}
		}
		rawSessions, err := parseABSSessionsPayload(body)
		if err != nil {
			return snapshot, err
		}
		for _, raw := range rawSessions {
			if !absSessionBelongsToUser(raw, absUserID) {
				valid = false
				continue
			}
			session, ok := parseABSListeningSession(raw, absUserID)
			if !ok {
				valid = false
				continue
			}
			key := strings.TrimSpace(session.SessionKey)
			if key == "" {
				continue
			}
			if _, exists := seenSessionKeys[key]; exists {
				valid = false
				continue
			}
			seenSessionKeys[key] = struct{}{}
			sessions = append(sessions, session)
		}
		if len(rawSessions) < dailyListeningSessionPageSize {
			snapshot.Complete = valid
			break
		}
	}
	snapshot.Sessions = sessions
	return snapshot, nil
}

func persistAndLoadABSCrossDaySessions(userID int64, absUserID string, days map[string]float64, snapshot absListeningSessionSnapshot) ([]absListeningSession, error) {
	if storage.DB == nil {
		return nil, fmt.Errorf("ABS_CROSS_DAY_DB_EMPTY")
	}
	if userID == 0 || strings.TrimSpace(absUserID) == "" {
		return nil, fmt.Errorf("ABS_CROSS_DAY_USER_EMPTY")
	}
	if snapshot.StartedAt.IsZero() {
		snapshot.StartedAt = time.Now()
	}

	observations, diagnostics := buildABSCrossDaySessionObservations(userID, absUserID, snapshot.Sessions, snapshot.StartedAt)
	if err := storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCurrentListeningBindingInTx(tx, userID, absUserID); err != nil {
			return err
		}
		if err := upsertABSCrossDaySessionObservations(tx, observations, days); err != nil {
			return err
		}
		if snapshot.Complete {
			return reconcileCompleteABSCrossDaySnapshotInTx(tx, userID, absUserID, days, snapshot)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("persist ABS cross-day sessions: %w", err)
	}
	if len(diagnostics) > 0 {
		parts := make([]string, 0, len(diagnostics))
		for key, count := range diagnostics {
			parts = append(parts, fmt.Sprintf("%s=%d", key, count))
		}
		sort.Strings(parts)
		log.Printf("ABS cross-day session diagnostics: user=%d abs=%s %s", userID, textfmt.FormatPlainValue(absUserID), strings.Join(parts, ","))
	}

	// A missing source-day key must still be validated against known sessions.
	// Invalidated rows retain the affected dates so obsolete allocations clear.
	var stored []model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where(
		"user_id = ? AND abs_user_id = ? AND (status IN ? OR retired_days_json <> '') AND algorithm_version = ?",
		userID, strings.TrimSpace(absUserID), []string{crossDayAdjustmentStatusAccepted, crossDayAdjustmentStatusBounded, crossDayAdjustmentStatusInvalidated, crossDayAdjustmentStatusDeleted}, dailyListeningCrossDayAlgorithmVersion,
	).Order("started_at ASC, session_key ASC").Find(&stored).Error; err != nil {
		return nil, fmt.Errorf("load ABS cross-day sessions: %w", err)
	}

	result := make([]absListeningSession, 0, len(stored))
	for _, row := range stored {
		var retired []string
		if row.RetiredDaysJSON != "" {
			if err := json.Unmarshal([]byte(row.RetiredDaysJSON), &retired); err != nil {
				return nil, fmt.Errorf("decode retired ABS session dates: %w", err)
			}
		}
		allocation := make(map[string]float64)
		if strings.TrimSpace(row.AllocationJSON) != "" {
			if err := json.Unmarshal([]byte(row.AllocationJSON), &allocation); err != nil {
				log.Printf("ABS cross-day allocation decode failed: user=%d session=%s err=%s", userID, textfmt.FormatPlainValue(row.SessionKey), textfmt.FormatPlainError(err))
				continue
			}
		}
		result = append(result, absListeningSession{
			SessionKey:       row.SessionKey,
			DateKey:          row.SourceDay,
			StartedAt:        row.StartedAt,
			UpdatedAt:        row.SessionUpdatedAt,
			ListeningSeconds: row.ListeningSeconds,
			HasListeningTime: true,
			CrossDayStatus:   row.Status,
			StoredAllocation: allocation,
			RetiredDays:      retired,
		})
	}
	return result, nil
}

func buildABSCrossDaySessionObservations(userID int64, absUserID string, sessions []absListeningSession, observedAt time.Time) ([]model.ABSCrossDaySessionAdjustment, map[string]int) {
	observations := make([]model.ABSCrossDaySessionAdjustment, 0, len(sessions))
	diagnostics := make(map[string]int)
	for _, session := range sessions {
		status, confidence, reason, tracked := classifyABSCrossDaySession(session)
		if session.SessionKey != "" && session.HasListeningTime && session.ListeningSeconds == 0 {
			// An explicit zero is a correction, not an absent observation. Retain
			// a tombstone so old pages cannot consume another session's budget.
			status, confidence, reason, tracked = crossDayAdjustmentStatusDeleted, crossDayAdjustmentConfidenceConservative, "listening_cleared", true
		} else if !tracked {
			if session.SessionKey == "" || !session.HasListeningTime || !finitePositiveListeningSeconds(session.ListeningSeconds) {
				continue
			}
			status, confidence, reason = crossDayAdjustmentStatusSameDay, crossDayAdjustmentConfidenceConservative, "same_day_observation"
			if session.StartedAt.IsZero() || session.UpdatedAt.IsZero() || !session.UpdatedAt.After(session.StartedAt) {
				status, confidence, reason, tracked = crossDayAdjustmentStatusUncertain, crossDayAdjustmentConfidenceUncertain, "invalid_session_time", true
			}
		}
		allocationJSON := ""
		switch status {
		case crossDayAdjustmentStatusAccepted:
			allocationJSON = crossDaySessionAllocationJSON(session)
		case crossDayAdjustmentStatusBounded:
			allocationJSON = crossDaySessionMinimumAllocationJSON(session)
		}
		if tracked {
			diagnostics[status+":"+reason]++
		}
		observations = append(observations, model.ABSCrossDaySessionAdjustment{
			UserID:           userID,
			AbsUserID:        strings.TrimSpace(absUserID),
			SessionKey:       strings.TrimSpace(session.SessionKey),
			SourceDay:        strings.TrimSpace(session.DateKey),
			StartedAt:        session.StartedAt,
			SessionUpdatedAt: session.UpdatedAt,
			ListeningSeconds: session.ListeningSeconds,
			AllocationJSON:   allocationJSON,
			Status:           status,
			Confidence:       confidence,
			Reason:           reason,
			AlgorithmVersion: dailyListeningCrossDayAlgorithmVersion,
			ObservedAt:       observedAt,
		})
	}
	return observations, diagnostics
}

func upsertABSCrossDaySessionObservations(tx *gorm.DB, observations []model.ABSCrossDaySessionAdjustment, budgets ...map[string]float64) error {
	if tx == nil {
		return fmt.Errorf("ABS_CROSS_DAY_DB_EMPTY")
	}
	if len(observations) == 0 {
		return nil
	}
	previous, eligible, err := prepareABSCrossDaySessionObservationsInTx(tx, observations)
	if err != nil {
		return err
	}
	if len(eligible) == 0 {
		return nil
	}
	if err := validateABSListeningReductionsInTx(tx, previous, eligible, budgets); err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "abs_user_id"}, {Name: "session_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "source_day", "started_at", "session_updated_at", "listening_seconds",
			"allocation_json", "retired_days_json", "status", "confidence", "reason", "algorithm_version", "observed_at", "updated_at",
		}),
	}).CreateInBatches(&eligible, 50).Error
}

// Shared by normal upserts and progressive scans so deferred corrections retain
// exactly the same version and tombstone ordering as foreground observations.
func prepareABSCrossDaySessionObservationsInTx(tx *gorm.DB, observations []model.ABSCrossDaySessionAdjustment) ([]model.ABSCrossDaySessionAdjustment, []model.ABSCrossDaySessionAdjustment, error) {
	// Callers hold the binding write lock. Do not let late HTTP responses replace
	// newer session evidence; time.Time also handles mixed UTC/local offsets.
	absIDs, keys := make([]string, 0), make([]string, 0, len(observations))
	seenAbsIDs := make(map[string]bool)
	for _, observation := range observations {
		if !seenAbsIDs[observation.AbsUserID] {
			absIDs = append(absIDs, observation.AbsUserID)
			seenAbsIDs[observation.AbsUserID] = true
		}
		keys = append(keys, observation.SessionKey)
	}
	var previous []model.ABSCrossDaySessionAdjustment
	if err := tx.Where("abs_user_id IN ? AND session_key IN ?", absIDs, keys).Find(&previous).Error; err != nil {
		return nil, nil, err
	}
	byKey := make(map[string]model.ABSCrossDaySessionAdjustment, len(previous))
	for _, old := range previous {
		byKey[old.AbsUserID+"\x00"+old.SessionKey] = old
	}
	eligible := make([]model.ABSCrossDaySessionAdjustment, 0, len(observations))
	for _, observation := range observations {
		old, exists := byKey[observation.AbsUserID+"\x00"+observation.SessionKey]
		if observation.Status == crossDayAdjustmentStatusInvalidated {
			if !exists {
				continue // A tombstone needs a prior allocation to clear.
			}
			observation.SourceDay, observation.AllocationJSON = old.SourceDay, old.AllocationJSON
		}
		if observation.Status == crossDayAdjustmentStatusDeleted && exists {
			observation.SourceDay, observation.AllocationJSON = old.SourceDay, old.AllocationJSON
			if observation.Reason == "listening_cleared" && !observation.ObservedAt.Before(old.ObservedAt) && observation.SessionUpdatedAt.Before(old.SessionUpdatedAt) {
				// A zero correction may omit or reset updatedAt. Keep the last
				// known session clock while ordering the clear by observation time.
				observation.SessionUpdatedAt = old.SessionUpdatedAt
			}
		}
		if exists && old.UserID == observation.UserID {
			if (old.Status == crossDayAdjustmentStatusInvalidated || old.Status == crossDayAdjustmentStatusDeleted) && observation.ObservedAt.Before(old.ObservedAt) {
				continue
			}
			if old.SessionUpdatedAt.After(observation.SessionUpdatedAt) ||
				(old.SessionUpdatedAt.Equal(observation.SessionUpdatedAt) && old.ObservedAt.After(observation.ObservedAt)) {
				continue
			}
		}
		if exists {
			retired, err := retiredABSListeningDays(old, observation)
			if err != nil {
				return nil, nil, err
			}
			observation.RetiredDaysJSON = retired
		}
		eligible = append(eligible, observation)
	}
	return previous, eligible, nil
}

// Retiring an allocation does not retire the current playback observation.
// Keep only the old date keys, independently of its current source and status.
func retiredABSListeningDays(old, current model.ABSCrossDaySessionAdjustment) (string, error) {
	var previous []string
	if old.RetiredDaysJSON != "" {
		if err := json.Unmarshal([]byte(old.RetiredDaysJSON), &previous); err != nil {
			return "", fmt.Errorf("decode retired ABS session dates: %w", err)
		}
	}
	if old.SourceDay != current.SourceDay || old.AllocationJSON != current.AllocationJSON ||
		(activeABSListeningObservation(old) && current.ListeningSeconds+1e-6 < old.ListeningSeconds) {
		previous = append(previous, old.SourceDay)
		if old.AllocationJSON != "" {
			var allocation map[string]float64
			if err := json.Unmarshal([]byte(old.AllocationJSON), &allocation); err != nil {
				return "", fmt.Errorf("decode previous ABS allocation: %w", err)
			}
			for day := range allocation {
				previous = append(previous, day)
			}
		}
	}
	seen, days := make(map[string]bool), make([]string, 0, len(previous))
	for _, day := range previous {
		if validDailyListeningDayKey(day) && !seen[day] {
			seen[day] = true
			days = append(days, day)
		}
	}
	if len(days) == 0 {
		return "", nil
	}
	sort.Strings(days)
	encoded, err := json.Marshal(days)
	return string(encoded), err
}

type absCrossDaySnapshotMismatchError struct {
	Day                   string
	StatsSeconds          float64
	SessionSeconds        float64
	OnlyMissingSourceDays bool
}

func (e *absCrossDaySnapshotMismatchError) Error() string {
	return fmt.Sprintf("ABS_CROSS_DAY_SNAPSHOT_MISMATCH: day=%s stats_seconds=%.3f session_seconds=%.3f", e.Day, e.StatsSeconds, e.SessionSeconds)
}

func validateABSCrossDaySnapshot(days map[string]float64, sessions []absListeningSession) error {
	totals := make(map[string]float64)
	seen := make(map[string]bool)
	for _, session := range sessions {
		if session.CrossDayStatus == crossDayAdjustmentStatusInvalidated || session.CrossDayStatus == crossDayAdjustmentStatusDeleted {
			continue
		}
		if seen[session.SessionKey] {
			continue
		}
		seen[session.SessionKey] = true
		totals[session.DateKey] += session.ListeningSeconds
	}
	var mismatch *absCrossDaySnapshotMismatchError
	for day, seconds := range totals {
		if seconds > abs.PositiveListeningSeconds(days[day])+1e-6 {
			if mismatch == nil {
				mismatch = &absCrossDaySnapshotMismatchError{day, abs.PositiveListeningSeconds(days[day]), seconds, true}
			}
			if _, present := days[day]; present {
				mismatch.OnlyMissingSourceDays = false
			}
		}
	}
	if mismatch != nil {
		return mismatch
	}
	return nil
}

func classifyABSCrossDaySession(session absListeningSession) (status string, confidence string, reason string, tracked bool) {
	if strings.TrimSpace(session.SessionKey) == "" || session.StartedAt.IsZero() || session.UpdatedAt.IsZero() || !session.UpdatedAt.After(session.StartedAt) {
		return "", "", "", false
	}
	if SectDayKey(session.StartedAt) == SectDayKey(session.UpdatedAt.Add(-time.Nanosecond)) {
		return "", "", "", false
	}
	if !session.HasListeningTime || session.ListeningSeconds <= 0 || math.IsNaN(session.ListeningSeconds) || math.IsInf(session.ListeningSeconds, 0) {
		return crossDayAdjustmentStatusUncertain, crossDayAdjustmentConfidenceUncertain, "listening_invalid", true
	}
	if !validDailyListeningDayKey(session.DateKey) {
		return crossDayAdjustmentStatusUncertain, crossDayAdjustmentConfidenceUncertain, "source_day_invalid", true
	}
	wallSeconds := session.UpdatedAt.Sub(session.StartedAt).Seconds()
	if session.ListeningSeconds > wallSeconds+dailyListeningCrossDayClockTolerance.Seconds() {
		return crossDayAdjustmentStatusUncertain, crossDayAdjustmentConfidenceUncertain, "listening_exceeds_wall", true
	}
	idleSeconds := math.Max(0, wallSeconds-session.ListeningSeconds)
	maxIdleSeconds := math.Max(dailyListeningCrossDayMinIdleAllowance.Seconds(), session.ListeningSeconds*0.25)
	if idleSeconds > maxIdleSeconds {
		return crossDayAdjustmentStatusBounded, crossDayAdjustmentConfidenceConservative, "idle_excess_lower_bound", true
	}
	return crossDayAdjustmentStatusAccepted, crossDayAdjustmentConfidenceEstimated, "wall_clock_ratio", true
}

func crossDaySessionAllocationJSON(session absListeningSession) string {
	segments := splitDurationByBeijingDay(session.StartedAt, session.UpdatedAt)
	total := 0.0
	for _, seconds := range segments {
		total += seconds
	}
	if total <= 0 {
		return ""
	}
	allocation := make(map[string]float64, len(segments))
	for dayKey, seconds := range segments {
		allocation[dayKey] = session.ListeningSeconds * seconds / total
	}
	body, err := json.Marshal(allocation)
	if err != nil {
		return ""
	}
	return string(body)
}

func crossDaySessionMinimumAllocationJSON(session absListeningSession) string {
	segments := splitDurationByBeijingDay(session.StartedAt, session.UpdatedAt)
	wallSeconds := session.UpdatedAt.Sub(session.StartedAt).Seconds()
	if wallSeconds <= 0 || session.ListeningSeconds <= 0 || session.ListeningSeconds > wallSeconds+dailyListeningCrossDayClockTolerance.Seconds() {
		return ""
	}
	allocation := make(map[string]float64, len(segments))
	for dayKey, segmentSeconds := range segments {
		minimum := math.Max(0, session.ListeningSeconds-(wallSeconds-segmentSeconds))
		minimum = math.Min(minimum, math.Min(session.ListeningSeconds, segmentSeconds))
		if minimum > 1e-6 {
			allocation[dayKey] = minimum
		}
	}
	body, err := json.Marshal(allocation)
	if err != nil {
		return ""
	}
	return string(body)
}

func rebalanceABSDaysForCrossDaySessions(days map[string]float64, sessions []absListeningSession) (map[string]float64, map[string]bool) {
	adjusted := make(map[string]float64, len(days))
	remainingOfficialByDay := make(map[string]float64, len(days))
	for dayKey, seconds := range days {
		dayKey = strings.TrimSpace(dayKey)
		if dayKey == "" {
			continue
		}
		seconds = abs.PositiveListeningSeconds(seconds)
		adjusted[dayKey] = seconds
		remainingOfficialByDay[dayKey] = seconds
	}

	correctedDays := make(map[string]bool)
	orderedSessions := append([]absListeningSession(nil), sessions...)
	sort.SliceStable(orderedSessions, func(i, j int) bool {
		if orderedSessions[i].StartedAt.Equal(orderedSessions[j].StartedAt) {
			return orderedSessions[i].SessionKey < orderedSessions[j].SessionKey
		}
		return orderedSessions[i].StartedAt.Before(orderedSessions[j].StartedAt)
	})
	seenSessionKeys := make(map[string]bool, len(orderedSessions))
	for _, session := range orderedSessions {
		sessionKey := strings.TrimSpace(session.SessionKey)
		if sessionKey == "" || seenSessionKeys[sessionKey] {
			continue
		}
		seenSessionKeys[sessionKey] = true
		for _, day := range session.RetiredDays {
			if validDailyListeningDayKey(day) {
				if _, exists := adjusted[day]; !exists {
					adjusted[day] = 0
				}
				correctedDays[day] = true
			}
		}

		if session.CrossDayStatus == crossDayAdjustmentStatusInvalidated || session.CrossDayStatus == crossDayAdjustmentStatusDeleted {
			if _, exists := adjusted[session.DateKey]; !exists {
				adjusted[session.DateKey] = 0
			}
			for day := range session.StoredAllocation {
				if _, exists := adjusted[day]; !exists {
					adjusted[day] = 0
				}
			}
			continue
		}

		if session.CrossDayStatus == crossDayAdjustmentStatusBounded {
			sourceDay := strings.TrimSpace(session.DateKey)
			remaining := remainingOfficialByDay[sourceDay]
			if !validDailyListeningDayKey(sourceDay) || remaining+1e-6 < session.ListeningSeconds {
				continue
			}
			moveByDay := make(map[string]float64)
			totalMoved := 0.0
			for dayKey, seconds := range session.StoredAllocation {
				seconds = abs.PositiveListeningSeconds(seconds)
				if dayKey == sourceDay || !validDailyListeningDayKey(dayKey) || seconds <= 1e-6 {
					continue
				}
				moveByDay[dayKey] += seconds
				totalMoved += seconds
			}
			if totalMoved <= 1e-6 || totalMoved > session.ListeningSeconds+1e-6 {
				continue
			}
			remainingOfficialByDay[sourceDay] = math.Max(0, remaining-session.ListeningSeconds)
			adjusted[sourceDay] = math.Max(0, adjusted[sourceDay]-totalMoved)
			correctedDays[sourceDay] = true
			for dayKey, seconds := range moveByDay {
				adjusted[dayKey] += seconds
				correctedDays[dayKey] = true
			}
			continue
		}

		if !crossDayListeningSessionCanRebalance(session) {
			continue
		}
		segments := splitDurationByBeijingDay(session.StartedAt, session.UpdatedAt)
		if len(segments) < 2 {
			continue
		}

		sourceDay := strings.TrimSpace(session.DateKey)
		remaining := remainingOfficialByDay[sourceDay]
		if remaining+1e-6 < session.ListeningSeconds {
			continue
		}
		remainingOfficialByDay[sourceDay] = math.Max(0, remaining-session.ListeningSeconds)
		adjusted[sourceDay] = math.Max(0, adjusted[sourceDay]-session.ListeningSeconds)

		totalSegmentSeconds := 0.0
		for _, segmentSeconds := range segments {
			totalSegmentSeconds += segmentSeconds
		}
		if totalSegmentSeconds <= 0 {
			continue
		}
		for dayKey, segmentSeconds := range segments {
			adjusted[dayKey] += session.ListeningSeconds * (segmentSeconds / totalSegmentSeconds)
			correctedDays[dayKey] = true
		}
		correctedDays[sourceDay] = true
	}
	return adjusted, correctedDays
}

func crossDayListeningSessionCanRebalance(session absListeningSession) bool {
	if !session.HasListeningTime || session.ListeningSeconds <= 0 ||
		math.IsNaN(session.ListeningSeconds) || math.IsInf(session.ListeningSeconds, 0) ||
		session.StartedAt.IsZero() || session.UpdatedAt.IsZero() || !session.UpdatedAt.After(session.StartedAt) ||
		!validDailyListeningDayKey(session.DateKey) {
		return false
	}
	if SectDayKey(session.StartedAt) == SectDayKey(session.UpdatedAt.Add(-time.Nanosecond)) {
		return false
	}

	wallSeconds := session.UpdatedAt.Sub(session.StartedAt).Seconds()
	if session.ListeningSeconds > wallSeconds+dailyListeningCrossDayClockTolerance.Seconds() {
		return false
	}
	idleSeconds := math.Max(0, wallSeconds-session.ListeningSeconds)
	maxIdleSeconds := math.Max(dailyListeningCrossDayMinIdleAllowance.Seconds(), session.ListeningSeconds*0.25)
	return idleSeconds <= maxIdleSeconds
}

func validDailyListeningDayKey(dayKey string) bool {
	dayKey = strings.TrimSpace(dayKey)
	if len(dayKey) != len("2006-01-02") {
		return false
	}
	parsed, err := time.ParseInLocation("2006-01-02", dayKey, storage.DailyOperationsLocation)
	return err == nil && parsed.Format("2006-01-02") == dayKey
}

func absSessionBelongsToUser(raw map[string]interface{}, absUserID string) bool {
	want := strings.TrimSpace(absUserID)
	if want == "" {
		return false
	}
	got, _ := raw["userId"].(string)
	return strings.TrimSpace(got) == want
}

func splitDurationByBeijingDay(start time.Time, end time.Time) map[string]float64 {
	result := make(map[string]float64)
	if !end.After(start) {
		return result
	}
	loc := storage.DailyOperationsLocation
	cursor := start
	for cursor.Before(end) {
		local := cursor.In(loc)
		nextLocalMidnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
		next := nextLocalMidnight
		if next.After(end) {
			next = end
		}
		if next.After(cursor) {
			result[local.Format("2006-01-02")] += next.Sub(cursor).Seconds()
		}
		cursor = next
	}
	return result
}

func timeValue(raw interface{}) (time.Time, bool) {
	switch v := raw.(type) {
	case time.Time:
		return v, !v.IsZero()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z07:00", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
			if t, err := time.ParseInLocation(layout, s, storage.DailyOperationsLocation); err == nil {
				return t, true
			}
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return unixFlexibleTime(n)
		}
	case float64:
		return unixFlexibleTime(int64(v))
	case int64:
		return unixFlexibleTime(v)
	case int:
		return unixFlexibleTime(int64(v))
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return unixFlexibleTime(n)
		}
	}
	return time.Time{}, false
}

func unixFlexibleTime(n int64) (time.Time, bool) {
	if n <= 0 {
		return time.Time{}, false
	}
	if n > 1_000_000_000_000 {
		return time.UnixMilli(n), true
	}
	return time.Unix(n, 0), true
}

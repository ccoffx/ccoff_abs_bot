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

func fetchABSCrossDayListeningSessions(absUserID string) ([]absListeningSession, error) {
	if abs.DefaultClient == nil || strings.TrimSpace(absUserID) == "" {
		return nil, fmt.Errorf("ABS \u4f1a\u8bdd\u8bfb\u53d6\u53c2\u6570\u4e0d\u5b8c\u6574")
	}

	sessions := make([]absListeningSession, 0, dailyListeningSessionPageSize)
	seenSessionKeys := make(map[string]struct{}, dailyListeningSessionPageSize)
	for page := 0; page < dailyListeningSessionMaxPages; page++ {
		body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningSessionsPath(absUserID, page, dailyListeningSessionPageSize), nil)
		if err != nil {
			return nil, err
		}
		if code != 200 {
			return nil, &abs.AbsAPIError{Operation: "\u8bfb\u53d6 ABS \u7528\u6237\u4f1a\u8bdd\u5931\u8d25", StatusCode: code, Message: "\u54cd\u5e94: " + abs.AbsResponseSnippet(body)}
		}
		rawSessions, err := parseABSSessionsPayload(body)
		if err != nil {
			return nil, err
		}
		for _, raw := range rawSessions {
			if !absSessionBelongsToUser(raw, absUserID) {
				continue
			}
			session, ok := parseABSListeningSession(raw, absUserID)
			if !ok {
				continue
			}
			key := strings.TrimSpace(session.SessionKey)
			if key == "" {
				continue
			}
			if _, exists := seenSessionKeys[key]; exists {
				continue
			}
			seenSessionKeys[key] = struct{}{}
			sessions = append(sessions, session)
		}
		if len(rawSessions) < dailyListeningSessionPageSize {
			break
		}
	}
	return sessions, nil
}

func persistAndLoadABSCrossDaySessions(userID int64, absUserID string, days map[string]float64, sessions []absListeningSession, observedAt time.Time) ([]absListeningSession, error) {
	if storage.DB == nil {
		return nil, fmt.Errorf("ABS_CROSS_DAY_DB_EMPTY")
	}
	if userID == 0 || strings.TrimSpace(absUserID) == "" {
		return nil, fmt.Errorf("ABS_CROSS_DAY_USER_EMPTY")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	observations, diagnostics := buildABSCrossDaySessionObservations(userID, absUserID, sessions, observedAt)
	if err := upsertABSCrossDaySessionObservations(storage.DB, observations); err != nil {
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

	sourceDays := make([]string, 0, len(days))
	for dayKey := range days {
		dayKey = strings.TrimSpace(dayKey)
		if validDailyListeningDayKey(dayKey) {
			sourceDays = append(sourceDays, dayKey)
		}
	}
	if len(sourceDays) == 0 {
		return nil, nil
	}
	sort.Strings(sourceDays)
	var stored []model.ABSCrossDaySessionAdjustment
	if err := storage.DB.Where(
		"user_id = ? AND abs_user_id = ? AND status IN ? AND algorithm_version = ? AND source_day IN ?",
		userID, strings.TrimSpace(absUserID), []string{crossDayAdjustmentStatusAccepted, crossDayAdjustmentStatusBounded}, dailyListeningCrossDayAlgorithmVersion, sourceDays,
	).Order("started_at ASC, session_key ASC").Find(&stored).Error; err != nil {
		return nil, fmt.Errorf("load ABS cross-day sessions: %w", err)
	}

	result := make([]absListeningSession, 0, len(stored))
	for _, row := range stored {
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
		})
	}
	return result, nil
}

func buildABSCrossDaySessionObservations(userID int64, absUserID string, sessions []absListeningSession, observedAt time.Time) ([]model.ABSCrossDaySessionAdjustment, map[string]int) {
	observations := make([]model.ABSCrossDaySessionAdjustment, 0, len(sessions))
	diagnostics := make(map[string]int)
	for _, session := range sessions {
		status, confidence, reason, tracked := classifyABSCrossDaySession(session)
		if !tracked {
			continue
		}
		allocationJSON := ""
		switch status {
		case crossDayAdjustmentStatusAccepted:
			allocationJSON = crossDaySessionAllocationJSON(session)
		case crossDayAdjustmentStatusBounded:
			allocationJSON = crossDaySessionMinimumAllocationJSON(session)
		}
		diagnostics[status+":"+reason]++
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

func upsertABSCrossDaySessionObservations(tx *gorm.DB, observations []model.ABSCrossDaySessionAdjustment) error {
	if tx == nil {
		return fmt.Errorf("ABS_CROSS_DAY_DB_EMPTY")
	}
	if len(observations) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "abs_user_id"}, {Name: "session_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "source_day", "started_at", "session_updated_at", "listening_seconds",
			"allocation_json", "status", "confidence", "reason", "algorithm_version", "observed_at", "updated_at",
		}),
	}).CreateInBatches(&observations, 50).Error
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

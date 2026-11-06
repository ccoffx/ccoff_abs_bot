package game

import (
	"encoding/json"
	"log"
	"math"
	"sort"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/textfmt"
)

func logDailyListeningDiagnostic(event string, userID int64, absUserID string, fields map[string]any) {
	fields["event"], fields["user_id"], fields["abs_user_id"] = event, userID, absUserID
	body, err := json.Marshal(fields)
	if err != nil {
		log.Printf("ABS listening trace encode failed: user=%d event=%s err=%s", userID, event, textfmt.FormatPlainError(err))
		return
	}
	log.Printf("ABS listening trace: %s", body)
}

func logDailyListeningRefresh(userID int64, absUserID string, force bool, startedAt time.Time, statsDuration time.Duration, status int, outcome, failure string) {
	logDailyListeningDiagnostic("refresh", userID, absUserID, map[string]any{
		"started_at": startedAt, "duration_ms": time.Since(startedAt).Milliseconds(),
		"fetched_at": startedAt, "stats_duration_ms": statsDuration.Milliseconds(),
		"force": force, "http_status": status, "outcome": outcome, "error": failure,
	})
}

type listeningSessionDiagnostic struct {
	ID               string    `json:"id"`
	SourceDay        string    `json:"source_day"`
	StartedAt        time.Time `json:"started_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ListeningSeconds float64   `json:"listening_seconds"`
	UpdateAgeSeconds float64   `json:"update_age_seconds"`
	CrossDayStatus   string    `json:"cross_day_status,omitempty"`
	CrossDayReason   string    `json:"cross_day_reason,omitempty"`
}

func listeningSessionDiagnosticAt(session absListeningSession, now time.Time) listeningSessionDiagnostic {
	status, _, reason, _ := classifyABSCrossDaySession(session)
	age := 0.0
	if !session.UpdatedAt.IsZero() {
		age = math.Max(0, now.Sub(session.UpdatedAt).Seconds())
	}
	return listeningSessionDiagnostic{
		ID: session.SessionKey, SourceDay: session.DateKey,
		StartedAt: session.StartedAt, UpdatedAt: session.UpdatedAt,
		ListeningSeconds: abs.PositiveListeningSeconds(session.ListeningSeconds),
		UpdateAgeSeconds: age, CrossDayStatus: status, CrossDayReason: reason,
	}
}

func logDailyListeningObservation(userID int64, absUserID string, official, adjusted map[string]float64, corrected map[string]bool, sessions []absListeningSession, startedAt, fetchedAt time.Time, observationErr error) {
	now := time.Now()
	daySet := map[string]bool{SectDayKey(now): true, SectDayKey(now.AddDate(0, 0, -1)): true}
	for day := range corrected {
		daySet[day] = true
	}
	dayKeys := make([]string, 0, len(daySet))
	for day := range daySet {
		dayKeys = append(dayKeys, day)
	}
	sort.Strings(dayKeys)
	type dayDiagnostic struct {
		DayKey           string  `json:"day_key"`
		OfficialPresent  bool    `json:"official_present"`
		OfficialSeconds  float64 `json:"official_seconds"`
		AdjustedSeconds  float64 `json:"adjusted_seconds"`
		EffectiveHours   float64 `json:"effective_hours"`
		CrossDayAdjusted bool    `json:"cross_day_adjusted"`
	}
	days := make([]dayDiagnostic, 0, len(dayKeys))
	for _, day := range dayKeys {
		raw, present := official[day]
		seconds := abs.PositiveListeningSeconds(adjusted[day])
		days = append(days, dayDiagnostic{day, present, abs.PositiveListeningSeconds(raw), seconds, calculateSectEffectiveHoursFromSeconds(seconds), corrected[day]})
	}
	var latest *listeningSessionDiagnostic
	crossDay := make([]listeningSessionDiagnostic, 0)
	for _, session := range sessions {
		diagnostic := listeningSessionDiagnosticAt(session, now)
		if latest == nil || diagnostic.UpdatedAt.After(latest.UpdatedAt) {
			latest = &diagnostic
		}
		if diagnostic.CrossDayStatus != "" {
			crossDay = append(crossDay, diagnostic)
		}
	}
	sort.Slice(crossDay, func(i, j int) bool { return crossDay[i].UpdatedAt.After(crossDay[j].UpdatedAt) })
	crossDayCount := len(crossDay)
	if len(crossDay) > 20 {
		crossDay = crossDay[:20]
	}
	failure := ""
	if observationErr != nil {
		failure = textfmt.FormatPlainError(observationErr)
	}
	logDailyListeningDiagnostic("observation", userID, absUserID, map[string]any{
		"fetched_at": fetchedAt,
		"started_at": startedAt, "sessions_duration_ms": time.Since(startedAt).Milliseconds(),
		"sessions_count": len(sessions), "latest_session": latest,
		"cross_day_count": crossDayCount, "cross_day_sessions": crossDay,
		"days": days, "correction_ready": observationErr == nil, "error": failure,
	})
}

func logDailyListeningWrite(userID int64, absUserID string, previous, records []model.DailyListeningStat, fetchedAt time.Time, stale bool) {
	before := make(map[string]model.DailyListeningStat, len(previous))
	for _, stat := range previous {
		before[stat.DayKey] = stat
	}
	type dayChange struct {
		DayKey                 string    `json:"day_key"`
		PreviousPresent        bool      `json:"previous_present"`
		PreviousRawSeconds     float64   `json:"previous_raw_seconds"`
		RawSeconds             float64   `json:"raw_seconds"`
		PreviousEffectiveHours float64   `json:"previous_effective_hours"`
		EffectiveHours         float64   `json:"effective_hours"`
		PreviousFetchedAt      time.Time `json:"previous_fetched_at"`
		Source                 string    `json:"source"`
		Outcome                string    `json:"outcome"`
	}
	changes := make([]dayChange, 0)
	yesterday := SectDayKey(fetchedAt.AddDate(0, 0, -1))
	for _, stat := range records {
		old, exists := before[stat.DayKey]
		// Keep every recent observation and every change to an existing historic
		// day, without logging an entire imported history on first registration.
		if stat.DayKey < yesterday && (!exists || (old.RawSeconds == stat.RawSeconds && old.EffectiveHours == stat.EffectiveHours && old.Source == stat.Source)) {
			continue
		}
		outcome := "applied"
		if stale {
			outcome = "ignored_older_snapshot"
		}
		changes = append(changes, dayChange{stat.DayKey, exists, old.RawSeconds, stat.RawSeconds, old.EffectiveHours, stat.EffectiveHours, old.LastFetchedAt, stat.Source, outcome})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].DayKey < changes[j].DayKey })
	logDailyListeningDiagnostic("write", userID, absUserID, map[string]any{"fetched_at": fetchedAt, "days": changes})
}

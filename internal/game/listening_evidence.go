package game

import (
	"math"
	"time"

	"ccoff-abs-bot/internal/storage"
)

// guaranteedSessionSeconds returns the minimum playback that must fall inside
// the window, even if every possible pause occurred there. Unknown or invalid
// timestamps cannot establish playback evidence.
func guaranteedSessionSeconds(session absListeningSession, start, end time.Time) float64 {
	if !session.HasListeningTime || session.StartedAt.IsZero() || session.UpdatedAt.IsZero() ||
		!session.UpdatedAt.After(session.StartedAt) || session.ListeningSeconds <= 0 ||
		math.IsNaN(session.ListeningSeconds) || math.IsInf(session.ListeningSeconds, 0) {
		return 0
	}
	wall := session.UpdatedAt.Sub(session.StartedAt).Seconds()
	if session.ListeningSeconds > wall {
		return 0
	}
	if start.Before(session.StartedAt) {
		start = session.StartedAt
	}
	if end.After(session.UpdatedAt) {
		end = session.UpdatedAt
	}
	if !end.After(start) {
		return 0
	}
	overlap := end.Sub(start).Seconds()
	return math.Max(0, session.ListeningSeconds-(wall-overlap))
}

// Penalties use only session evidence, never the official source-day assignment
// or proportional estimates. Missing history can reduce this lower bound, but
// cannot inflate it. Only the explicitly requested scan dates are evaluated;
// an absent official days key cannot suppress otherwise proven playback.
func listeningAbuseProvenDays(sessions []absListeningSession, targetDays []string) map[string]float64 {
	days := make(map[string]float64)
	windows := make(map[string]time.Time, len(targetDays))
	for _, day := range targetDays {
		start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
		if err == nil {
			windows[day] = start
		}
	}
	seen := make(map[string]bool)
	for _, session := range sessions {
		if session.SessionKey == "" || seen[session.SessionKey] {
			continue
		}
		seen[session.SessionKey] = true
		// Avoid traversing an invalid or arbitrarily long timestamp range.
		if guaranteedSessionSeconds(session, session.StartedAt, session.UpdatedAt) <= 0 {
			continue
		}
		for day, start := range windows {
			days[day] += guaranteedSessionSeconds(session, start, start.AddDate(0, 0, 1))
		}
	}
	return days
}

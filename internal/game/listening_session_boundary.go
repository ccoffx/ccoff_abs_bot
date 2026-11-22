package game

import "time"

// ABS /users/:id/listening-sessions is ordered by updatedAt descending (not
// startedAt or source day). Once the page crosses the lower time bound, older
// pages cannot overlap the window, even for sessions spanning several days.
// Missing timestamps or unexpected ordering must never certify completeness.
func listeningSessionPageReachesBefore(raw []map[string]interface{}, before time.Time) bool {
	if len(raw) == 0 || before.IsZero() {
		return false
	}
	var previous time.Time
	for _, session := range raw {
		updated, ok := timeValue(session["updatedAt"])
		if !ok || updated.IsZero() || (!previous.IsZero() && updated.After(previous)) {
			return false
		}
		previous = updated
	}
	return previous.Before(before)
}

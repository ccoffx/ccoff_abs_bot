package game

import (
	"ccoff-abs-bot/internal/textfmt"
	"encoding/json"
	"time"

	"ccoff-abs-bot/internal/abs"
)

// Historical session-ledger tests exercise the retained evidence algorithms for
// abuse/retreats independently of the official-daily cultivation policy. This
// harness deliberately materializes estimated days for assertions only; the
// production abuse path never writes them into ordinary cultivation rows.
func refreshSessionEvidenceForTest(userID int64, absID string, force bool, abuseDays ...string) (map[string]float64, bool) {
	if len(abuseDays) > 0 {
		return refreshDailyListeningStatsFromABS(userID, absID, force, abuseDays...)
	}
	if !force {
		if days, ok := cachedDailyListeningDaysWithinTTL(userID, absID, time.Now()); ok {
			return days, true
		}
	}
	started := time.Now()
	outcome, failure := "updated", ""
	defer func() {
		logDailyListeningRefresh(userID, absID, force, started, time.Since(started), 200, outcome, failure)
	}()
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(absID), nil)
	if err != nil || code != 200 {
		return nil, false
	}
	var stats struct {
		Days map[string]float64 `json:"days"`
	}
	if json.Unmarshal(body, &stats) != nil || stats.Days == nil {
		return nil, false
	}
	days, corrected, _, validation, err := prepareDailyListeningDaysChecked(userID, absID, stats.Days, started)
	if err != nil {
		outcome, failure = "correction_failed", textfmt.FormatPlainError(err)
		return nil, false
	}
	if recordPreparedDailyListeningStatsWithReport(userID, absID, days, corrected, started, true, validation) != nil {
		return nil, false
	}
	return validation.Seconds, true
}

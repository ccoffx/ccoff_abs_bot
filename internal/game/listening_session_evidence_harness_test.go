package game

import (
	"encoding/json"
	"errors"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/textfmt"
)

// Historical session-ledger tests exercise the retained evidence algorithms for
// abuse/retreats independently of the official-daily cultivation policy. This
// harness deliberately materializes estimated days for assertions only.
func refreshSessionEvidenceForTest(userID int64, absID string, force bool, abuseDays ...string) (map[string]float64, bool) {
	if !force && len(abuseDays) == 0 {
		if days, ok := cachedDailyListeningDaysWithinTTL(userID, absID, time.Now()); ok {
			return days, true
		}
	}
	started := time.Now()
	outcome, failure := "updated", ""
	defer func() {
		logDailyListeningRefresh(userID, absID, force, started, time.Since(started), 200, outcome, failure)
	}()
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningDaysPath(absID, ""), nil)
	if err != nil || code != 200 {
		return nil, false
	}
	var stats struct {
		Days map[string]float64 `json:"days"`
	}
	if json.Unmarshal(body, &stats) != nil || stats.Days == nil {
		return nil, false
	}
	days, corrected, sessions, validation, err := prepareDailyListeningDaysChecked(userID, absID, stats.Days, started)
	if err != nil {
		outcome, failure = "correction_failed", textfmt.FormatPlainError(err)
		var mismatch *absCrossDaySnapshotMismatchError
		if len(abuseDays) > 0 && errors.As(err, &mismatch) && mismatch.OnlyMissingSourceDays &&
			recordSectRetreatListeningSessions(userID, absID, sessions) == nil {
			return listeningAbuseProvenDays(sessions, abuseDays), true
		}
		return nil, false
	}
	if len(abuseDays) > 0 {
		return listeningAbuseProvenDays(sessions, abuseDays), true
	}
	if recordPreparedDailyListeningStatsWithReport(userID, absID, days, corrected, started, true, validation) != nil {
		return nil, false
	}
	return validation.Seconds, true
}

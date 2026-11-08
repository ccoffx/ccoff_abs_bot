package game

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Keep only candidate cross-day IDs and their source-day totals, rather than
// persisting every session in a user's history during a repair scan.
type absCrossDayReconciliationEvidence struct {
	MissingSessions map[string]string  `json:"missing_sessions"`
	DaySeconds      map[string]float64 `json:"day_seconds"`
	Valid           bool               `json:"valid"`
}

func newABSCrossDayReconciliationEvidence() absCrossDayReconciliationEvidence {
	return absCrossDayReconciliationEvidence{MissingSessions: make(map[string]string), DaySeconds: make(map[string]float64), Valid: true}
}

func (e *absCrossDayReconciliationEvidence) addCandidate(key, day string) {
	e.MissingSessions[key] = day
	e.DaySeconds[day] = 0
}

func (e *absCrossDayReconciliationEvidence) observe(sessions []absListeningSession, rawCount int) {
	if len(sessions) != rawCount {
		e.Valid = false
	}
	for _, session := range sessions {
		delete(e.MissingSessions, session.SessionKey)
		day := strings.TrimSpace(session.DateKey)
		if !session.HasListeningTime || !validDailyListeningDayKey(day) || session.ListeningSeconds < 0 || math.IsNaN(session.ListeningSeconds) || math.IsInf(session.ListeningSeconds, 0) {
			e.Valid = false
			continue
		}
		if _, tracked := e.DaySeconds[day]; tracked {
			e.DaySeconds[day] += session.ListeningSeconds
		}
	}
}

func reconcileCompleteABSCrossDaySnapshotInTx(tx *gorm.DB, userID int64, absUserID string, days map[string]float64, snapshot absListeningSessionSnapshot) error {
	var rows []model.ABSCrossDaySessionAdjustment
	if err := tx.Where("user_id = ? AND abs_user_id = ? AND status IN ? AND algorithm_version = ?", userID, strings.TrimSpace(absUserID),
		[]string{crossDayAdjustmentStatusAccepted, crossDayAdjustmentStatusBounded, crossDayAdjustmentStatusSameDay, crossDayAdjustmentStatusUncertain}, dailyListeningCrossDayAlgorithmVersion).Find(&rows).Error; err != nil {
		return err
	}
	evidence := newABSCrossDayReconciliationEvidence()
	for _, row := range rows {
		evidence.addCandidate(row.SessionKey, row.SourceDay)
	}
	evidence.observe(snapshot.Sessions, len(snapshot.Sessions))
	return invalidateMissingABSCrossDaySessionsInTx(tx, userID, absUserID, days, evidence, snapshot.StartedAt, snapshot.StartedAt)
}

func invalidateMissingABSCrossDaySessionsInTx(tx *gorm.DB, userID int64, absUserID string, days map[string]float64, evidence absCrossDayReconciliationEvidence, startedAt, observedAt time.Time) error {
	if !evidence.Valid || len(evidence.MissingSessions) == 0 {
		return nil
	}
	keys := make([]string, 0, len(evidence.MissingSessions))
	for key := range evidence.MissingSessions {
		keys = append(keys, key)
	}
	var rows []model.ABSCrossDaySessionAdjustment
	if err := tx.Where("user_id = ? AND abs_user_id = ? AND session_key IN ? AND status IN ? AND algorithm_version = ?", userID, strings.TrimSpace(absUserID), keys,
		[]string{crossDayAdjustmentStatusAccepted, crossDayAdjustmentStatusBounded, crossDayAdjustmentStatusSameDay, crossDayAdjustmentStatusUncertain}, dailyListeningCrossDayAlgorithmVersion).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		// A concurrent request may have supplied newer evidence during the scan.
		// Absence is actionable only when the complete source-day sum agrees with
		// listening-stats; a temporarily empty/partial API response cannot erase it.
		if !row.ObservedAt.Before(startedAt) || row.SourceDay != evidence.MissingSessions[row.SessionKey] ||
			math.Abs(evidence.DaySeconds[row.SourceDay]-abs.PositiveListeningSeconds(days[row.SourceDay])) > 1e-6 {
			continue
		}
		status := crossDayAdjustmentStatusInvalidated
		if row.Status == crossDayAdjustmentStatusSameDay || row.Status == crossDayAdjustmentStatusUncertain {
			status = crossDayAdjustmentStatusDeleted
		}
		if err := tx.Model(&row).Updates(map[string]any{
			"status": status, "confidence": crossDayAdjustmentConfidenceUncertain,
			"reason": "missing_from_complete_history", "observed_at": observedAt,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func missingABSListeningSessionEvidence(days map[string]float64, snapshot absListeningSessionSnapshot, persisted []absListeningSession) absCrossDayReconciliationEvidence {
	totals, seen := make(map[string]float64), make(map[string]bool)
	for _, session := range snapshot.Sessions {
		seen[session.SessionKey] = true
	}
	active := latestDailyListeningSessions(persisted)
	for _, session := range active {
		totals[session.DateKey] += session.ListeningSeconds
	}
	evidence := newABSCrossDayReconciliationEvidence()
	for _, session := range active {
		if !seen[session.SessionKey] && totals[session.DateKey] > abs.PositiveListeningSeconds(days[session.DateKey])+1e-6 {
			evidence.addCandidate(session.SessionKey, session.DateKey)
		}
	}
	return evidence
}

func scheduleABSCrossDayReconciliation(userID int64, absUserID string, days map[string]float64, snapshot absListeningSessionSnapshot, persisted []absListeningSession) error {
	evidence := missingABSListeningSessionEvidence(days, snapshot, persisted)
	if len(evidence.MissingSessions) == 0 {
		return nil
	}
	return scheduleABSListeningReconciliationEvidence(userID, absUserID, evidence)
}

func scheduleABSListeningReconciliationEvidence(userID int64, absUserID string, evidence absCrossDayReconciliationEvidence) error {
	body, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCurrentListeningBindingInTx(tx, userID, absUserID); err != nil {
			return err
		}
		cursor := model.ABSCrossDaySessionScanCursor{UserID: userID, AbsUserID: strings.TrimSpace(absUserID), DailyValidationVersion: dailyListeningValidationVersion}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "abs_user_id"}}, DoNothing: true}).Create(&cursor).Error; err != nil {
			return err
		}
		if err := tx.Where("abs_user_id = ?", strings.TrimSpace(absUserID)).First(&cursor).Error; err != nil {
			return err
		}
		if cursor.UserID == userID && cursor.ReconcileStartedAt != nil && !cursor.Completed {
			return nil // Repeated manual refreshes must not restart an in-flight scan.
		}
		return tx.Model(&cursor).Updates(map[string]any{
			"daily_validation_version": dailyListeningValidationVersion,
			"user_id":                  userID, "next_page": 0, "pages_scanned": 0, "sessions_observed": 0,
			"completed": false, "completed_at": nil, "last_scanned_at": nil, "next_retry_at": nil, "last_error_code": "",
			"reconcile_started_at": time.Now(), "reconcile_evidence_json": string(body),
			"pending_observations_json": "",
		}).Error
	})
}

func fetchABSCrossDayReconciliationDays(absUserID string) (map[string]float64, error) {
	body, code, err := abs.DefaultClient.SendRequest("GET", abs.AbsUserListeningStatsPath(absUserID), nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("ABS_CROSS_DAY_RECONCILIATION_STATS_STATUS_%d", code)
	}
	var stats struct {
		Days map[string]float64 `json:"days"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, err
	}
	if stats.Days == nil {
		return nil, fmt.Errorf("ABS_LISTENING_DAYS_MISSING")
	}
	return stats.Days, nil
}

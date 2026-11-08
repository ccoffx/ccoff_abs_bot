package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
)

// Pending observations are limited to corrections of already known sessions,
// rather than a second copy of the entire listening history.
func mergeABSBackfillObservations(pending, page []model.ABSCrossDaySessionAdjustment) []model.ABSCrossDaySessionAdjustment {
	result := make([]model.ABSCrossDaySessionAdjustment, 0, len(pending)+len(page))
	indices := make(map[string]int)
	for _, rows := range [][]model.ABSCrossDaySessionAdjustment{pending, page} {
		for _, row := range rows {
			// Older scanners may have persisted unknown durations as uncertain
			// zeroes. They must not replace valid pending evidence during merge.
			if unknownABSListeningDuration(row) {
				continue
			}
			key := row.AbsUserID + "\x00" + row.SessionKey
			if i, ok := indices[key]; ok {
				old := result[i]
				if row.Status == crossDayAdjustmentStatusDeleted && row.Reason == "listening_cleared" && !row.ObservedAt.Before(old.ObservedAt) && row.SessionUpdatedAt.Before(old.SessionUpdatedAt) {
					row.SessionUpdatedAt = old.SessionUpdatedAt
				}
				if (old.Status == crossDayAdjustmentStatusDeleted || old.Status == crossDayAdjustmentStatusInvalidated) && row.ObservedAt.Before(old.ObservedAt) {
					continue
				}
				if old.SessionUpdatedAt.After(row.SessionUpdatedAt) || (old.SessionUpdatedAt.Equal(row.SessionUpdatedAt) && old.ObservedAt.After(row.ObservedAt)) {
					continue
				}
				result[i] = row
			} else {
				indices[key] = len(result)
				result = append(result, row)
			}
		}
	}
	return result
}

func splitABSBackfillReductions(previous, incoming []model.ABSCrossDaySessionAdjustment) (safe, deferred []model.ABSCrossDaySessionAdjustment) {
	oldByKey := make(map[string]model.ABSCrossDaySessionAdjustment, len(previous))
	for _, row := range previous {
		oldByKey[row.AbsUserID+"\x00"+row.SessionKey] = row
	}
	for _, row := range incoming {
		old, exists := oldByKey[row.AbsUserID+"\x00"+row.SessionKey]
		if exists && isABSListeningReduction(old, row) {
			deferred = append(deferred, row)
		} else {
			safe = append(safe, row)
		}
	}
	return safe, deferred
}

// Every page failure backs off, including statistics/validation/commit errors.
// If a complete pass is inconsistent, collect fresh evidence from page zero:
// later playback may have shifted pages while this pass was in progress.
func recordABSBackfillFailure(cursor model.ABSCrossDaySessionScanCursor, now time.Time, failure error) error {
	updateErr := storage.DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"next_retry_at":   now.Add(absCrossDayBackfillRetryDelay),
			"last_scanned_at": now,
			"last_error_code": textfmt.TruncateRunes(textfmt.FormatPlainError(failure), 120),
		}
		var mismatch *absDailyListeningSnapshotMismatchError
		if errors.As(failure, &mismatch) {
			var rows []model.ABSCrossDaySessionAdjustment
			if err := tx.Where("user_id = ? AND abs_user_id = ? AND algorithm_version = ?", cursor.UserID, cursor.AbsUserID, dailyListeningCrossDayAlgorithmVersion).Find(&rows).Error; err != nil {
				return err
			}
			evidence := newABSCrossDayReconciliationEvidence()
			for _, row := range rows {
				if activeABSListeningObservation(row) {
					evidence.addCandidate(row.SessionKey, row.SourceDay)
				}
			}
			evidence.DaySeconds[mismatch.Day] = 0
			body, err := json.Marshal(evidence)
			if err != nil {
				return err
			}
			updates["next_page"], updates["pages_scanned"], updates["sessions_observed"] = 0, 0, 0
			updates["pending_observations_json"] = ""
			updates["reconcile_started_at"], updates["reconcile_evidence_json"] = now, string(body)
		}
		return tx.Model(&model.ABSCrossDaySessionScanCursor{}).
			Where("id = ? AND user_id = ? AND abs_user_id = ? AND next_page = ? AND completed = ?", cursor.ID, cursor.UserID, cursor.AbsUserID, cursor.NextPage, false).
			Where("reconcile_started_at IS ?", cursor.ReconcileStartedAt).Updates(updates).Error
	})
	if updateErr != nil {
		return fmt.Errorf("backfill=%v cursor_update=%w", failure, updateErr)
	}
	return failure
}

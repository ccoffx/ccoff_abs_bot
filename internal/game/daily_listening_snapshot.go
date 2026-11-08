package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
	"gorm.io/gorm"
)

var errABSListeningStatsRequired = errors.New("ABS_LISTENING_STATS_REQUIRED_FOR_CORRECTION")

type absDailyListeningSnapshotMismatchError struct {
	Day                                    string
	Official, Observed, PreviousUnobserved float64
}

func (e *absDailyListeningSnapshotMismatchError) Error() string {
	return fmt.Sprintf("ABS_DAILY_SNAPSHOT_MISMATCH: day=%s stats_seconds=%.3f observed_seconds=%.3f previous_unobserved_seconds=%.3f", e.Day, e.Official, e.Observed, e.PreviousUnobserved)
}

// A remainder can also grow because newer playback is outside the recent
// pages. Revisit history without resetting an already running repair scan.
func scheduleABSListeningSnapshotRepair(userID int64, absID string, snapshot absListeningSessionSnapshot, mismatch error) {
	var detail *absDailyListeningSnapshotMismatchError
	if snapshot.Complete || !errors.As(mismatch, &detail) {
		return
	}
	evidence := newABSCrossDayReconciliationEvidence()
	evidence.DaySeconds[detail.Day] = 0
	if err := scheduleABSListeningReconciliationEvidence(userID, absID, evidence); err != nil {
		log.Printf("ABS daily snapshot repair schedule failed: user=%d err=%s", userID, textfmt.FormatPlainError(err))
	}
}

func activeABSListeningObservation(row model.ABSCrossDaySessionAdjustment) bool {
	return row.Status != crossDayAdjustmentStatusDeleted && row.Status != crossDayAdjustmentStatusInvalidated &&
		validDailyListeningDayKey(row.SourceDay) && finitePositiveListeningSeconds(row.ListeningSeconds)
}

func isABSListeningReduction(old, current model.ABSCrossDaySessionAdjustment) bool {
	return old.UserID == current.UserID && activeABSListeningObservation(old) &&
		(!activeABSListeningObservation(current) || current.SourceDay != old.SourceDay || current.ListeningSeconds+1e-6 < old.ListeningSeconds)
}

// A known reduction must not turn the previous session's budget into unseen
// history. Verify it before replacing the observation, so a failed check can
// be retried with the same evidence after listening-stats catches up.
func validateABSListeningReductionsInTx(tx *gorm.DB, previous, incoming []model.ABSCrossDaySessionAdjustment, budgets []map[string]float64) error {
	oldByKey := make(map[string]model.ABSCrossDaySessionAdjustment, len(previous))
	for _, row := range previous {
		oldByKey[row.AbsUserID+"\x00"+row.SessionKey] = row
	}
	type identity struct {
		user int64
		abs  string
	}
	affected := make(map[identity]map[string]bool)
	for _, row := range incoming {
		old, exists := oldByKey[row.AbsUserID+"\x00"+row.SessionKey]
		if !exists || !isABSListeningReduction(old, row) {
			continue
		}
		id := identity{row.UserID, row.AbsUserID}
		if affected[id] == nil {
			affected[id] = make(map[string]bool)
		}
		affected[id][old.SourceDay] = true
	}
	if len(affected) == 0 {
		return nil
	}
	if len(budgets) == 0 || budgets[0] == nil {
		return errABSListeningStatsRequired
	}
	for id, days := range affected {
		var rows []model.ABSCrossDaySessionAdjustment
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND algorithm_version = ?", id.user, id.abs, dailyListeningCrossDayAlgorithmVersion).Find(&rows).Error; err != nil {
			return err
		}
		before, after := make(map[string]float64), make(map[string]float64)
		for _, row := range rows {
			if activeABSListeningObservation(row) {
				before[row.SourceDay] += row.ListeningSeconds
				after[row.SourceDay] += row.ListeningSeconds
			}
		}
		for _, row := range incoming {
			if row.UserID != id.user || row.AbsUserID != id.abs {
				continue
			}
			if old, exists := oldByKey[row.AbsUserID+"\x00"+row.SessionKey]; exists && old.UserID == id.user && activeABSListeningObservation(old) {
				after[old.SourceDay] -= old.ListeningSeconds
			}
			if activeABSListeningObservation(row) {
				after[row.SourceDay] += row.ListeningSeconds
			}
		}
		keys := make([]string, 0, len(days))
		for day := range days {
			keys = append(keys, day)
		}
		var stats []model.DailyListeningStat
		if err := tx.Where("user_id = ? AND abs_user_id = ? AND day_key IN ?", id.user, id.abs, keys).Find(&stats).Error; err != nil {
			return err
		}
		lastOfficial := make(map[string]float64)
		for _, stat := range stats {
			if stat.ValidationJSON != "" {
				lastOfficial[stat.DayKey] = abs.PositiveListeningSeconds(stat.OfficialRawSeconds)
			}
		}
		for day := range days {
			current := abs.PositiveListeningSeconds(budgets[0][day])
			prior, exists := lastOfficial[day]
			if !exists {
				prior = current
			}
			unknownBefore := math.Max(0, prior-before[day])
			unknownAfter := math.Max(0, current-after[day])
			if unknownAfter > unknownBefore+1e-6 {
				return &absDailyListeningSnapshotMismatchError{day, current, after[day], unknownBefore}
			}
		}
	}
	return nil
}

// Once a correction is committed, a subsequent stale statistics response must
// not recreate the retired budget. Compare unseen source-day remainders using
// the previous successful snapshot, while allowing newly observed playback.
func validateABSListeningSourceRemainders(userID int64, absID string, official map[string]float64, sessions []absListeningSession) error {
	affected := make(map[string]bool)
	for _, session := range sessions {
		if (session.CrossDayStatus == crossDayAdjustmentStatusDeleted || session.CrossDayStatus == crossDayAdjustmentStatusInvalidated) && validDailyListeningDayKey(session.DateKey) {
			affected[session.DateKey] = true
		}
		for _, day := range session.RetiredDays {
			if validDailyListeningDayKey(day) {
				affected[day] = true
			}
		}
	}
	if len(affected) == 0 {
		return nil
	}
	keys := make([]string, 0, len(affected))
	for day := range affected {
		keys = append(keys, day)
	}
	var stats []model.DailyListeningStat
	if err := storage.DB.Where("user_id = ? AND abs_user_id = ? AND day_key IN ?", userID, absID, keys).Find(&stats).Error; err != nil {
		return err
	}
	observed := make(map[string]float64)
	for _, session := range latestDailyListeningSessions(sessions) {
		observed[session.DateKey] += session.ListeningSeconds
	}
	for _, stat := range stats {
		var detail dailyListeningValidationDetail
		if stat.ValidationJSON == "" {
			continue
		}
		if err := json.Unmarshal([]byte(stat.ValidationJSON), &detail); err != nil {
			return fmt.Errorf("decode prior ABS listening validation: %w", err)
		}
		if detail.ObservedSourceSeconds == nil {
			continue // Legacy snapshots did not record the source-day coverage.
		}
		unknownBefore := math.Max(0, stat.OfficialRawSeconds-*detail.ObservedSourceSeconds)
		current := abs.PositiveListeningSeconds(official[stat.DayKey])
		if math.Max(0, current-observed[stat.DayKey]) > unknownBefore+1e-6 {
			return &absDailyListeningSnapshotMismatchError{stat.DayKey, current, observed[stat.DayKey], unknownBefore}
		}
	}
	return nil
}

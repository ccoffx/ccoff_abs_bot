package game

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

// Session key -> Beijing day -> proven seconds. Keeping the strongest observed
// lower bound prevents a later, longer session snapshot from erasing playback
// already proven inside the retreat, without counting that session twice.
type sectRetreatListeningEvidence map[string]map[string]float64

func decodeSectRetreatEvidence(retreat model.SectCaveRetreat) (sectRetreatListeningEvidence, error) {
	evidence := make(sectRetreatListeningEvidence)
	if retreat.ListeningEvidenceJSON != "" {
		if err := json.Unmarshal([]byte(retreat.ListeningEvidenceJSON), &evidence); err != nil {
			return nil, fmt.Errorf("retreat %d listening evidence: %w", retreat.ID, err)
		}
	}
	if evidence == nil {
		evidence = make(sectRetreatListeningEvidence)
	}
	for _, days := range evidence {
		for day, seconds := range days {
			if !validDailyListeningDayKey(day) || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
				return nil, fmt.Errorf("retreat %d invalid listening evidence", retreat.ID)
			}
		}
	}
	return evidence, nil
}

// Observations may arrive after the retreat ends: only mathematically certain
// playback inside its fixed window earns a bonus. Evidence is persisted with
// session identity so refreshes and pagination cannot award it twice.
func recordSectRetreatListeningSessions(userID int64, absUserID string, incoming []absListeningSession) error {
	return storage.DB.Transaction(func(tx *gorm.DB) error {
		return recordSectRetreatListeningSessionsInTx(tx, userID, absUserID, incoming)
	})
}

// Historical scanning shares this transaction with cross-day observations and
// cursor advancement: a page is never marked consumed before its evidence is saved.
func recordSectRetreatListeningSessionsInTx(tx *gorm.DB, userID int64, absUserID string, incoming []absListeningSession) error {
	var retreats []model.SectCaveRetreat
	if err := tx.Where("user_id = ?", userID).Find(&retreats).Error; err != nil {
		return err
	}
	for _, retreat := range retreats {
		evidence, err := decodeSectRetreatEvidence(retreat)
		if err != nil {
			return err
		}
		changed := false
		for _, session := range incoming {
			if session.SessionKey == "" || !session.StartedAt.Before(retreat.EndAt) || !session.UpdatedAt.After(retreat.StartAt) {
				continue
			}
			key := absUserID + ":" + session.SessionKey
			for day, seconds := range provenSectRetreatSessionDays(retreat, session) {
				if seconds <= evidence[key][day] {
					continue
				}
				if evidence[key] == nil {
					evidence[key] = make(map[string]float64)
				}
				evidence[key][day] = seconds
				changed = true
			}
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		if err := tx.Model(&model.SectCaveRetreat{}).Where("id = ?", retreat.ID).UpdateColumn("listening_evidence_json", string(encoded)).Error; err != nil {
			return err
		}
	}
	return nil
}

func provenSectRetreatSessionDays(retreat model.SectCaveRetreat, session absListeningSession) map[string]float64 {
	days := make(map[string]float64)
	for day := range splitDurationByBeijingDay(retreat.StartAt, retreat.EndAt) {
		start, err := time.ParseInLocation("2006-01-02", day, storage.DailyOperationsLocation)
		if err != nil {
			continue
		}
		end := start.AddDate(0, 0, 1)
		if start.Before(retreat.StartAt) {
			start = retreat.StartAt
		}
		if end.After(retreat.EndAt) {
			end = retreat.EndAt
		}
		if seconds := guaranteedSessionSeconds(session, start, end); seconds > 0 {
			days[day] = seconds
		}
	}
	return days
}

func provenSectRetreatSecondsForDay(retreat model.SectCaveRetreat, day time.Time) (float64, error) {
	evidence, err := decodeSectRetreatEvidence(retreat)
	if err != nil {
		return 0, err
	}
	start, end := retreat.StartAt, retreat.EndAt
	if start.Before(day) {
		start = day
	}
	if next := day.AddDate(0, 0, 1); end.After(next) {
		end = next
	}
	if !end.After(start) {
		return 0, nil
	}
	total := 0.0
	for _, days := range evidence {
		total += days[SectDayKey(day)]
	}
	return math.Min(total, end.Sub(start).Seconds()), nil
}

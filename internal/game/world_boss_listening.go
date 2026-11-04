package game

import (
	"fmt"
	"time"

	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
)

func worldBossListeningHours(p *model.WorldBossParticipant, event model.WorldBossEvent, absID string, now time.Time) (float64, bool, error) {
	if p.AbsUserID == "" || p.AbsUserID != absID {
		return 0, false, fmt.Errorf("WORLD_BOSS_ABS_BINDING_CHANGED")
	}
	start, end := p.CreatedAt, now
	if start.Before(event.StartAt) {
		start = event.StartAt
	}
	if end.After(event.EndAt) {
		end = event.EndAt
	}
	cutoff := event.EndAt
	if event.ListeningCutoffAt != nil && event.ListeningCutoffAt.Before(cutoff) {
		cutoff = *event.ListeningCutoffAt
	}
	if end.After(cutoff) {
		end = cutoff
	}
	final := !now.Before(cutoff)
	state := listeningWindowProgress{p.ListeningEvidenceJSON, p.ListeningScanPage, p.FinalListeningScanStarted, p.FinalListeningScanComplete}
	seconds, complete, fetchErr := scanListeningWindow(&state, absID, start, end, final)
	p.ListeningEvidenceJSON, p.ListeningScanPage = state.ListeningEvidenceJSON, state.ListeningScanPage
	p.FinalListeningScanStarted, p.FinalListeningScanComplete = state.FinalListeningScanStarted, state.FinalListeningScanComplete
	res := storage.DB.Model(&model.WorldBossParticipant{}).Where("id = ? AND user_id = ?", p.ID, p.UserID).Updates(map[string]interface{}{
		"abs_user_id": absID, "listening_evidence_json": p.ListeningEvidenceJSON, "listening_scan_page": p.ListeningScanPage,
		"final_listening_scan_started": p.FinalListeningScanStarted, "final_listening_scan_complete": p.FinalListeningScanComplete,
	})
	if res.Error != nil {
		return 0, false, res.Error
	}
	if res.RowsAffected != 1 {
		return 0, false, fmt.Errorf("WORLD_BOSS_EVIDENCE_WRITE_MISSED")
	}
	return seconds / 3600, complete, fetchErr
}

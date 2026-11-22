package game

import (
	"ccoff-abs-bot/internal/model"
	"fmt"
	"time"
)

func worldBossListeningHours(p *model.WorldBossParticipant, event model.WorldBossEvent, absID string, now time.Time) (float64, bool, error) {
	if p.AbsUserID == "" || p.AbsUserID != absID {
		return 0, false, fmt.Errorf("WORLD_BOSS_ABS_BINDING_CHANGED")
	}
	start, cutoff := p.CreatedAt, event.EndAt
	if start.Before(event.StartAt) || start.IsZero() {
		start = event.StartAt
	}
	if event.ListeningCutoffAt != nil && event.ListeningCutoffAt.Before(cutoff) {
		cutoff = *event.ListeningCutoffAt
	}
	final := event.Status == "settling"
	seconds, err := captureActivityListeningSnapshot("world_boss_participants", "world_boss_events", "boss_id", event.BossID, p.ID, absID,
		start, cutoff, now, p.DeltaHours*3600, &p.ListeningSnapshot, final)
	return seconds / 3600, final && err == nil, err
}

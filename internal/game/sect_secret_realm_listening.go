package game

import (
	"ccoff-abs-bot/internal/model"
	"fmt"
	"time"
)

func sectSecretRealmListeningSeconds(p *model.SectSecretRealmParticipant, event model.SectSecretRealmEvent, absID string, now time.Time) (float64, bool, error) {
	if p.AbsUserID == "" || p.AbsUserID != absID {
		return 0, false, fmt.Errorf("SECT_SECRET_REALM_ABS_BINDING_CHANGED")
	}
	start := p.CreatedAt
	if start.Before(event.StartAt) || start.IsZero() {
		start = event.StartAt
	}
	final := event.Status == "settling"
	seconds, err := captureActivityListeningSnapshot("sect_secret_realm_participants", "sect_secret_realm_events", "realm_id", event.RealmID, p.ID, absID,
		start, event.EndAt, now, p.RawDeltaSeconds, &p.ListeningSnapshot, final)
	return seconds, final && err == nil, err
}

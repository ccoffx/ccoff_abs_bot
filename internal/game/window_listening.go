package game

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

type listeningWindowProgress struct {
	ListeningEvidenceJSON      string
	ListeningScanPage          int
	FinalListeningScanStarted  bool
	FinalListeningScanComplete bool
}

// Keep per-session proven lower bounds and a resumable history cursor. A
// delayed settlement must finish scanning history before paying any rewards.
func scanListeningWindow(p *listeningWindowProgress, absID string, start, end time.Time, final bool) (float64, bool, error) {
	evidence := make(map[string]float64)
	if p.ListeningEvidenceJSON != "" {
		if err := json.Unmarshal([]byte(p.ListeningEvidenceJSON), &evidence); err != nil {
			return 0, false, err
		}
	}
	if evidence == nil {
		evidence = make(map[string]float64)
	}
	for _, seconds := range evidence {
		if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0, false, fmt.Errorf("LISTENING_WINDOW_INVALID_EVIDENCE")
		}
	}
	page := 0
	if final {
		if !p.FinalListeningScanStarted {
			p.ListeningScanPage = 0
			p.FinalListeningScanStarted = true
			p.FinalListeningScanComplete = false
		}
		page = p.ListeningScanPage
	}
	var fetchErr error
	if !final || !p.FinalListeningScanComplete {
		// A delayed settlement can have arbitrarily many newer sessions. The
		// persisted cursor continues beyond the recent five pages on retries.
		for fetched := 0; fetched < dailyListeningSessionMaxPages; fetched++ {
			sessions, count, err := fetchABSCrossDayListeningSessionPage(absID, page)
			if err != nil {
				fetchErr = err
				break
			}
			for _, session := range sessions {
				seconds := rewardSessionSeconds(session, start, end)
				if seconds > evidence[session.SessionKey] {
					evidence[session.SessionKey] = seconds
				}
			}
			page++
			if final {
				p.ListeningScanPage = page
			}
			if count < dailyListeningSessionPageSize {
				if final {
					p.FinalListeningScanComplete = true
				}
				break
			}
		}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return 0, false, err
	}
	p.ListeningEvidenceJSON = string(encoded)
	seconds := 0.0
	for _, amount := range evidence {
		seconds += amount
	}
	window := math.Max(0, end.Sub(start).Seconds())
	return math.Min(seconds, window), p.FinalListeningScanComplete, fetchErr
}

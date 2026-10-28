package game

import "testing"

func TestHistoricalSessionsRebalanceAcrossBeijingMidnight(t *testing.T) {
	for _, playing := range []bool{false, true} {
		session, ok := parseABSListeningSession(map[string]interface{}{
			"id": "session-1", "libraryItemId": "book-1",
			"date": "2026-10-27", "timeListening": float64(3600),
			"startedAt": "2026-10-27T23:30:00+08:00",
			"updatedAt": "2026-10-28T00:30:00+08:00",
			"isPlaying": playing, "currentTime": float64(999999),
		}, "abs-user")
		if !ok {
			t.Fatal("historical session was rejected")
		}
		days := map[string]float64{"2026-10-27": 3600}
		// Duplicate observations must not count the session twice. Playback
		// position and current state cannot change official listening totals.
		adjusted, corrected := rebalanceABSDaysForCrossDaySessions(days, []absListeningSession{session, session})
		if adjusted["2026-10-27"] != 1800 || adjusted["2026-10-28"] != 1800 {
			t.Fatalf("playing=%t: unexpected daily allocation: %v", playing, adjusted)
		}
		if !corrected["2026-10-27"] || !corrected["2026-10-28"] || days["2026-10-27"] != 3600 {
			t.Fatal("correction flags or original ABS data changed unexpectedly")
		}
	}
}

func TestSessionPlaybackPositionDoesNotCreateListeningTime(t *testing.T) {
	session, _ := parseABSListeningSession(map[string]interface{}{
		"id": "session-1", "date": "2026-10-27",
		"startedAt":   "2026-10-27T23:30:00+08:00",
		"updatedAt":   "2026-10-28T00:30:00+08:00",
		"currentTime": float64(3600), "isPlaying": true,
	}, "abs-user")
	adjusted, corrected := rebalanceABSDaysForCrossDaySessions(
		map[string]float64{"2026-10-27": 3600}, []absListeningSession{session},
	)
	if adjusted["2026-10-27"] != 3600 || adjusted["2026-10-28"] != 0 || len(corrected) != 0 {
		t.Fatalf("playback position changed official totals: %v, %v", adjusted, corrected)
	}
}

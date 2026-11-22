package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Represent a large account whose first page already crosses the activity
// boundary. Older pages exist, but fetching any of them is unnecessary.
func activityBoundaryPage(start, end time.Time) []any {
	sessions := []any{
		bossSession("after", end, end.Add(time.Hour), 3600),
		bossSession("cross-start", start.Add(-time.Hour), end, end.Sub(start.Add(-time.Hour)).Seconds()),
	}
	for i := len(sessions); i < dailyListeningSessionPageSize; i++ {
		oldEnd := start.Add(-time.Duration(i) * time.Hour)
		sessions = append(sessions, bossSession(fmt.Sprintf("old-%d", i), oldEnd.Add(-time.Minute), oldEnd, 60))
	}
	return sessions
}

func TestListeningSessionBoundaryRequiresOrderedKnownUpdateTimes(t *testing.T) {
	before := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		updates []any
		want    bool
	}{
		{"crossing", []any{before.Add(time.Hour), before.Add(-time.Hour)}, true},
		{"all recent", []any{before.Add(time.Hour), before}, false},
		{"unordered", []any{before.Add(-time.Hour), before.Add(time.Hour), before.Add(-2 * time.Hour)}, false},
		{"missing timestamp", []any{nil, before.Add(-time.Hour)}, false},
		{"milliseconds", []any{float64(before.Add(-time.Hour).UnixMilli())}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]map[string]any, len(tc.updates))
			for i, updated := range tc.updates {
				raw[i] = map[string]any{"updatedAt": updated}
			}
			if got := listeningSessionPageReachesBefore(raw, before); got != tc.want {
				t.Fatalf("boundary=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestPeriodicListeningSnapshotStopsAtRecentWindowWithoutClaimingFullHistory(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": activityBoundaryPage(now.Add(-96*time.Hour), now)})
	})
	snapshot, err := fetchABSCrossDayListeningSessions("abs1")
	if err != nil || calls.Load() != 1 || snapshot.Complete || len(snapshot.Sessions) != dailyListeningSessionPageSize {
		t.Fatalf("recent snapshot: calls=%d complete=%v sessions=%d err=%v", calls.Load(), snapshot.Complete, len(snapshot.Sessions), err)
	}
}

package game

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLeaderboardBooksScansBeyondRecentAndFivePageLimits(t *testing.T) {
	for _, period := range []string{"daily", "weekly", "monthly"} {
		t.Run(period, func(t *testing.T) {
			sessions := make([]map[string]any, 0, 613)
			for i := 0; i < 612; i++ {
				day, title := "2026-12-01", "outside period"
				if i >= 600 {
					day, title = "2026-11-13", "older book"
				}
				sessions = append(sessions, map[string]any{"id": fmt.Sprint(i), "date": day, "timeListening": 600, "mediaMetadata": map[string]string{"title": title}})
			}
			// Pagination overlap must not duplicate a listen or its seconds.
			sessions = append(sessions, sessions[600])
			var requests atomic.Int64
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/listening-sessions") {
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				if err != nil {
					t.Error(err)
				}
				if r.URL.Query().Get("itemsPerPage") != "100" {
					t.Error("unexpected page size")
				}
				requests.Add(1)
				start := page * 100
				if start > len(sessions) {
					t.Errorf("unexpected page %d", page)
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions[start:min(start+100, len(sessions))]})
			})
			books, err := fetchLeaderboardBookStats("abs1", period, map[string]bool{"2026-11-13": true}, "2026-11")
			if err != nil {
				t.Fatal(err)
			}
			book := books["older book"]
			if len(books) != 1 || book == nil || book.ListenCount != 12 || book.TotalTime != 7200 || requests.Load() != 7 {
				t.Fatalf("history missing or duplicated: books=%+v book=%+v requests=%d", books, book, requests.Load())
			}
		})
	}
}

func TestLeaderboardBooksDiscardsPartialOrStalledHistory(t *testing.T) {
	for _, failure := range []string{"later_page_fails", "repeated_page", "missing_sessions", "invalid_seconds"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int64
			bossEvidenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				page := requests.Add(1) - 1
				if page > 2 {
					t.Error("pagination did not stop")
					w.WriteHeader(500)
					return
				}
				if failure == "later_page_fails" && page == 1 {
					w.WriteHeader(503)
					return
				}
				if failure == "missing_sessions" {
					fmt.Fprint(w, `{}`)
					return
				}
				if failure == "invalid_seconds" {
					fmt.Fprint(w, `{"sessions":[{"id":"bad","date":"2026-11-13","timeListening":-10}]}`)
					return
				}
				sessions := make([]map[string]any, 100)
				for i := range sessions {
					sessions[i] = map[string]any{"id": fmt.Sprint(i), "date": "2026-11-13", "timeListening": 60, "mediaMetadata": map[string]string{"title": "partial book"}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
			})
			books, err := fetchLeaderboardBookStats("abs1", "monthly", nil, "2026-11")
			if err == nil || books != nil {
				t.Fatalf("incomplete history escaped: books=%+v err=%v", books, err)
			}
		})
	}
}

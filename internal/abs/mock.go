package abs

// MockTransport implements the ABS HTTP boundary in-process. It has no fallback
// transport, so even an unknown URL or endpoint cannot contact a real server.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mockUser struct {
	ID           string             `json:"id"`
	Username     string             `json:"username"`
	PasswordHash string             `json:"passwordHash"`
	Active       bool               `json:"isActive"`
	Days         map[string]float64 `json:"days"`
}
type mockState struct {
	Version int                  `json:"version"`
	NextID  int                  `json:"nextId"`
	Failure string               `json:"failure"`
	Users   map[string]*mockUser `json:"users"`
}
type MockTransport struct {
	mu    sync.Mutex
	path  string
	state mockState
}

func OpenMockTransport(path string) (*MockTransport, error) {
	m := &MockTransport{path: path, state: mockState{Version: 1, Users: map[string]*mockUser{}}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &m.state); err != nil {
			return nil, fmt.Errorf("invalid mock ABS state: %w", err)
		}
		if m.state.Version != 1 || m.state.Users == nil {
			return nil, fmt.Errorf("unsupported mock ABS state")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if m.state.Users["mock-template"] == nil {
		m.state.Users["mock-template"] = &mockUser{ID: "mock-template", Username: "mock_template", Active: true, Days: map[string]float64{}}
		if err := m.save(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func mockPassword(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (m *MockTransport) save() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".mock-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), m.path)
}

// mutate restores in-memory state if durable persistence fails.
func (m *MockTransport) mutate(fn func() error) error {
	before, _ := json.Marshal(m.state)
	if err := fn(); err != nil {
		var restored mockState
		_ = json.Unmarshal(before, &restored)
		m.state = restored
		return err
	}
	if err := m.save(); err != nil {
		var restored mockState
		_ = json.Unmarshal(before, &restored)
		m.state = restored
		return err
	}
	return nil
}

func (m *MockTransport) SeedUser(telegramID int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("mock-tg-%d", telegramID)
	err := m.mutate(func() error {
		if m.state.Users[id] == nil {
			m.state.Users[id] = &mockUser{ID: id, Username: fmt.Sprintf("demo%d", telegramID), PasswordHash: mockPassword("TestPassword123!"), Active: true, Days: map[string]float64{}}
		}
		return nil
	})
	return id, err
}

func (m *MockTransport) SetListening(id, day string, hours float64) error {
	if parsed, err := time.Parse("2006-01-02", day); err != nil || parsed.Format("2006-01-02") != day {
		return fmt.Errorf("date must be YYYY-MM-DD")
	}
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours < 0 || hours > 24 {
		return fmt.Errorf("hours must be between 0 and 24")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mutate(func() error {
		u := m.state.Users[id]
		if u == nil {
			return fmt.Errorf("mock user not found; run /test seed first")
		}
		if u.Days == nil {
			u.Days = map[string]float64{}
		}
		u.Days[day] = hours * 3600
		return nil
	})
}

func (m *MockTransport) SetFailure(mode string) error {
	if mode != "off" && mode != "timeout" && mode != "500" && mode != "invalid-json" {
		return fmt.Errorf("failure must be off, timeout, 500 or invalid-json")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mutate(func() error { m.state.Failure = mode; return nil })
}

func mockUserResponse(u *mockUser) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "type": "user", "isActive": u.Active, "permissions": map[string]any{"download": true, "update": false, "delete": false}, "librariesAccessible": []string{}, "mediaProgress": []any{}}
}

func (m *MockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if r.URL.Scheme != "https" || r.URL.Host != "abs.mock.invalid" {
		return nil, fmt.Errorf("mock ABS rejects external URL")
	}
	if m.state.Failure == "timeout" {
		return nil, &mockTimeout{}
	}
	if m.state.Failure == "500" {
		return mockResponse(r, 500, map[string]any{"error": "simulated ABS failure"}), nil
	}
	if m.state.Failure == "invalid-json" {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("{invalid")), Request: r}, nil
	}
	code, result := m.handle(r)
	return mockResponse(r, code, result), nil
}

type mockTimeout struct{}

func (*mockTimeout) Error() string   { return "simulated ABS timeout" }
func (*mockTimeout) Timeout() bool   { return true }
func (*mockTimeout) Temporary() bool { return true }

func mockResponse(r *http.Request, code int, result any) *http.Response {
	data, _ := json.Marshal(result)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}
}

func (m *MockTransport) handle(r *http.Request) (int, any) {
	path := strings.Trim(r.URL.Path, "/")
	var payload struct {
		Username string  `json:"username"`
		Password *string `json:"password"`
		Active   *bool   `json:"isActive"`
	}
	if r.Body != nil && (r.Method == "POST" || r.Method == "PATCH") {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			return 400, map[string]any{"error": "invalid request JSON"}
		}
	}
	if path == "login" && r.Method == "POST" {
		for _, u := range m.state.Users {
			if payload.Password != nil && u.Username == payload.Username && u.PasswordHash == mockPassword(*payload.Password) && u.Active {
				return 200, map[string]any{"user": mockUserResponse(u)}
			}
		}
		return 401, map[string]any{"error": "invalid credentials"}
	}
	if path == "api/users" {
		if r.Method == "GET" {
			ids := make([]string, 0, len(m.state.Users))
			for id := range m.state.Users {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			users := []any{}
			for _, id := range ids {
				users = append(users, mockUserResponse(m.state.Users[id]))
			}
			return 200, users
		}
		if r.Method == "POST" {
			if payload.Username == "" || payload.Password == nil || *payload.Password == "" {
				return 400, map[string]any{"error": "username/password required"}
			}
			for _, u := range m.state.Users {
				if u.Username == payload.Username {
					return 409, map[string]any{"error": "username exists"}
				}
			}
			var id string
			if err := m.mutate(func() error {
				m.state.NextID++
				id = "mock-user-" + strconv.Itoa(m.state.NextID)
				m.state.Users[id] = &mockUser{ID: id, Username: payload.Username, PasswordHash: mockPassword(*payload.Password), Active: true, Days: map[string]float64{}}
				return nil
			}); err != nil {
				return 500, map[string]any{"error": "mock persistence failed"}
			}
			return 201, map[string]any{"user": mockUserResponse(m.state.Users[id])}
		}
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "users" {
		u := m.state.Users[parts[2]]
		if u == nil {
			return 404, map[string]any{"error": "user not found"}
		}
		if len(parts) == 4 && r.Method == "GET" {
			switch parts[3] {
			case "listening-stats":
				total := 0.0
				for _, v := range u.Days {
					total += v
				}
				return 200, map[string]any{"totalTime": total, "days": u.Days, "items": map[string]any{}}
			case "listening-sessions":
				return 200, map[string]any{"total": 0, "sessions": []any{}}
			}
		}
		if len(parts) == 3 {
			switch r.Method {
			case "GET":
				return 200, mockUserResponse(u)
			case "PATCH":
				for id, other := range m.state.Users {
					if payload.Username != "" && other.Username == payload.Username && id != u.ID {
						return 409, map[string]any{"error": "username exists"}
					}
				}
				if err := m.mutate(func() error {
					if payload.Username != "" {
						u.Username = payload.Username
					}
					if payload.Password != nil {
						u.PasswordHash = mockPassword(*payload.Password)
					}
					if payload.Active != nil {
						u.Active = *payload.Active
					}
					return nil
				}); err != nil {
					return 500, map[string]any{"error": "mock persistence failed"}
				}
				return 200, mockUserResponse(u)
			case "DELETE":
				if u.ID == "mock-template" {
					return 403, map[string]any{"error": "template protected"}
				}
				if err := m.mutate(func() error { delete(m.state.Users, u.ID); return nil }); err != nil {
					return 500, map[string]any{"error": "mock persistence failed"}
				}
				return 200, map[string]any{"success": true}
			}
		}
	}
	if r.Method == "GET" {
		switch path {
		case "api/sessions":
			return 200, map[string]any{"sessions": []any{}}
		case "api/libraries":
			return 200, []any{}
		}
	}
	return 501, map[string]any{"error": "mock ABS endpoint not implemented", "method": r.Method, "path": path}
}

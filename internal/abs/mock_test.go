package abs

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"ccoff-abs-bot/internal/config"
)

func mockClient(t *testing.T, path string) (*AbsClient, *MockTransport) {
	t.Helper()
	m, err := OpenMockTransport(path)
	if err != nil {
		t.Fatal(err)
	}
	return &AbsClient{BaseURL: "https://abs.mock.invalid", Token: "mock", HttpClient: &http.Client{Transport: m}}, m
}

func TestMockAccountLifecycleAndPersistence(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{AbsTemplateID: "mock-template"}
	t.Cleanup(func() { config.AppConfig = previous })
	path := filepath.Join(t.TempDir(), "mock.json")
	c, m := mockClient(t, path)
	id, err := c.RegisterUser("alice", "old-pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterUser("alice", "duplicate"); err == nil {
		t.Fatal("duplicate username accepted")
	}
	if err := c.UpdateAbsPassword(id, "new-pass"); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateAbsUsername(id, "alice_new"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VerifyUser("alice_new", "old-pass"); err == nil {
		t.Fatal("old password accepted")
	}
	if got, err := c.VerifyUser("alice_new", "new-pass"); err != nil || got != id {
		t.Fatalf("verify: %s %v", got, err)
	}
	if err := m.SetListening(id, "2026-10-28", 2.5); err != nil {
		t.Fatal(err)
	}
	if err := m.SetListening(id, "2026-10-29", 1); err != nil {
		t.Fatal(err)
	}
	c, _ = mockClient(t, path)
	data, code, err := c.SendRequest("GET", AbsUserListeningStatsPath(id), nil)
	var stats struct {
		Total float64            `json:"totalTime"`
		Days  map[string]float64 `json:"days"`
	}
	if err != nil || code != 200 || json.Unmarshal(data, &stats) != nil || stats.Total != 12600 || stats.Days["2026-10-28"] != 9000 {
		t.Fatalf("stats: %s %v", data, err)
	}
	if err := c.SetUserActiveStatus(id, false); err != nil {
		t.Fatal(err)
	}
	if active, err := c.GetUserActiveStatus(id); err != nil || active {
		t.Fatalf("inactive: %v %v", active, err)
	}
	if _, err := c.VerifyUser("alice_new", "new-pass"); err == nil {
		t.Fatal("inactive login accepted")
	}
	if err := c.SetUserActiveStatus(id, true); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUser(id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetUserActiveStatus(id); err == nil {
		t.Fatal("deleted user exists")
	}
}

func TestMockRejectsExternalAndUnknownAndSupportsFailures(t *testing.T) {
	c, m := mockClient(t, filepath.Join(t.TempDir(), "mock.json"))
	c.BaseURL = "https://example.com"
	if _, _, err := c.SendRequest("GET", "/api/users", nil); err == nil {
		t.Fatal("external URL accepted")
	}
	c.BaseURL = "https://abs.mock.invalid"
	if _, code, _ := c.SendRequest("GET", "/unknown", nil); code != 501 {
		t.Fatal("unknown endpoint succeeded")
	}
	for _, mode := range []string{"timeout", "500", "invalid-json", "off"} {
		if err := m.SetFailure(mode); err != nil {
			t.Fatal(err)
		}
		body, code, err := c.SendRequest("GET", "/api/users", nil)
		switch mode {
		case "timeout":
			if err == nil {
				t.Fatal("timeout missing")
			}
		case "500":
			if code != 500 {
				t.Fatal("500 missing")
			}
		case "invalid-json":
			if json.Valid(body) {
				t.Fatal("invalid JSON missing")
			}
		case "off":
			if err != nil || code != 200 {
				t.Fatal("recovery failed")
			}
		}
	}
	if err := m.SetListening("missing", "2026-02-30", 2); err == nil {
		t.Fatal("invalid date accepted")
	}
	if err := m.SetFailure("unknown"); err == nil {
		t.Fatal("unknown failure accepted")
	}
}

func TestMockConcurrentRegistrationIsUnique(t *testing.T) {
	previous := config.AppConfig
	config.AppConfig = &config.Config{AbsTemplateID: "mock-template"}
	t.Cleanup(func() { config.AppConfig = previous })
	c, _ := mockClient(t, filepath.Join(t.TempDir(), "mock.json"))
	var wg sync.WaitGroup
	successes := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := c.RegisterUser("same", "password"); err == nil {
				successes <- id
			}
		}()
	}
	wg.Wait()
	close(successes)
	if len(successes) != 1 {
		t.Fatalf("created %d copies", len(successes))
	}
}

func TestMockPersistenceFailureDoesNotLeavePhantomUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock.json")
	c, m := mockClient(t, path)
	m.path = filepath.Join(path, "not-a-directory.json")
	if _, err := m.SeedUser(99); err == nil {
		t.Fatal("persistence failure hidden")
	}
	m.path = path
	users, err := c.GetAllUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatal("failed mutation left a phantom user")
	}
}

package abs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUserUpdatesPatchOnlyRequestedField(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		update      func(*AbsClient) error
	}{
		{"password", "password", "new-secret", func(c *AbsClient) error { return c.UpdateAbsPassword("account", "new-secret") }},
		{"username", "username", "new-name", func(c *AbsClient) error { return c.UpdateAbsUsername("account", "new-name") }},
		{"status", "isActive", false, func(c *AbsClient) error { return c.SetUserActiveStatus("account", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/users/account" {
					t.Errorf("unexpected account path: %s", r.URL.Path)
				}
				if r.Method == http.MethodGet {
					// These values may change after this response. None of them
					// should be replayed by an unrelated account mutation.
					fmt.Fprint(w, `{"id":"account","username":"old-name","type":"admin","isActive":true,"permissions":{"download":true},"librariesAccessible":["library"]}`)
					return
				}
				if r.Method != http.MethodPatch {
					t.Errorf("unexpected method: %s", r.Method)
				}
				patches++
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if len(payload) != 1 || payload[tc.field] != tc.value {
					t.Errorf("update could overwrite unrelated account fields: %+v", payload)
				}
				fmt.Fprint(w, `{"success":true}`)
			}))
			defer srv.Close()
			c := &AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
			if err := tc.update(c); err != nil || patches != 1 {
				t.Fatalf("patches=%d err=%v", patches, err)
			}
		})
	}
}

func TestUserResponseContracts(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"user":{"id":"account"}}`, true},
		{`{"id":"account"}`, false},
		{`{"user":null}`, false},
	} {
		id, err := extractAbsUserIDFromResponse([]byte(tc.body))
		if (err == nil) != tc.valid || (tc.valid && id != "account") {
			t.Fatalf("body=%s id=%q err=%v", tc.body, id, err)
		}
	}
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"id":"account","isActive":true}`, true},
		{`{"id":"account","isActive":"true"}`, false},
		{`{"user":{"id":"account","isActive":true}}`, false},
		{`{"id":"another","isActive":true}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c := &AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
			active, err := c.GetUserActiveStatus("account")
			if (err == nil) != tc.valid || (tc.valid && !active) {
				t.Fatalf("active=%t err=%v", active, err)
			}
		})
	}
}

func TestUserListResponseContract(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"users":[]}`, true},
		{`{"users":[{"id":"account","username":"alice","type":"user"}]}`, true},
		{`{"users":[{"id":"root","username":"owner","type":"root"},{"id":"admin","username":"operator","type":"admin"}]}`, true},
		{`{"users":[{"id":"account","username":"alice"}]}`, false},
		{`{"users":[{"id":"account","username":"alice","type":"unknown"}]}`, false},
		{`{"users":[{"id":"","username":"alice","type":"user"}]}`, false},
		{`{"users":[{"id":"account","username":"","type":"user"}]}`, false},
		{`{"users":[{"id":"account","username":"alice","type":"user"},{"id":"account","username":"bob","type":"user"}]}`, false},
		{`[]`, false},
		{`{}`, false},
		{`{"users":null}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			c := &AbsClient{BaseURL: srv.URL, Token: "test", HttpClient: srv.Client()}
			_, err := c.GetAllUsers()
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if IsAbsNotFoundError(errors.New("network: 404")) || !IsAbsNotFoundError(fmt.Errorf("lookup: %w", &AbsAPIError{StatusCode: 404})) {
		t.Fatal("not-found detection must use the typed API status")
	}
}

package abs

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
		{`{"users":[{"id":"account","username":"alice"}]}`, true},
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

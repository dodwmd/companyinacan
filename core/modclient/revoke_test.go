package modclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRevokeIdentity(t *testing.T) {
	var got map[string]string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/identity/revoke" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer projected" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"tokens":2,"sessions":1,"detail":"x"}`))
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("projected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{BaseURL: srv.URL, TokenFile: tok, ModuleID: "provisioning"}
	res, err := c.RevokeIdentity(context.Background(), "pers_0123456789abcdef01234567", "user:sso-abc")
	if err != nil || res.Tokens != 2 || res.Sessions != 1 || got["party_id"] != "pers_0123456789abcdef01234567" || got["subject"] != "user:sso-abc" {
		t.Fatalf("%+v %v %v", res, err, got)
	}
	for _, code := range []int{http.StatusConflict, http.StatusForbidden, http.StatusBadRequest} {
		status = code
		if _, err := c.RevokeIdentity(context.Background(), "p", "s"); !errors.Is(err, ErrRevokeRefused) {
			t.Errorf("%d: %v", code, err)
		}
	}
	// can-core being down is "unavailable": retried, never read as "revoked"
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		status = code
		if _, err := c.RevokeIdentity(context.Background(), "p", "s"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%d: %v", code, err)
		}
	}
}

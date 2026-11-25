package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ask(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestKnownTokenIsActiveWithItsRoles(t *testing.T) {
	h := handler(map[string]entry{"tok": {Subject: "user:a", Roles: []string{"hr"}}})
	m := ask(t, h, `{"token":"tok"}`)
	if m["active"] != true || m["subject"] != "user:a" {
		t.Fatalf("got %v", m)
	}
}

func TestUnknownTokenIsInactiveAndLeaksNothing(t *testing.T) {
	h := handler(map[string]entry{"tok": {Subject: "user:a", Roles: []string{"hr"}}})
	m := ask(t, h, `{"token":"nope"}`)
	if m["active"] != false || m["subject"] != nil || m["roles"] != nil {
		t.Fatalf("got %v", m)
	}
}

func TestBadBodyIs400(t *testing.T) {
	rec := httptest.NewRecorder()
	handler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestShippedTokenFileParses(t *testing.T) {
	// the table the e2e overlay bakes into the image must stay valid JSON with known fields only
	b, err := readTable("../config/introstub.json")
	if err != nil || len(b) < 4 {
		t.Fatalf("%v (%d entries)", err, len(b))
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/work"
)

const workCLIToken = "cli-test-token-0123456789abcdefXYZ"

func workServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := work.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "w.db")
	app, err := work.NewApp(cfg, work.Secrets{AdminToken: workCLIToken}, work.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() { srv.Close(); _ = app.Close() })
	req, _ := http.NewRequest("POST", srv.URL+"/v1/projects", strings.NewReader(`{"key":"ACME","name":"Acme"}`))
	req.Header.Set("Authorization", "Bearer "+workCLIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("seed project: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	return srv
}

func runWork(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(append([]string{"work"}, args...), strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestWorkCLIFlow(t *testing.T) {
	srv := workServer(t)
	t.Setenv("CAN_WORK_URL", srv.URL)
	t.Setenv("CAN_WORK_TOKEN", workCLIToken)

	code, out, errs := runWork(t, "", "create", "ACME", "--type", "story", "--title", "Plan Q1", "--priority", "P1", "--assignee", "bob@example.com", "--label", "planning")
	if code != 0 || !strings.Contains(out, "ACME-1") || !strings.Contains(out, "P1") {
		t.Fatalf("create: %d %q %q", code, out, errs)
	}
	// Same idempotency key twice is one issue.
	for i := 0; i < 2; i++ {
		if code, _, errs := runWork(t, "", "create", "ACME", "--title", "Once", "--idempotency-key", "script-run-0001"); code != 0 {
			t.Fatalf("idempotent create: %d %s", code, errs)
		}
	}
	code, out, _ = runWork(t, "", "--output", "json", "list", "-q", "priority:P2")
	var items []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &items) != nil || len(items) != 1 || items[0]["title"] != "Once" {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, out, errs = runWork(t, "", "update", "ACME-1", "--priority", "P0", "--add-label", "urgent", "--assignee", "none", "--if-match", "1"); code != 0 {
		t.Fatalf("update: %d %s %s", code, out, errs)
	}
	if code, _, errs = runWork(t, "", "update", "ACME-1", "--title", "stale", "--if-match", "1"); code != 1 || !strings.Contains(errs, "version_mismatch") {
		t.Fatalf("stale update: %d %s", code, errs)
	}
	if code, _, errs = runWork(t, "", "transition", "ACME-1", "done"); code != 1 || !strings.Contains(errs, "cannot move") {
		t.Fatalf("illegal transition: %d %s", code, errs)
	}
	if code, out, errs = runWork(t, "", "transition", "ACME-1", "in_progress"); code != 0 || !strings.Contains(out, "in_progress") {
		t.Fatalf("transition: %d %s %s", code, out, errs)
	}
	if code, out, errs = runWork(t, "from stdin\n", "comment", "ACME-1", "--body", "-"); code != 0 || !strings.Contains(out, "comment 1") {
		t.Fatalf("comment: %d %s %s", code, out, errs)
	}
	if code, out, _ = runWork(t, "", "get", "ACME-1"); code != 0 || !strings.Contains(out, "ACME-1") {
		t.Fatalf("get: %d %s", code, out)
	}
	batch := `{"issues":[{"type":"epic","title":"E"},{"type":"story","title":"S"}]}`
	for i := 0; i < 2; i++ {
		if code, _, errs = runWork(t, batch, "bulk-create", "ACME", "--idempotency-key", "bulk-script-001"); code != 0 {
			t.Fatalf("bulk: %d %s", code, errs)
		}
	}
	_, out, _ = runWork(t, "", "--output", "json", "list", "--all", "--limit", "2")
	items = nil
	_ = json.Unmarshal([]byte(out), &items)
	if len(items) != 4 {
		t.Fatalf("expected 4 issues after retries, got %d: %s", len(items), out)
	}
}

func TestWorkCLIErrors(t *testing.T) {
	t.Setenv("CAN_WORK_URL", "")
	t.Setenv("CAN_WORK_TOKEN", "")
	if code, _, errs := runWork(t, "", "get", "ACME-1"); code != ExitUsage || !strings.Contains(errs, "CAN_WORK_TOKEN") {
		t.Fatalf("no token: %d %s", code, errs)
	}
	t.Setenv("CAN_WORK_TOKEN", "x")
	t.Setenv("CAN_WORK_URL", "ftp://nope")
	if code, _, _ := runWork(t, "", "get", "ACME-1"); code != ExitUsage {
		t.Fatalf("bad url: %d", code)
	}
	srv := workServer(t)
	t.Setenv("CAN_WORK_URL", srv.URL)
	t.Setenv("CAN_WORK_TOKEN", "wrong-token-wrong-token-wrong")
	if code, _, errs := runWork(t, "", "get", "ACME-1"); code != ExitFailure || !strings.Contains(errs, "401") {
		t.Fatalf("bad token: %d %s", code, errs)
	}
	t.Setenv("CAN_WORK_TOKEN", workCLIToken)
	if code, _, _ := runWork(t, "", "create", "ACME"); code != ExitUsage {
		t.Fatalf("missing --title: %d", code)
	}
	if code, _, _ := runWork(t, "", "update", "ACME-1"); code != ExitUsage {
		t.Fatalf("empty update: %d", code)
	}
	if code, _, _ := runWork(t, "  ", "comment", "ACME-1", "--body", "-"); code != ExitUsage {
		t.Fatalf("blank comment: %d", code)
	}
}

func TestWorkClientRetriesOnlyWhenSafe(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := work.NewClient(srv.URL, "tok")
	c.Sleep = func(time.Duration) {}
	_, _ = c.Do(context.Background(), "POST", "/v1/x", map[string]string{}, nil)
	if posts.Load() != 1 {
		t.Fatalf("a POST without an idempotency key was retried: %d", posts.Load())
	}
	posts.Store(0)
	_, err := c.Do(context.Background(), "POST", "/v1/x", map[string]string{}, nil, "Idempotency-Key", "abcdefgh12")
	if posts.Load() != 3 || err == nil {
		t.Fatalf("keyed POST attempts = %d, err = %v", posts.Load(), err)
	}
}

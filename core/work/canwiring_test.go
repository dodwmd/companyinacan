package work

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const pat = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func canHarness(t *testing.T) (*harness, *fakecore.Core) {
	t.Helper()
	core := fakecore.New(t, "work")
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
	app, err := NewApp(cfg, Secrets{AdminToken: bootstrap}, Options{Logger: slog.New(slog.DiscardHandler), Can: core.Client(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return &harness{t: t, app: app, clk: &clock{t: time.Now()}, h: app.Handler()}, core
}

func TestPersonalAccessTokenAuthenticatesThroughCanCore(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	h.mustDo(201, "POST", "/v1/projects", pat, map[string]any{"key": "WEB", "name": "Website"})
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "OPS", "name": "Ops"}) // the bootstrap token still works
}

func TestReadOnlyTokenCannotWrite(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermRead}})
	if r := h.do("POST", "/v1/projects", pat, map[string]any{"key": "WEB", "name": "Website"}); r.Code != 403 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if r := h.do("GET", "/v1/projects", pat, nil); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestUnknownRevokedAndForeignTokensAreRefused(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	core.AddToken(pat+"X", fakecore.Token{Subject: "user:mallory", Permissions: []string{PermAdmin}, Module: "people"}) // bound to another module
	core.AddToken(pat+"Y", fakecore.Token{Subject: "user:bob", Permissions: []string{"people.read"}})                    // no permission this module defines
	core.AddToken(pat+"Z", fakecore.Token{Subject: "service-account", Permissions: []string{PermAdmin}})                // not a user subject
	for name, tok := range map[string]string{"unknown": pat + "unknown", "foreign": pat + "X", "no known permission": pat + "Y", "bad subject": pat + "Z"} {
		if r := h.do("GET", "/v1/projects", tok, nil); r.Code != 401 {
			t.Errorf("%s: %d", name, r.Code)
		}
	}
	if r := h.do("GET", "/v1/projects", pat, nil); r.Code != 200 {
		t.Fatal(r.Code)
	}
	core.Revoke(pat)
	h.app.handler = h.app.handler // the module caches an answer for a short TTL; a fresh app proves the revoked answer
	h2, core2 := canHarness(t)
	core2.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}, Revoked: true})
	if r := h2.do("GET", "/v1/projects", pat, nil); r.Code != 401 {
		t.Fatalf("revoked token: %d", r.Code)
	}
}

func TestCanCoreOutageFailsClosedButBootstrapStillWorks(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	core.SetDown(true)
	if r := h.do("GET", "/v1/projects", pat, nil); r.Code != 401 {
		t.Fatalf("with can-core down a personal token must not be accepted: %d", r.Code)
	}
	if r := h.do("GET", "/v1/projects", bootstrap, nil); r.Code != 200 {
		t.Fatalf("the bootstrap token must not depend on can-core: %d", r.Code)
	}
}

func TestValidTokenIsNeverThrottledByBadOnes(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(pat, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	for i := 0; i < 40; i++ { // over the per-address failure limit
		h.do("GET", "/v1/projects", "canpat_"+strings.Repeat("b", 20)+string(rune('a'+i%26))+strings.Repeat("c", i%9), nil)
	}
	for i := 0; i < 50; i++ {
		if r := h.do("GET", "/v1/projects", pat, nil); r.Code != 200 {
			t.Fatalf("valid token refused after a flood of bad ones (%d, attempt %d)", r.Code, i)
		}
	}
}

func TestEventsGoThroughCanCoreWithoutAWebhookURL(t *testing.T) {
	h, core := canHarness(t)
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "WEB", "name": "Website"})
	h.mustDo(201, "POST", "/v1/projects/WEB/issues", bootstrap, map[string]any{"title": "First", "type": "task"})
	if h.app.dispatcher == nil {
		t.Fatal("no dispatcher although the module is installed under can-core")
	}
	n, err := h.app.dispatcher.DeliverDue(context.Background())
	if err != nil || n == 0 {
		t.Fatalf("delivered %d: %v", n, err)
	}
	var created bool
	for _, e := range core.Published() {
		created = created || e.Type == "work.issue.created.v1"
		if e.SourceID == "" || !strings.HasPrefix(e.SourceID, "evt_") {
			t.Errorf("event without a source id: %+v", e)
		}
		for _, k := range []string{"description", "body", "comment"} {
			if strings.Contains(string(e.Data), `"`+k+`"`) {
				t.Errorf("free text (%s) travels in the event: %s", k, e.Data)
			}
		}
	}
	if !created {
		t.Fatalf("work.issue.created.v1 was not published: %+v", core.Published())
	}
	// a second pass publishes nothing new
	if n, _ := h.app.dispatcher.DeliverDue(context.Background()); n != 0 {
		t.Fatalf("redelivered %d", n)
	}
}

func TestPublishFailureKeepsTheEventForRetry(t *testing.T) {
	h, core := canHarness(t)
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "WEB", "name": "Website"})
	h.mustDo(201, "POST", "/v1/projects/WEB/issues", bootstrap, map[string]any{"title": "First", "type": "task"})
	core.SetDown(true)
	if n, _ := h.app.dispatcher.DeliverDue(context.Background()); n != 0 {
		t.Fatalf("delivered %d while can-core was down", n)
	}
	core.SetDown(false)
	h.app.dispatcher.Backoff = time.Nanosecond
	time.Sleep(5 * time.Millisecond)
	h.clk.t = h.clk.t.Add(time.Hour)
	h.app.dispatcher.Now = func() time.Time { return h.clk.Now() }
	if n, _ := h.app.dispatcher.DeliverDue(context.Background()); n == 0 {
		t.Fatal("the event was lost instead of retried")
	}
}

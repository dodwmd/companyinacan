package finance

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUISessionsPerSubjectAreCapped(t *testing.T) {
	u := newUIEnv(t)
	tok := u.token("user:ann", []string{"finance.read"}, nil, nil, false)
	var first *client
	for i := 0; i < uiMaxPerSubject+2; i++ {
		u.advance(time.Second) // sessions are ordered by their creation time
		c := u.client(t)
		if w := c.loginWith(tok); w.Code != 303 {
			t.Fatalf("login %d = %d", i, w.Code)
		}
		if i == 0 {
			first = c
		}
	}
	status(t, first.get("/"), 303) // the oldest session was dropped
}

func TestUIFlashMessageCannotBeChosenByALink(t *testing.T) {
	u := newUIEnv(t)
	c := u.login(t, u.token("user:ann", []string{"finance.read"}, nil, nil, false))
	w := c.get("/?ok=" + url.QueryEscape("<b>pwned</b> send your token"))
	status(t, w, 200)
	mustNot(t, w, "pwned", "<b>")
	mustHave(t, c.get("/?ok=flash.saved"), "Saved.")
}

func TestUILoginTokenIsNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	u := newUIEnv(t, func(_ *Config, o *Options) { o.Logger = slog.New(slog.NewTextHandler(&buf, nil)) })
	tok := u.token("user:ann", []string{"finance.read"}, nil, nil, false)
	c := u.login(t, tok)
	c.get("/invoices?status=draft&q=secretquery")
	if strings.Contains(buf.String(), tok) || strings.Contains(buf.String(), "secretquery") {
		t.Fatalf("the log holds a token or a query string: %s", buf.String())
	}
}

func TestUIStaticAssetsAndHeaders(t *testing.T) {
	u := newUIEnv(t)
	c := u.client(t)
	css := c.get("/static/finance.css")
	status(t, css, 200)
	if !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Errorf("content type %q", css.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/static/../go.mod", "/static/.hidden", "/static/missing.css"} {
		if w := c.get(p); w.Code == 200 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
	login := c.get("/login")
	for h, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if login.Header().Get(h) != want {
			t.Errorf("%s = %q", h, login.Header().Get(h))
		}
	}
	for _, h := range []string{"ETag", "Last-Modified"} {
		if login.Header().Get(h) != "" {
			t.Errorf("%s must not be set on a page", h)
		}
	}
	if strings.Contains(login.Body.String(), "data:") || strings.Contains(login.Body.String(), "<script") {
		t.Error("login page has a data: URL or a script")
	}
}

// Concurrent requests of one session while it re-authenticates must not race (run with -race).
func TestUIConcurrentRequestsOfOneSession(t *testing.T) {
	u := newUIEnv(t)
	c := u.login(t, u.token("user:boss", []string{"finance.approve"}, nil, nil, false))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cc := &client{t: t, e: c.e, cookies: c.cookies}
			if i%2 == 0 {
				u.advance(time.Second)
			}
			cc.do("GET", "/approvals", nil)
		}(i)
	}
	wg.Wait()
}

func TestUIBoundedLists(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < uiPageSize+5; i++ {
		w.u.must(201, adminTok, "POST", "/v1/invoices", obj{"party_id": mustAtoi(t, w.party), "lines": []obj{line("Bulk", 1000, 100)}}, nil)
	}
	page := w.readonly.get("/invoices")
	status(t, page, 200)
	if n := strings.Count(page.Body.String(), `<th scope="row"><a href="/invoices/`); n != uiPageSize {
		t.Fatalf("rows = %d, want %d", n, uiPageSize)
	}
	mustHave(t, page, `rel="next"`)
	// an invalid cursor is a 422, not a scan
	status(t, w.readonly.get("/invoices?cursor=garbage"), 422)
	// an invalid filter is a 422
	status(t, w.readonly.get("/invoices?from=2026-13-45"), 422)
	status(t, w.readonly.get("/invoices?status=bogus"), 422)
}

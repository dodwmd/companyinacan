package finance

import (
	"bytes"
	"fmt"
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

func TestUISettingsHidesCompanyWideCountersFromScopedTokens(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Counted", "10.00")
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	mustHave(t, w.readonly.get("/settings"), "Last number issued", "INV")
	scoped := w.u.login(t, w.u.token("user:scoped", []string{"finance.read"}, []int64{mustAtoi(t, w.party)}, nil, false))
	page := scoped.get("/settings")
	status(t, page, 200)
	mustNot(t, page, "Last number issued", "<th scope=\"row\">INV</th>")
	proj := w.u.login(t, w.u.token("user:proj", []string{"finance.read"}, nil, []string{"ACME"}, false))
	mustNot(t, proj.get("/settings"), "Last number issued")
	mustHave(t, proj.get("/settings"), "Tax rates") // the rest of the page stays
}

func TestLogPathRedactsTheLinkSegmentAnywhere(t *testing.T) {
	for _, p := range []string{"/public/invoices/SECRET", "//public/invoices/SECRET", "/public/public/invoices/SECRET", "/x/public/invoices/SECRET/y"} {
		if got := logPath(p); strings.Contains(got, "SECRET") {
			t.Errorf("logPath(%q) = %q", p, got)
		}
	}
	if logPath("/v1/invoices/3") != "/v1/invoices/3" {
		t.Error("ordinary paths are logged as they are")
	}
	var buf bytes.Buffer
	u := newUIEnv(t, func(_ *Config, o *Options) { o.Logger = slog.New(slog.NewTextHandler(&buf, nil)) })
	httptestGet(u.h, "//public/invoices/SECRETTWO")
	httptestGet(u.h, "/public/public/invoices/SECRETTHREE")
	if strings.Contains(buf.String(), "SECRETTWO") || strings.Contains(buf.String(), "SECRETTHREE") {
		t.Fatalf("a link token reached the log: %s", buf.String())
	}
}

func TestUIStateChangesNeedTheFormVersion(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Versioned", "10.00")
	for _, p := range []string{"/invoices/" + inv + "/approve", "/invoices/" + inv + "/send", "/invoices/" + inv + "/delete", "/invoices/" + inv + "/void", "/invoices/" + inv} {
		if r := w.writer.post(p, url.Values{"version": {""}, "reason": {"x"}, "currency": {"USD"}, "desc": {"x"}, "qty": {"1"}, "price": {"1"}}); r.Code != 400 {
			t.Errorf("POST %s without a version = %d, want 400", p, r.Code)
		}
	}
	// a stale version is a 409, as in the other UIs
	status(t, w.approver.post("/invoices/"+inv+"/approve", url.Values{"version": {"99"}}), 409)
	if mustInvoice(t, w, inv).Status != StDraft {
		t.Fatal("a refused post changed the invoice")
	}
}

func TestUIErasureAsksForTheTokenAgain(t *testing.T) {
	w := newWorld(t)
	status(t, w.admin.post("/parties/"+w.party+"/erase", url.Values{"confirm": {"ERASE"}}), 422)
	mustHave(t, w.readonly.get("/parties/"+w.party), "Acme Pty") // nothing was erased
}

func TestUIPartyPickerSearchesAndSaysWhenCut(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 105; i++ {
		w.u.must(201, adminTok, "POST", "/v1/parties", obj{"name": fmt.Sprintf("Bulk %03d", i), "kind": "customer"}, nil)
	}
	page := w.writer.get("/invoices/new")
	mustHave(t, page, "Only the first 100 matching parties", `name="pq"`)
	narrow := w.writer.get("/invoices/new?pq=Bulk+104")
	mustHave(t, narrow, "Bulk 104")
	mustNot(t, narrow, "Only the first 100", "Bulk 001")
	status(t, w.writer.get("/contracts/new?pq="+strings.Repeat("a", 101)), 422)
}

func TestUILinkLifetimeIsBoundedBeforeItIsUsed(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Link", "10.00")
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	for _, v := range []string{"9223372036854775807", "2161", "-1", "abc"} {
		if r := w.writer.post("/invoices/"+inv+"/link", url.Values{"ttl_hours": {v}}); r.Code != 422 {
			t.Errorf("ttl_hours %q = %d, want 422", v, r.Code)
		}
	}
	status(t, w.writer.post("/invoices/"+inv+"/link", url.Values{"ttl_hours": {"2160"}}), 200)
}

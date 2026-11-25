package adminui

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/pat"
)

func newTokenUI(t *testing.T) *pui {
	t.Helper()
	p := newPUI(t)
	p.ui.PATs = pat.New(p.db, p.audit)
	p.ui.PATs.Now = func() time.Time { return *p.now }
	p.ui.PATs.Holds = func(ctx context.Context, subject, perm string) (bool, error) {
		return p.rb.Check(ctx, subject, perm, "")
	}
	return p
}

var patTokenRe = regexp.MustCompile(`canpat_[A-Za-z0-9_-]{43}`)

func TestTokenPageIsUnavailableWithoutTheService(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	if r := h.do("GET", "/tokens", nil, s.with()...); r.StatusCode != 503 {
		t.Fatalf("GET /tokens without PATs = %d", r.StatusCode)
	}
}

func TestIssueShowsTheTokenOnceThenListsWithoutIt(t *testing.T) {
	p := newTokenUI(t)
	root := p.loginAs("root")
	page := p.do("GET", "/tokens", nil, root.with()...)
	if page.StatusCode != 200 || !strings.Contains(page.body, `name="permissions"`) || !strings.Contains(page.body, "expenses.approve") {
		t.Fatalf("%d\n%s", page.StatusCode, page.body)
	}
	if probs := a11yProblems(page.body); len(probs) > 0 {
		t.Errorf("accessibility problems: %v", probs)
	}
	r := p.do("POST", "/tokens", url.Values{"csrf": {root.csrf}, "module": {"expenses"}, "name": {"my laptop"}, "permissions": {"expenses.approve"}, "ttl_hours": {"24"}}, root.with()...)
	if r.StatusCode != 200 {
		t.Fatalf("issue = %d\n%s", r.StatusCode, r.body)
	}
	tok := patTokenRe.FindString(r.body)
	if tok == "" {
		t.Fatalf("the new token is not shown:\n%s", r.body)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control %q", r.Header.Get("Cache-Control"))
	}
	if probs := a11yProblems(r.body); len(probs) > 0 {
		t.Errorf("accessibility problems: %v", probs)
	}
	again := p.do("GET", "/tokens", nil, root.with()...)
	if strings.Contains(again.body, tok) || !strings.Contains(again.body, "my laptop") {
		t.Fatalf("the list shows the token again or lost the row:\n%s", again.body)
	}
	id := regexp.MustCompile(`/tokens/(pat_[0-9a-f]+)/revoke`).FindStringSubmatch(again.body)
	if id == nil {
		t.Fatalf("no revoke form:\n%s", again.body)
	}
	if r := p.do("POST", "/tokens/"+id[1]+"/revoke", url.Values{"csrf": {"wrong"}}, root.with()...); r.StatusCode != 403 {
		t.Fatalf("revoke without a valid csrf token = %d", r.StatusCode)
	}
	if r := p.do("POST", "/tokens/"+id[1]+"/revoke", url.Values{"csrf": {root.csrf}}, root.with()...); r.StatusCode != 303 {
		t.Fatalf("revoke = %d", r.StatusCode)
	}
	if _, ok := p.ui.PATs.Introspect(context.Background(), "expenses", tok); ok {
		t.Fatal("token valid after revoke")
	}
}

func TestIssueRefusesWhatTheUserDoesNotHold(t *testing.T) {
	p := newTokenUI(t)
	alice := p.loginAs("alice")
	r := p.do("POST", "/tokens", url.Values{"csrf": {alice.csrf}, "module": {"expenses"}, "name": {"x"}, "permissions": {"expenses.approve"}, "ttl_hours": {"24"}}, alice.with()...)
	if r.StatusCode != 403 || patTokenRe.MatchString(r.body) {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
	if r := p.do("POST", "/tokens", url.Values{"module": {"expenses"}, "name": {"x"}, "permissions": {"expenses.approve"}, "ttl_hours": {"24"}}, alice.with()...); r.StatusCode != 403 {
		t.Fatalf("missing csrf = %d", r.StatusCode)
	}
}

func TestIssueBadInputIs400(t *testing.T) {
	p := newTokenUI(t)
	root := p.loginAs("root")
	for _, f := range []url.Values{
		{"module": {"nope"}, "name": {"x"}, "permissions": {"expenses.approve"}, "ttl_hours": {"24"}},
		{"module": {"expenses"}, "name": {""}, "permissions": {"expenses.approve"}, "ttl_hours": {"24"}},
		{"module": {"expenses"}, "name": {"x"}, "permissions": {"expenses.approve"}, "ttl_hours": {"abc"}},
		{"module": {"expenses"}, "name": {"x"}, "ttl_hours": {"24"}},
	} {
		f.Set("csrf", root.csrf)
		if r := p.do("POST", "/tokens", f, root.with()...); r.StatusCode != 400 {
			t.Errorf("%v -> %d", f, r.StatusCode)
		}
	}
}

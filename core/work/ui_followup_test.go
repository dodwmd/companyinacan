package work

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The activity feed of a visible issue must not name issues of projects the caller cannot see, in
// the API or in the UI.
func TestActivityDoesNotLeakOutOfScopeLinkedKeys(t *testing.T) {
	f := newUIFixture(t)
	f.h.mustDo(201, "POST", "/v1/issues/"+f.acmeKey+"/links", bootstrap, map[string]any{"type": "blocks", "target": f.secKey})
	r := f.h.do("GET", "/v1/issues/"+f.acmeKey+"/activity", f.rw, nil)
	if r.Code != 200 || strings.Contains(string(r.Body), f.secKey) || strings.Contains(string(r.Body), "SEC") {
		t.Fatalf("API activity leaks: %d %s", r.Code, r.Body)
	}
	if !strings.Contains(string(r.Body), `"linked"`) {
		t.Fatalf("the link entry should stay, redacted: %s", r.Body)
	}
	b := f.browser(t, f.rw)
	page := b.do("GET", "/ui/issues/"+f.acmeKey, nil)
	if page.Code != 200 || strings.Contains(page.Body, f.secKey) || strings.Contains(page.Body, "vuln in prod") {
		t.Fatalf("UI leaks the out-of-scope key: %d", page.Code)
	}
	// An unscoped admin still sees the keys.
	if r := f.h.do("GET", "/v1/issues/"+f.acmeKey+"/activity", bootstrap, nil); !strings.Contains(string(r.Body), f.secKey) {
		t.Fatalf("admin lost the link keys: %s", r.Body)
	}
}

func TestUIForceSecureCookies(t *testing.T) {
	h := newHarness(t, func(c *Config) { on := true; c.UI.ForceSecure = &on })
	req := httptest.NewRequest("GET", "/ui/login", nil)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	cs := rec.Result().Cookies()
	if len(cs) == 0 {
		t.Fatal("no cookie")
	}
	for _, c := range cs {
		if !c.Secure || !strings.HasPrefix(c.Name, "__Host-") {
			t.Errorf("cookie %s not forced secure: %+v", c.Name, c)
		}
	}
	// A session established through the forced-secure names works.
	tok := h.newToken("personal", "dev@example.com", []string{PermWrite})
	b := newUIBrowser(t, h)
	b.cookies = map[string]*http.Cookie{}
	c := b.csrf("/ui/login")
	if r := b.do("POST", "/ui/login", url.Values{"token": {tok}, "csrf": {c}}); r.Code != 303 {
		t.Fatalf("login: %d", r.Code)
	}
	if r := b.do("GET", "/ui/", nil); r.Code != 200 {
		t.Fatalf("session: %d", r.Code)
	}
}

func TestUIForceSecureDefaultsFromPublicURL(t *testing.T) {
	for url, want := range map[string]bool{"https://work.example.com": true, "http://work.localhost:8080": false, "": false} {
		cfg := DefaultConfig()
		if got := cfg.forceSecure(url); got != want {
			t.Errorf("%q: %v", url, got)
		}
	}
	off := false
	cfg := DefaultConfig()
	cfg.UI.ForceSecure = &off
	if cfg.forceSecure("https://x.example") {
		t.Error("explicit false must win")
	}
}

package adminui

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/exportcrypt"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// withExport gives the server an export over a state database of its own.
func withExport(t *testing.T, h *harness) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(context.Background(), path)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h.ui.Export = &companyexport.Service{DBPath: path, Company: "Acme", Audit: h.audit,
		Modules: func(context.Context) ([]store.Module, error) { return nil, nil }, Now: func() time.Time { return *h.now }}
}

func gunzips(t *testing.T, b []byte) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	_, err = io.Copy(io.Discard, zr)
	must(t, err)
}

func TestExportDownload(t *testing.T) {
	h := newHarness(t)
	withExport(t, h)
	s := h.login()
	page := h.do("GET", "/export", nil, s.with()...)
	if page.StatusCode != 200 || !strings.Contains(page.body, `action="/export"`) {
		t.Fatalf("%d %s", page.StatusCode, page.body)
	}
	if probs := a11yProblems(page.body); len(probs) > 0 {
		t.Error(probs)
	}
	post := func(kv ...string) resp {
		form := url.Values{"csrf": {s.csrf}}
		for i := 0; i+1 < len(kv); i += 2 {
			form.Set(kv[i], kv[i+1])
		}
		return h.do("POST", "/export", form, s.with()...)
	}
	for name, kv := range map[string][]string{
		"sealed unconfirmed":   {"sealed", "yes", "passphrase", "a long enough passphrase", "passphrase2", "a long enough passphrase"},
		"sealed unencrypted":   {"sealed", "yes", "understood", "yes"},
		"passphrases differ":   {"passphrase", "a long enough passphrase", "passphrase2", "another long passphrase"},
		"passphrase too short": {"passphrase", "short", "passphrase2", "short"},
	} {
		if r := post(kv...); r.StatusCode != 400 || r.Header.Get("Content-Disposition") != "" {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	r := post()
	if r.StatusCode != 200 || !strings.HasSuffix(r.Header.Get("Content-Disposition"), `.tar.gz"`) || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", r.StatusCode, r.Header, r.body)
	}
	gunzips(t, []byte(r.body))
	r = post("sealed", "yes", "understood", "yes", "passphrase", "a long enough passphrase", "passphrase2", "a long enough passphrase")
	if r.StatusCode != 200 || !strings.HasSuffix(r.Header.Get("Content-Disposition"), `.canenc"`) {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	var plain bytes.Buffer
	must(t, exportcrypt.Decrypt(&plain, strings.NewReader(r.body), []byte("a long enough passphrase")))
	gunzips(t, plain.Bytes())
	recs, err := h.audit.Query(context.Background(), audit.Filter{ActionPrefix: "company."})
	must(t, err)
	raw, err := json.Marshal(recs)
	must(t, err)
	got := string(raw)
	if strings.Count(got, `"company.export.completed"`) != 2 || !strings.Contains(got, `include_sealed\":true`) || strings.Contains(got, "long enough passphrase") {
		t.Errorf("audit: %s", got)
	}
	if strings.Contains(h.logs.String(), "long enough passphrase") {
		t.Error("the passphrase is in the log")
	}

	// An old sign-in must be proved again before anything leaves.
	*h.now = h.now.Add(time.Hour)
	if r := post(); r.StatusCode != 303 || r.Header.Get("Location") != "/reauth?next=%2Fexport" || r.Header.Get("Content-Disposition") != "" {
		t.Fatalf("stale session: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if page := h.do("GET", "/export", nil, s.with()...); strings.Contains(page.body, `action="/export"`) {
		t.Error("the form is offered to a stale session")
	}
	// A post without the CSRF token is refused.
	if r := h.do("POST", "/export", url.Values{}, s.with()...); r.StatusCode != 403 {
		t.Error(r.StatusCode)
	}
}

func TestExportIsTheOwners(t *testing.T) {
	p := newPUI(t)
	withExport(t, p.harness)
	_, err := p.db.ExecContext(context.Background(), `INSERT INTO rbac_assignments(subject, role, scope, granted_by, granted_at) VALUES ('user:alice','operator','','test',0)`)
	must(t, err)
	p.rb.Invalidate()
	alice := p.loginAs("alice")
	for _, method := range []string{"GET", "POST"} {
		if r := p.do(method, "/export", url.Values{"csrf": {alice.csrf}}, alice.with()...); r.StatusCode != 403 {
			t.Errorf("%s as an operator: %d", method, r.StatusCode)
		}
	}
	if page := p.do("GET", "/settings", nil, alice.with()...); strings.Contains(page.body, `href="/export"`) {
		t.Error("an operator is shown the link")
	}
	root := p.loginAs("root")
	if page := p.do("GET", "/settings", nil, root.with()...); !strings.Contains(page.body, `href="/export"`) {
		t.Error("the owner has no link")
	}
}

package finance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

const signBase = "https://finance.example.test"

type fakeMailer struct {
	mu   sync.Mutex
	sent []modclient.Email
	err  error
}

func (m *fakeMailer) SendEmail(_ context.Context, e modclient.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, e)
	return nil
}

func (m *fakeMailer) all() []modclient.Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

var linkRE = regexp.MustCompile(regexp.QuoteMeta(signBase) + `(/public/sign/[A-Za-z0-9_-]{43})`)

type signEnv struct {
	*env
	mail *fakeMailer
	logs *lockedBuf
	ct   Contract
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const contractBody = "Services agreement.\n\nThe supplier supplies <widgets> & nothing else."

func newSignEnv(t *testing.T, opts ...envOpt) *signEnv {
	t.Helper()
	m, logs := &fakeMailer{}, &lockedBuf{}
	opts = append([]envOpt{func(c *Config, o *Options) {
		c.SignBaseURL = signBase
		o.Mailer = m
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	}}, opts...)
	e := &signEnv{env: newEnv(t, opts...), mail: m, logs: logs}
	p := e.party(adminTok, "Customer One", "customer")
	e.must(201, adminTok, "POST", "/v1/contracts", obj{"party_id": p, "title": "Services agreement", "body": contractBody}, &e.ct)
	return e
}

func (e *signEnv) path(suffix string) string { return "/v1/contracts/" + id(e.ct.ID) + suffix }

func signers(list ...[3]string) []obj {
	var out []obj
	for _, s := range list {
		out = append(out, obj{"name": s[0], "email": s[1], "role": s[2]})
	}
	return out
}

var (
	ann = [3]string{"Ann External", "ann@customer.example", "counterparty"}
	bob = [3]string{"Bob Boss", "bob@company.example", "company"}
)

func (e *signEnv) send(ordered bool, list ...[3]string) Envelope {
	e.t.Helper()
	var env Envelope
	e.must(201, adminTok, "POST", e.path("/signing"), obj{"signers": signers(list...), "ordered": ordered}, &env)
	return env
}

// link returns the newest link mailed to an address.
func (e *signEnv) link(addr string) string {
	e.t.Helper()
	e.app.store.sign.wg.Wait()
	all := e.mail.all()
	for i := len(all) - 1; i >= 0; i-- {
		if len(all[i].To) != 1 {
			e.t.Fatalf("a message must go to one signer: %v", all[i].To)
		}
		if m := linkRE.FindStringSubmatch(all[i].Text); m != nil && all[i].To[0] == addr {
			return m[1]
		}
	}
	e.t.Fatalf("no link was mailed to %s", addr)
	return ""
}

func (e *signEnv) public(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "198.51.100.77:4000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *signEnv) signForm(name string) url.Values {
	return url.Values{"action": {"sign"}, "typed_name": {name}, "consent": {"yes"}, "sha256": {e.ct.SHA256}}
}

func (e *signEnv) contract() Contract {
	e.t.Helper()
	var c Contract
	e.must(200, adminTok, "GET", e.path(""), nil, &c)
	return c
}

func (e *signEnv) verify() VerifyReport {
	e.t.Helper()
	var v VerifyReport
	e.must(200, adminTok, "GET", e.path("/verify"), nil, &v)
	return v
}

func TestSigningHappyPathWithCountersign(t *testing.T) {
	e := newSignEnv(t)
	env := e.send(true, ann, bob)
	if env.Status != EnvPending || env.Signers[0].Status != SignerInvited || env.Signers[0].MailStatus != MailQueued || env.Signers[1].Status != SignerWaiting {
		t.Fatalf("%+v", env)
	}
	if c := e.contract(); c.Status != CtSent || c.ESignProvider != SignProvider {
		t.Fatalf("%+v", c)
	}
	if n := len(e.mail.all()); n != 1 {
		t.Fatalf("in order, only the first signer is invited: %d messages", n)
	}
	// Staff cannot record the signature for the signers.
	e.must(409, adminTok, "POST", e.path("/signature"), obj{"sha256": e.ct.SHA256}, nil)

	link := e.link(ann[1])
	// Opening the page twice (a mail scanner, then the person) consumes nothing.
	for range 2 {
		w := e.public("GET", link, nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "&lt;widgets&gt; &amp; nothing else") || !strings.Contains(body, e.ct.SHA256) {
			t.Fatalf("%d %s", w.Code, body)
		}
		for k, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "X-Frame-Options": "DENY", "X-Robots-Tag": "noindex, nofollow",
			"Content-Security-Policy": "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s = %q", k, got)
			}
		}
		if strings.Contains(body, "<script") || strings.Contains(body, "style=") || strings.Contains(body, "//fonts") {
			t.Error("script, inline style or outside resource")
		}
		if strings.Contains(body, bob[1]) || strings.Contains(body, bob[0]) {
			t.Error("the page names the other signer")
		}
	}
	doc := e.public("GET", link+"/document", nil)
	sum := sha256.Sum256(doc.Body.Bytes())
	if doc.Code != 200 || hex.EncodeToString(sum[:]) != e.ct.SHA256 {
		t.Fatalf("the download is not the hashed document: %d", doc.Code)
	}
	if css := e.public("GET", "/public/sign/assets/sign.css", nil); css.Code != 200 {
		t.Fatalf("css %d", css.Code)
	}

	// Incomplete forms do not use the link up.
	for _, f := range []url.Values{
		{"action": {"sign"}, "typed_name": {"Ann"}, "sha256": {e.ct.SHA256}},
		{"action": {"sign"}, "typed_name": {"  "}, "consent": {"yes"}, "sha256": {e.ct.SHA256}},
		{"action": {"sign"}, "typed_name": {"Ann"}, "consent": {"yes"}, "sha256": {strings.Repeat("0", 64)}},
		{"action": {"approve"}},
	} {
		if w := e.public("POST", link, f); w.Code != 422 {
			t.Fatalf("%v: %d", f, w.Code)
		}
	}
	if w := e.public("POST", link, e.signForm("Ann"), "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
		t.Fatalf("cross-site: %d", w.Code)
	}
	w := e.public("POST", link, e.signForm("Ann Q. External"), "Sec-Fetch-Site", "same-origin")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Your signature is recorded") || !strings.Contains(w.Body.String(), "Others still have to sign") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := e.contract(); c.Status != CtSent || c.SignedAt != nil {
		t.Fatalf("one of two signatures: %+v", c)
	}
	if w := e.public("POST", link, e.signForm("Ann")); w.Code != 404 {
		t.Fatalf("a link works once: %d", w.Code)
	}
	// Not yet completed: no certificate.
	e.must(404, adminTok, "GET", e.path("/signing/certificate"), nil, nil)

	second := e.link(bob[1])
	if w := e.public("POST", second, e.signForm("Bob Boss")); w.Code != 200 || !strings.Contains(w.Body.String(), "Everyone has signed") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.app.store.sign.wg.Wait()
	c := e.contract()
	if c.Status != CtActive || c.SignedAt == nil || c.SignedSHA256 != e.ct.SHA256 {
		t.Fatalf("%+v", c)
	}
	ev := e.outbox(EventContractSigned)
	if len(ev) != 1 {
		t.Fatalf("%d signed events", len(ev))
	}
	// Paths the router redirects or does not know must not reach the log either.
	for _, p := range []string{"/" + link, "/public//sign/" + strings.TrimPrefix(link, "/public/sign/"), link + "/", link + "/x/y"} {
		e.public("GET", p, nil)
	}
	// No personal data in events, the audit log or the logs; no link in any of them.
	var audit []string
	rows, err := e.app.store.db.Query(`SELECT actor || ' ' || action || ' ' || target || ' ' || detail FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		audit = append(audit, s)
	}
	_ = rows.Close()
	haystack := strings.Join(audit, "\n") + "\n" + e.logs.String()
	for _, o := range ev {
		haystack += "\n" + toJSONString(o)
	}
	for _, secret := range []string{ann[1], bob[1], "Ann Q. External", "Bob Boss", "198.51.100", strings.TrimPrefix(link, "/public/sign/"), strings.TrimPrefix(second, "/public/sign/")} {
		if strings.Contains(haystack, secret) {
			t.Errorf("%q is in the audit log, application log or events", secret)
		}
	}
	for _, want := range []string{"contract.signing.send", "contract.signing.invite", "contract.signing.signed", "contract.signed", "contract.signing.mail"} {
		if !strings.Contains(haystack, want) {
			t.Errorf("no %s audit entry", want)
		}
	}

	// The signers' copy and the staff certificate.
	var copies int
	for _, m := range e.mail.all() {
		if strings.HasPrefix(m.Subject, "Signed: ") {
			copies++
			if !strings.Contains(m.Text, e.ct.SHA256) || strings.Contains(m.Text, "198.51.100") || strings.Contains(m.Text, "@company.example") || strings.Contains(m.Text, "@customer.example") {
				t.Errorf("signers' copy: %s", m.Text)
			}
		}
	}
	if copies != 2 {
		t.Fatalf("%d completion messages", copies)
	}
	code, hdr, cert := e.req(adminTok, "GET", e.path("/signing/certificate"), nil)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{e.ct.SHA256, "Ann Q. External", ann[1], "198.51.100.0/24", "not an advanced or qualified"} {
		if !strings.Contains(string(cert), want) {
			t.Errorf("certificate lacks %q:\n%s", want, cert)
		}
	}
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}

	// Signed means immutable: through the API and past it.
	e.must(409, adminTok, "POST", e.path("/versions"), obj{"body": "other"}, nil)
	e.must(409, adminTok, "POST", e.path("/signing/revoke"), nil, nil)
	e.must(409, adminTok, "POST", e.path("/signing"), obj{"signers": signers(ann)}, nil)
	for _, q := range []string{
		`UPDATE contracts SET title = 'x'`, `UPDATE contracts SET signed_sha256 = 'x'`, `UPDATE contracts SET status = 'draft'`,
		`UPDATE contract_versions SET body = 'x'`,
		`INSERT INTO contract_versions (contract_id, version, body, variables, sha256, created_by, created_at) VALUES (1, 2, 'x', '{}', 'x', 'x', 0)`,
		`UPDATE signing_envelopes SET status = 'revoked'`, `UPDATE signing_envelopes SET sha256 = 'x'`, `DELETE FROM signing_envelopes`,
		`UPDATE signing_signers SET typed_name = 'x'`, `UPDATE signing_signers SET email = 'x@y.example'`, `DELETE FROM signing_signers`,
		`UPDATE signing_events SET data = '{}'`, `DELETE FROM signing_events`,
	} {
		if _, err := e.app.store.db.Exec(q); err == nil {
			t.Errorf("the database allowed: %s", q)
		}
	}
}

func toJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A database editor who bypasses the triggers is caught by verify.
func TestSigningTamperingIsDetected(t *testing.T) {
	for name, q := range map[string]string{
		"typed name":    `UPDATE signing_signers SET typed_name = 'Someone Else'`,
		"address":       `UPDATE signing_signers SET email = 'other@x.example'`,
		"time":          `UPDATE signing_signers SET signed_at = signed_at - 86400`,
		"event":         `UPDATE signing_events SET at = at - 1 WHERE seq = 2`,
		"removed event": `DELETE FROM signing_events WHERE kind = 'invited'`,
		"title":         `UPDATE signing_envelopes SET title = 'Another agreement'`,
		"text":          `UPDATE contract_versions SET body = 'x', sha256 = '2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881'`,
		"no envelope":   `DELETE FROM signing_events; DELETE FROM signing_signers; DELETE FROM signing_envelopes`,
		// The erasure flag must not switch the checks off on a completed request.
		"scrubbed":            `UPDATE signing_envelopes SET scrubbed = 1`,
		"scrubbed, rewritten": `UPDATE signing_envelopes SET scrubbed = 1; UPDATE signing_signers SET name = 'Mallory', email = 'm@x.example', typed_name = 'Mallory'`,
		"scrubbed, blanked":   `UPDATE signing_envelopes SET scrubbed = 1; UPDATE signing_signers SET name = '', email = '', typed_name = '', consent_text = '', ip = ''`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newSignEnv(t)
			e.send(false, ann)
			if w := e.public("POST", e.link(ann[1]), e.signForm("Ann")); w.Code != 200 {
				t.Fatalf("%d", w.Code)
			}
			e.app.store.sign.wg.Wait()
			if v := e.verify(); !v.OK {
				t.Fatalf("before: %v", v.Problems)
			}
			var triggers []string
			rows, err := e.app.store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger'`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var n string
				_ = rows.Scan(&n)
				triggers = append(triggers, n)
			}
			_ = rows.Close()
			for _, n := range triggers {
				if _, err := e.app.store.db.Exec(`DROP TRIGGER ` + n); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.app.store.db.Exec(q); err != nil {
				t.Fatal(err)
			}
			if v := e.verify(); v.OK {
				t.Fatal("not detected")
			}
		})
	}
}

// Every link that does not work gets the same page and status.
func TestSigningDeadLinksAreIdentical(t *testing.T) {
	e := newSignEnv(t)
	want := e.public("GET", "/public/sign/"+strings.Repeat("A", 43), nil)
	if want.Code != 404 {
		t.Fatalf("%d", want.Code)
	}
	dead := func(what, link string) {
		t.Helper()
		for _, w := range []*httptest.ResponseRecorder{e.public("GET", link, nil), e.public("POST", link, e.signForm("X")), e.public("GET", link+"/document", nil)} {
			if w.Code != 404 || w.Body.String() != want.Body.String() {
				t.Errorf("%s: %d differs from an unknown link", what, w.Code)
			}
		}
	}
	dead("junk", "/public/sign/short")
	if w := e.public("GET", "/public/sign/", nil); w.Code != 404 || w.Body.String() != want.Body.String() {
		t.Errorf("bare prefix: %d", w.Code)
	}
	dead("deep", "/public/sign/a/b/c")

	// resent: the earlier link dies
	env := e.send(false, ann)
	first := e.link(ann[1])
	e.must(200, adminTok, "POST", e.path("/signing/signers/"+id(env.Signers[0].ID)+"/resend"), nil, nil)
	second := e.link(ann[1])
	if first == second {
		t.Fatal("resend kept the link")
	}
	dead("replaced", first)
	// expired
	e.advance(defaultLinkDays*24*time.Hour + time.Second)
	dead("expired", second)
	var cur Envelope
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &cur)
	if !cur.Signers[0].Expired {
		t.Error("not shown as expired")
	}
	e.must(200, adminTok, "POST", e.path("/signing/signers/"+id(env.Signers[0].ID)+"/resend"), nil, nil)
	third := e.link(ann[1])
	if w := e.public("GET", third, nil); w.Code != 200 {
		t.Fatalf("fresh link %d", w.Code)
	}
	// revoked
	e.must(200, adminTok, "POST", e.path("/signing/revoke"), nil, nil)
	dead("revoked", third)
	if c := e.contract(); c.Status != CtDraft {
		t.Fatalf("%s", c.Status)
	}
	// a new version revokes in the same step
	e.send(false, ann)
	fourth := e.link(ann[1])
	e.must(200, adminTok, "POST", e.path("/versions"), obj{"body": "Changed terms."}, nil)
	dead("new version", fourth)
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &cur)
	if cur.Status != EnvRevoked {
		t.Fatalf("%s", cur.Status)
	}
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}
}

func TestSigningDecline(t *testing.T) {
	e := newSignEnv(t)
	e.send(false, ann, bob)
	a, b := e.link(ann[1]), e.link(bob[1])
	if w := e.public("POST", a, url.Values{"action": {"decline"}, "reason": {strings.Repeat("x", 501)}}); w.Code != 422 {
		t.Fatalf("%d", w.Code)
	}
	if w := e.public("POST", a, url.Values{"action": {"decline"}, "reason": {"Wrong price"}}); w.Code != 200 || !strings.Contains(w.Body.String(), "You declined") {
		t.Fatalf("%d", w.Code)
	}
	for _, l := range []string{a, b} {
		if w := e.public("GET", l, nil); w.Code != 404 {
			t.Errorf("after a refusal: %d", w.Code)
		}
	}
	var env Envelope
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &env)
	if env.Status != EnvDeclined || env.Signers[0].DeclineReason != "Wrong price" {
		t.Fatalf("%+v", env)
	}
	if c := e.contract(); c.Status != CtDraft || c.SignedAt != nil {
		t.Fatalf("%+v", c)
	}
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}
	e.send(false, ann) // it can be sent again
}

func TestSigningRefusals(t *testing.T) {
	e := newSignEnv(t)
	for name, body := range map[string]obj{
		"none":      {"signers": []obj{}},
		"bad email": {"signers": signers([3]string{"A", "A <a@b.example>", ""})},
		"two same":  {"signers": signers(ann, ann)},
		"no name":   {"signers": signers([3]string{" ", "a@b.example", ""})},
		"role":      {"signers": signers([3]string{"A", "a@b.example", "witness"})},
		"too long":  {"signers": signers(ann), "expires_in_days": 31},
	} {
		if code := e.do(adminTok, "POST", e.path("/signing"), body, nil); code != 422 {
			t.Errorf("%s: %d", name, code)
		}
	}
	reader := e.token("reader", []string{"finance.read"}, nil, nil, false)
	e.must(403, reader, "POST", e.path("/signing"), obj{"signers": signers(ann)}, nil)
	other := e.token("scoped", []string{"finance.read", "finance.write"}, []int64{e.party(adminTok, "Other Co", "customer")}, nil, false)
	e.must(404, other, "POST", e.path("/signing"), obj{"signers": signers(ann)}, nil)
	e.must(404, other, "GET", e.path("/signing"), nil, nil)
	// Text that could show something other than what is signed.
	var c Contract
	e.must(201, adminTok, "POST", "/v1/contracts", obj{"party_id": e.ct.PartyID, "title": "t", "body": "pay \u202e001$ only"}, &c)
	e.must(422, adminTok, "POST", "/v1/contracts/"+id(c.ID)+"/signing", obj{"signers": signers(ann)}, nil)
}

// A relay or allow-list refusal leaves the request open, says so, and nobody is shown the link.
func TestSigningMailFailure(t *testing.T) {
	e := newSignEnv(t)
	e.mail.err = &modclient.StatusError{Status: http.StatusForbidden}
	code, _, body := e.req(adminTok, "POST", e.path("/signing"), obj{"signers": signers(ann)})
	if code != 201 || !strings.Contains(string(body), `"mail_status":"not_allowed"`) || strings.Contains(string(body), "/public/sign/") || strings.Contains(string(body), "token") {
		t.Fatalf("%d %s", code, body)
	}
	var env Envelope
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &env)
	e.mail.err = nil
	e.must(200, adminTok, "POST", e.path("/signing/signers/"+id(env.Signers[0].ID)+"/resend"), nil, &env)
	if env.Signers[0].MailStatus != MailQueued {
		t.Fatalf("%+v", env.Signers[0])
	}
	for i := 2; i < maxSends; i++ {
		e.must(200, adminTok, "POST", e.path("/signing/signers/"+id(env.Signers[0].ID)+"/resend"), nil, nil)
	}
	e.must(409, adminTok, "POST", e.path("/signing/signers/"+id(env.Signers[0].ID)+"/resend"), nil, nil)
}

func TestSigningIsOffUnlessPublished(t *testing.T) {
	e := newSignEnv(t, func(c *Config, _ *Options) { c.SignBaseURL = "" })
	e.must(409, adminTok, "POST", e.path("/signing"), obj{"signers": signers(ann)}, nil)
	// Turning the setting off also kills links that are already out.
	on := newSignEnv(t)
	on.send(false, ann)
	link := on.link(ann[1])
	on.app.store.sign.baseURL = ""
	if w := on.public("GET", link, nil); w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
}

func TestSigningConfig(t *testing.T) {
	for url, ok := range map[string]bool{"": true, "https://finance.acme.example": true, "http://finance.localhost:8080": true,
		"http://finance.acme.example": false, "https://finance.acme.example/": false, "https://u@finance.acme.example": false, "https://x.example/p": false} {
		c := DefaultConfig()
		c.SignBaseURL = url
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("sign_base_url %q: %v", url, err)
		}
	}
	c := DefaultConfig()
	c.SigningRecordIP = "everything"
	if c.Validate() == nil {
		t.Error("signing_record_ip")
	}
}

func TestRecordedIP(t *testing.T) {
	for _, c := range [][3]string{
		{IPTruncated, "203.0.113.9", "203.0.113.0/24"}, {IPTruncated, "2001:db8:1:2::5", "2001:db8:1::/48"},
		{IPFull, "203.0.113.9", "203.0.113.9"}, {IPNone, "203.0.113.9", ""}, {IPFull, "not an address", ""},
	} {
		if got := recordedIP(c[0], c[1]); got != c[2] {
			t.Errorf("%v: %q", c, got)
		}
	}
}

// Bad links are throttled per address; a working one never is.
func TestSigningBadLinksAreRateLimited(t *testing.T) {
	e := newSignEnv(t, func(c *Config, _ *Options) { c.RateLimit.AuthFailuresPerMin = 3 })
	e.send(false, ann)
	link := e.link(ann[1])
	var last int
	for i := range 5 {
		last = e.public("GET", "/public/sign/"+strings.Repeat("B", 42)+strconv.Itoa(i), nil).Code
	}
	if last != 429 {
		t.Fatalf("%d", last)
	}
	if w := e.public("GET", link, nil); w.Code != 200 {
		t.Fatalf("a working link was throttled: %d", w.Code)
	}
	if w := e.public("POST", link, url.Values{"action": {"sign"}, "typed_name": {strings.Repeat("a", maxSignForm)}}); w.Code == 200 {
		t.Fatalf("oversized form accepted")
	}
}

func TestSigningErasureAndRetention(t *testing.T) {
	e := newSignEnv(t)
	e.send(false, ann)
	link := e.link(ann[1])
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.ct.PartyID)+"/erase", nil, nil)
	if w := e.public("GET", link, nil); w.Code != 404 {
		t.Fatalf("an erased party's link: %d", w.Code)
	}
	var n int
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM signing_signers WHERE email <> '' OR name <> ''`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("signer data left: %d %v", n, err)
	}
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}

	// A completed request is kept by erasure and removed with its contract by retention.
	e = newSignEnv(t)
	e.send(false, ann)
	if w := e.public("POST", e.link(ann[1]), e.signForm("Ann")); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	e.app.store.sign.wg.Wait()
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.ct.PartyID)+"/erase", nil, nil)
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM signing_signers WHERE typed_name = 'Ann'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("signed evidence: %d %v", n, err)
	}
	e.must(200, adminTok, "POST", e.path("/terminate"), obj{"reason": "done"}, nil)
	e.advance(9 * 366 * 24 * time.Hour)
	rep, err := e.app.Purge(context.Background())
	if err != nil || rep.Contracts != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if err := e.app.store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM signing_envelopes) + (SELECT COUNT(*) FROM signing_signers) + (SELECT COUNT(*) FROM signing_events)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("signing records left: %d %v", n, err)
	}
}

// Two people pressing Sign on the same link: one signature.
func TestSigningConcurrentUseOfOneLink(t *testing.T) {
	e := newSignEnv(t)
	e.send(false, ann)
	link := e.link(ann[1])
	codes := make(chan int, 8)
	for range 8 {
		go func() { codes <- e.public("POST", link, e.signForm("Ann")).Code }()
	}
	ok := 0
	for range 8 {
		if <-codes == 200 {
			ok++
		}
	}
	e.app.store.sign.wg.Wait()
	if ok != 1 || len(e.outbox(EventContractSigned)) != 1 {
		t.Fatalf("%d successes", ok)
	}
}

// Staff send, resend, withdraw and read the certificate from the web UI, which never shows a link.
func TestUISigning(t *testing.T) {
	m := &fakeMailer{}
	u := newUIEnv(t, func(c *Config, o *Options) { c.SignBaseURL, o.Mailer = signBase, m })
	p := u.party(adminTok, "Customer One", "customer")
	var ct Contract
	u.must(201, adminTok, "POST", "/v1/contracts", obj{"party_id": p, "title": "NDA <b>", "body": contractBody}, &ct)
	c := u.login(t, adminTok)
	page := "/contracts/" + id(ct.ID)
	if w := c.get(page); !strings.Contains(w.Body.String(), "Send a signing link by email") {
		t.Fatalf("no form: %d", w.Code)
	}
	if w := c.post(page+"/signing", url.Values{"counterparty": {"ann@customer.example"}}); w.Code != 422 {
		t.Fatalf("a line without a name: %d", w.Code)
	}
	c.get(page)
	if w := c.post(page+"/signing", url.Values{"counterparty": {"Ann External <ann@customer.example>\r\n"}, "company": {"Bob Boss <bob@company.example>"}, "ordered": {"yes"}, "days": {"3"}}); w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	env := &signEnv{env: u.env, mail: m, ct: ct}
	link := env.link("ann@customer.example")
	body := c.get(page).Body.String()
	if !strings.Contains(body, "Waiting for signatures") || strings.Contains(body, "/public/sign/") || strings.Contains(body, strings.TrimPrefix(link, "/public/sign/")) {
		t.Fatalf("%s", body)
	}
	m1 := regexp.MustCompile(`/signing/signers/(\d+)/resend`).FindStringSubmatch(body)
	if m1 == nil {
		t.Fatal("no resend button")
	}
	if w := c.post(page+"/signing/signers/"+m1[1]+"/resend", url.Values{}); w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
	link = env.link("ann@customer.example")
	// With the UI on, the public pages are still the signer's, with no session.
	if w := env.public("GET", link, nil); w.Code != 200 || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "NDA &lt;b&gt;") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := env.public("POST", link, env.signForm("Ann")); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if w := env.public("POST", env.link("bob@company.example"), env.signForm("Bob")); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	u.app.store.sign.wg.Wait()
	if body := c.get(page).Body.String(); !strings.Contains(body, "Completion certificate") {
		t.Fatalf("%s", body)
	}
	if w := c.get(page + "/signing/certificate"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "CERTIFICATE OF COMPLETION") {
		t.Fatalf("%d", w.Code)
	}
	if w := c.post(page+"/signing/revoke", url.Values{}); w.Code != 409 {
		t.Fatalf("revoking a completed request: %d", w.Code)
	}
}

// A request that was legitimately erased must be blank: data put back under the flag is caught.
func TestSigningErasedRequestMustStayBlank(t *testing.T) {
	e := newSignEnv(t)
	e.send(false, ann)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.ct.PartyID)+"/erase", nil, nil)
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}
	if _, err := e.app.store.db.Exec(`DROP TRIGGER signing_envelopes_closed; DROP TRIGGER signing_signers_identity; UPDATE signing_signers SET name = 'Mallory'`); err != nil {
		t.Fatal(err)
	}
	if v := e.verify(); v.OK || !strings.Contains(strings.Join(v.Problems, " "), "still holds data") {
		t.Fatalf("%v", v.Problems)
	}
}

// A certificate the mail service refuses is visible to staff, audited, can be sent again, and the
// signer is not told that one is coming.
func TestSigningLostCertificate(t *testing.T) {
	e := newSignEnv(t)
	env := e.send(false, ann)
	link := e.link(ann[1])
	e.mail.err = &modclient.StatusError{Status: http.StatusForbidden}
	w := e.public("POST", link, e.signForm("Ann"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "could not be emailed") || strings.Contains(w.Body.String(), "handed to the mail service") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var cur Envelope
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &cur)
	if cur.Status != EnvCompleted || cur.Signers[0].CertificateStatus != MailNotAllowed {
		t.Fatalf("%+v", cur.Signers[0])
	}
	var n int
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'contract.signing.certificate_mail' AND detail LIKE '%not_allowed%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit: %d %v", n, err)
	}
	e.mail.err = nil
	resend := e.path("/signing/signers/" + id(env.Signers[0].ID) + "/resend")
	e.must(200, adminTok, "POST", resend, nil, &cur)
	all := e.mail.all()
	last := all[len(all)-1]
	if cur.Signers[0].CertificateStatus != MailQueued || last.To[0] != ann[1] || !strings.HasPrefix(last.Subject, "Signed: ") || linkRE.MatchString(last.Text) {
		t.Fatalf("%+v %+v", cur.Signers[0], last)
	}
	if v := e.verify(); !v.OK {
		t.Fatalf("%v", v.Problems)
	}
	// Still possible when the public path has been switched off since; bounded; stopped by erasure.
	e.app.store.sign.baseURL = ""
	for i := 2; i < maxSends; i++ {
		e.must(200, adminTok, "POST", resend, nil, nil)
	}
	e.must(409, adminTok, "POST", resend, nil, nil)
	if _, err := e.app.store.db.Exec(`UPDATE signing_signers SET cert_sends = 1`); err != nil {
		t.Fatal(err)
	}
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.ct.PartyID)+"/erase", nil, nil)
	e.must(409, adminTok, "POST", resend, nil, nil)
}

func TestSigningDeceptiveCharacters(t *testing.T) {
	for _, r := range []rune{0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0x061C, 0x2060, 0x2064, 0x202E, 0x2067, 0xFEFF, 0xE0001, 0xE0041, 0xE007F, 0x07} {
		if !deceptive("a" + string(r) + "b") {
			t.Errorf("U+%04X accepted", r)
		}
	}
	if deceptive("Zoë O'Brien\n\t张伟 — 100 €") {
		t.Error("ordinary text refused")
	}
	e := newSignEnv(t)
	for _, name := range []string{"Ann\u2028Payment: none", "Ann\u2029x", "Ann\u200bx", "Ann\U000E0041"} {
		e.must(422, adminTok, "POST", e.path("/signing"), obj{"signers": signers([3]string{name, "a@b.example", ""})}, nil)
	}
	e.send(false, ann)
	link := e.link(ann[1])
	for _, name := range []string{"Ann\u2028Signed: Bob", "Ann\u2029", "A\u200fnn", "Ann\u061c"} {
		if w := e.public("POST", link, e.signForm(name+" x")); w.Code != 422 {
			t.Errorf("typed name %q: %d", name, w.Code)
		}
	}
}

// Verify reads one snapshot: signatures committing beside it are never reported as tampering.
func TestSigningVerifyWhileSigning(t *testing.T) {
	e := newSignEnv(t)
	var list [][3]string
	for i := range maxSigners {
		list = append(list, [3]string{"Signer " + strconv.Itoa(i), "s" + strconv.Itoa(i) + "@customer.example", ""})
	}
	e.send(false, list...)
	links := make([]string, len(list))
	for i, s := range list {
		links[i] = e.link(s[1])
	}
	stop, bad := make(chan struct{}), make(chan []string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rep, err := e.app.store.VerifyContract(context.Background(), &Principal{Permissions: []string{PermAdmin}}, e.ct.ID)
			if err != nil || !rep.OK {
				var p []string
				if rep != nil {
					p = rep.Problems
				}
				bad <- append(p, fmt.Sprint(err))
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	for _, l := range links {
		if w := e.public("POST", l, e.signForm("S")); w.Code != 200 {
			t.Fatalf("%d", w.Code)
		}
	}
	close(stop)
	wg.Wait()
	e.app.store.sign.wg.Wait()
	select {
	case p := <-bad:
		t.Fatalf("verify during signing: %v", p)
	default:
	}
}

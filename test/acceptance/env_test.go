//go:build acceptance

package acceptance

// The operator's computer: the `can` binary run from the company directory, ssh to the server, a
// browser, and the bookkeeping of the report.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/acceptance/report"
	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

const (
	companyName = "Acceptance Co"
	domain      = "acceptance.test"
	siteURL     = "https://" + domain
	adminURL    = "https://admin." + domain
	workURL     = "https://work." + domain
	supportURL  = "https://support." + domain
	s3URL       = "https://s3." + domain + ":9000"
	sinkHost    = "smtp-sink.acceptance-sink.svc.cluster.local"
	salesEmail  = "sales@" + domain
	smtpUser    = "acceptance-relay"
)

type env struct {
	dir, company, standins, out string
	canBin, images              string
	host, user                  string
	roots                       *x509.CertPool

	smtpPassword string
	secrets      []string
	rep          *report.Report
	totpStep     int64
	logs         int
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if os.Getenv("CAN_ACCEPTANCE") != "1" || os.Getenv("CAN_ACCEPTANCE_DESTROYS_THIS_MACHINE") != "1" {
		t.Skip("the acceptance journey installs and destroys a server on this machine: it runs only in .github/workflows/acceptance.yml")
	}
	dir := os.Getenv("CAN_ACCEPTANCE_DIR")
	if !filepath.IsAbs(dir) {
		t.Fatal("CAN_ACCEPTANCE_DIR must name the directory setup-runner.sh prepared")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		dir: dir, company: filepath.Join(dir, "company"), standins: filepath.Join(dir, "standins"), out: filepath.Join(dir, "out"),
		canBin: filepath.Join(dir, "dist", "can"), images: filepath.Join(dir, "dist", "images"), user: u.Username,
		rep: &report.Report{Started: time.Now(), Facts: map[string]string{}},
	}
	e.host = strings.TrimSpace(string(e.read(t, filepath.Join(e.standins, "host"))))
	e.roots = x509.NewCertPool()
	if !e.roots.AppendCertsFromPEM(e.read(t, filepath.Join(e.standins, "ca.crt"))) {
		t.Fatal("no test CA")
	}
	for _, l := range strings.Split(string(e.read(t, filepath.Join(e.standins, "s3.env"))), "\n") {
		if _, v, ok := strings.Cut(l, "="); ok {
			e.secrets = append(e.secrets, v)
		}
	}
	e.smtpPassword = randomHex(t, 16)
	e.secret(e.smtpPassword)
	return e
}

func (e *env) read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test files
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) write(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (e *env) secret(v ...string) { e.secrets = append(e.secrets, v...) }

func (e *env) redact(s string) string { return report.Redact(s, e.secrets) }

// save writes one numbered log into the output directory, secrets removed.
func (e *env) save(name, body string) {
	e.logs++
	_ = os.WriteFile(filepath.Join(e.out, fmt.Sprintf("%02d-%s.log", e.logs, name)), []byte(e.redact(body)), 0o600)
}

func tail(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// run executes a command and returns stdout and stderr apart.
func run(timeout time.Duration, dir string, environ []string, stdin, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // test helper
	cmd.Dir, cmd.Env = dir, environ
	cmd.WaitDelay = 10 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s: %w", timeout, err)
	}
	return out.String(), errb.String(), err
}

// operatorEnv is a plain user environment: nothing of the checkout or of CI leaks into `can`.
func operatorEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER")}, extra...)
}

// can runs the built binary from the company directory, as the operator does.
func (e *env) can(timeout time.Duration, args ...string) (string, string, error) {
	out, errOut, err := run(timeout, e.company, operatorEnv(), "", e.canBin, args...)
	// stdout of `can up --print-secrets` holds secrets that are not known yet: only stderr is kept.
	e.save("can-"+args[0], "$ can "+strings.Join(args, " ")+"\n"+errOut+fmt.Sprintf("\nexit: %v\n", err))
	return out, errOut, err
}

func (e *env) mustCan(t *testing.T, timeout time.Duration, args ...string) (string, string) {
	t.Helper()
	out, errOut, err := e.can(timeout, args...)
	if err != nil {
		t.Fatalf("can %s: %v\n%s\n%s", strings.Join(args, " "), err, e.redact(tail(errOut, 60)), e.redact(tail(out, 20)))
	}
	return out, errOut
}

// ssh runs a script as root on the server.
func (e *env) ssh(timeout time.Duration, script string) (string, error) {
	out, errOut, err := run(timeout, "", operatorEnv(), "set -euo pipefail\nexport PATH=\"$PATH:/usr/local/bin\"\n"+script+"\n",
		"ssh", "-i", filepath.Join(e.standins, "id_ed25519"), "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", e.user+"@"+e.host, "sudo -n bash -s")
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, tail(errOut+"\n"+out, 30))
	}
	return out, nil
}

func (e *env) mustSSH(t *testing.T, timeout time.Duration, script string) string {
	t.Helper()
	out, err := e.ssh(timeout, script)
	if err != nil {
		t.Fatalf("on the server: %s\n%v", e.redact(script), e.redact(err.Error()))
	}
	return out
}

// eventually polls until fn succeeds, and fails with what it last saw.
func eventually(t *testing.T, timeout time.Duration, what string, fn func() (bool, string)) {
	t.Helper()
	end := time.Now().Add(timeout)
	for {
		ok, saw := fn()
		if ok {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out after %s waiting for %s: %s", timeout, what, tail(saw, 30))
		}
		time.Sleep(3 * time.Second)
	}
}

// browser is a person's browser: cookies, the test CA, no automatic redirects.
type browser struct{ c *http.Client }

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{&http.Client{
		Jar: jar, Timeout: 90 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true},
	}}
}

type page struct {
	status int
	header http.Header
	body   string
}

func (b *browser) do(method, u, body string, hdr map[string]string) (page, error) {
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		return page{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) can-acceptance")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		return page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return page{resp.StatusCode, resp.Header, string(raw)}, err
}

func (b *browser) get(t *testing.T, u string, want ...int) page {
	t.Helper()
	p, err := b.do("GET", u, "", nil)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	expect(t, "GET "+u, p, want...)
	return p
}

// post submits a form the way the browser does from a page of the same origin. The address is not
// put in messages beyond its path: launch links carry tickets.
func (b *browser) post(t *testing.T, u string, form url.Values, want ...int) page {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	p, err := b.do("POST", u, form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": origin, "Referer": origin + "/", "Sec-Fetch-Site": "same-origin"})
	if err != nil {
		t.Fatalf("POST %s: %v", parsed.Path, err)
	}
	expect(t, "POST "+parsed.Host+parsed.Path, p, want...)
	return p
}

func expect(t *testing.T, what string, p page, want ...int) {
	t.Helper()
	for _, w := range want {
		if p.status == w {
			return
		}
	}
	t.Fatalf("%s: want HTTP %v, got %d: %.500s", what, want, p.status, p.body)
}

func match(t *testing.T, what string, re *regexp.Regexp, s string) string {
	t.Helper()
	m := re.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("%s: no match for %s in: %.800s", what, re, s)
	}
	return m[1]
}

// totp returns a code for a time step not used before: can-core accepts a step once.
func (e *env) totp(t *testing.T, secret string) string {
	t.Helper()
	for time.Now().Unix()/30 <= e.totpStep {
		time.Sleep(time.Second)
	}
	now := time.Now()
	e.totpStep = now.Unix() / 30
	code, err := harness.TOTP(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

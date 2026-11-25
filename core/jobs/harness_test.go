package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	adminTok   = "admin-token-0123456789abcdefABCDEF-xyz"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t       *testing.T
	app     *App
	h       http.Handler
	clk     *clock
	dir     string
	dbPath  string
	fake    *FakeSyndicator
	scanner *FakeScanner
	hook    *webhookSink
}

type webhookSink struct {
	mu     sync.Mutex
	events []Event
	srv    *httptest.Server
}

func newSink() *webhookSink {
	w := &webhookSink{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev Event
		_ = json.Unmarshal(b, &ev)
		w.mu.Lock()
		w.events = append(w.events, ev)
		w.mu.Unlock()
	}))
	return w
}

func (w *webhookSink) all() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Event(nil), w.events...)
}

func newEnv(t *testing.T, mut func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(dir, "jobs.db")
	cfg.ResumeDir = filepath.Join(dir, "resumes")
	cfg.Company.Name = "Example Co"
	cfg.Company.BaseURL = "https://example.test"
	cfg.RateLimit.AuthFailuresPerMin = 5
	cfg.RateLimit.ApplyPerHour = 1000
	cfg.Apply.CollectEEO = true
	cfg.Apply.ScannerRequired = false
	hook := newSink()
	t.Cleanup(hook.srv.Close)
	cfg.Webhook.URL = hook.srv.URL
	if mut != nil {
		mut(cfg)
	}
	kr, err := ParseKeyring("k1 " + testKeyHex + "\n")
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	fake := NewFakeSyndicator("fake-board")
	scanner := &FakeScanner{InfectedMarker: "EICAR-TEST"}
	app, err := NewApp(cfg, Secrets{AdminToken: adminTok, WebhookSecret: "whsec-0123456789abcdef", HashKey: "hash-key-0123456789abcdef0123456789abcdef", FormSecret: "form-secret-0123456789abcdef"}, Options{
		Logger: slog.New(slog.DiscardHandler), Now: clk.Now, Keyring: kr, Scanner: scanner,
		Syndicators: append(DefaultSyndicators(), fake),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return &env{t: t, app: app, h: app.Handler(), clk: clk, dir: dir, dbPath: cfg.DBPath, fake: fake, scanner: scanner, hook: hook}
}

type resp struct {
	Code int
	Body []byte
	Hdr  http.Header
}

func (r *resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("response is not JSON (%d): %s", r.Code, r.Body)
	}
}

func (r *resp) m(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	r.json(t, &m)
	return m
}

func (r *resp) str() string { return string(r.Body) }

func (e *env) do(tok, method, path string, body any) *resp {
	e.t.Helper()
	return e.doFrom(tok, "192.0.2.10:4000", method, path, body)
}

func (e *env) doFrom(tok, remote, method, path string, body any) *resp {
	e.t.Helper()
	return e.doReq(tok, remote, method, path, nil, body)
}

func (e *env) doReq(tok, remote, method, path string, hdr map[string]string, body any) *resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = remote
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return &resp{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

func (e *env) want(r *resp, code int) *resp {
	e.t.Helper()
	if r.Code != code {
		e.t.Fatalf("status = %d, want %d: %s", r.Code, code, r.Body)
	}
	return r
}

// mint creates a stored token for a role through the admin API.
func (e *env) mint(role, subject string, jobs ...string) string {
	e.t.Helper()
	if jobs == nil {
		jobs = []string{}
	}
	r := e.want(e.do(adminTok, "POST", "/v1/tokens", map[string]any{"kind": "service", "subject": subject, "name": role + " " + subject, "role": role, "jobs": jobs}), 201)
	return r.m(e.t)["secret"].(string)
}

func jobBody(title, manager string) map[string]any {
	return map[string]any{
		"title": title, "department": "Engineering", "location": map[string]any{"city": "Austin", "region": "TX", "country": "US"},
		"remote": "hybrid", "employment_type": "full_time", "hiring_manager": manager, "headcount": 1,
		"salary":         map[string]any{"min": 100000, "max": 140000, "currency": "USD", "period": "year"},
		"publish_salary": false,
		"sections":       []map[string]string{{"heading": "About", "body": "Do good work.\n\nWith good people."}},
	}
}

// openJob creates and opens a job and returns its id and slug.
func (e *env) openJob(title, manager string) (id, slug string) {
	e.t.Helper()
	m := e.want(e.do(adminTok, "POST", "/v1/jobs", jobBody(title, manager)), 201).m(e.t)
	id, slug = m["id"].(string), m["slug"].(string)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}), 200)
	return id, slug
}

type upload struct {
	field, name, ctype string
	data               []byte
}

var tokRE = regexp.MustCompile(`name="_t" value="([^"]+)"`)

// apply submits the public form like a browser would: fetch the page for a timing token,
// wait like a human, post multipart.
func (e *env) apply(slug string, f map[string]string, files ...upload) *resp {
	return e.applyFrom("198.51.100.7:5000", slug, f, files...)
}

func (e *env) applyFrom(remote, slug string, f map[string]string, files ...upload) *resp {
	e.t.Helper()
	page := e.doFrom("", remote, "GET", "/careers/"+slug, nil)
	if page.Code != 200 {
		e.t.Fatalf("careers page: %d %s", page.Code, page.Body)
	}
	mm := tokRE.FindSubmatch(page.Body)
	if mm == nil {
		e.t.Fatalf("no timing token in the form: %s", page.Body)
	}
	e.clk.Add(10 * time.Second)
	fields := map[string]string{"_t": string(mm[1])}
	for k, v := range f {
		fields[k] = v
	}
	return e.postForm(remote, "/careers/"+slug+"/apply", fields, nil, files...)
}

func (e *env) postForm(remote, path string, fields map[string]string, hdr map[string]string, files ...upload) *resp {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+f.field+`"; filename="`+f.name+`"`)
		h.Set("Content-Type", f.ctype)
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(f.data)
	}
	_ = w.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return &resp{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

func goodApplication(email string) map[string]string {
	return map[string]string{"name": "Ada Example", "email": email, "phone": "+1 555 0100", "cover_letter": "I like building things.", "consent": "on"}
}

var pdf = upload{field: "resume", name: "ada.pdf", ctype: "application/pdf", data: []byte("%PDF-1.7\nfake resume bytes RESUME-MARKER-123\n")}

// applyOK submits a valid application and returns its application id (via the staff API).
func (e *env) applyOK(slug, email string, files ...upload) string {
	e.t.Helper()
	r := e.apply(slug, goodApplication(email), files...)
	if r.Code != 303 {
		e.t.Fatalf("apply = %d: %s", r.Code, r.Body)
	}
	jobID := ""
	pr := e.want(e.do(adminTok, "GET", "/v1/jobs?limit=200", nil), 200).m(e.t)
	for _, it := range pr["items"].([]any) {
		if it.(map[string]any)["slug"] == slug {
			jobID = it.(map[string]any)["id"].(string)
		}
	}
	l := e.want(e.do(adminTok, "GET", "/v1/applications?job_id="+jobID+"&limit=200", nil), 200).m(e.t)
	items := l["items"].([]any)
	if len(items) == 0 {
		e.t.Fatal("no application stored")
	}
	return items[0].(map[string]any)["id"].(string)
}

// rawDBBytes returns the database file and its WAL as one byte slice, for leak checks.
func (e *env) rawDBBytes() []byte {
	e.t.Helper()
	var all []byte
	for _, suf := range []string{"", "-wal"} {
		b, err := os.ReadFile(e.dbPath + suf)
		if err == nil {
			all = append(all, b...)
		}
	}
	return all
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func ctxBG() context.Context { return context.Background() }

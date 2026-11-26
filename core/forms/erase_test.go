package forms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/dodwmd/companyinacan/schemas/events"
)

const erasedAddr = "Zelda.Erase@Example.COM"

func (h *harness) insert(email string, fields map[string]string) *Lead {
	h.t.Helper()
	l := &Lead{FormID: "contact", Email: email, Name: "Zelda Q", Fields: fields, CreatedAt: h.clock.now(), RetentionUntil: h.clock.now().Add(24 * time.Hour), emailDue: h.clock.now().Add(time.Hour)}
	if err := h.app.store.Insert(context.Background(), l); err != nil {
		h.t.Fatal(err)
	}
	return l
}

func (h *harness) dbBytes() []byte {
	h.t.Helper()
	var all []byte
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(h.app.cfg.DBPath + suffix)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			h.t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestEraseNeedsPermission(t *testing.T) {
	h := newHarness(t, harnessCfg)
	l := h.insert(erasedAddr, map[string]string{"email": erasedAddr})
	for _, c := range []struct{ method, path, body string }{
		{"DELETE", "/v1/leads/" + l.ID, ""},
		{"POST", "/v1/leads/find", `{"email":"a@b.example"}`},
		{"GET", "/v1/erasures", ""},
		{"POST", "/v1/purge", `{"subject":"` + l.ID + `","class":"lead-submission"}`},
	} {
		if rec := h.apiDo(c.method, c.path, readToken, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with the read token: %d", c.method, c.path, rec.Code)
		}
		if rec := h.apiDo(c.method, c.path, "", c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.method, c.path, rec.Code)
		}
		if rec := h.do(h.pub, c.method, c.path, strings.NewReader(c.body), nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the public listener: %d", c.method, c.path, rec.Code)
		}
	}
	if ok, _ := h.app.store.exists(context.Background(), l.ID); !ok {
		t.Fatal("the lead was erased")
	}
}

func TestEraseRemovesScrubsAndRecords(t *testing.T) {
	h := newHarness(t, harnessCfg)
	logs := &lockedBuffer{}
	h.app.log = slog.New(slog.NewTextHandler(logs, nil))
	api := &leadsAPI{store: h.app.store, log: h.app.log, notifier: h.app.dispatcher, auth: func() Authenticator {
		st := &StaticTokens{}
		st.Add(adminToken, Principal{Subject: "user:ops", Permissions: []string{PermLeadsErase}})
		return st
	}()}
	h.api = api.handler()

	keep := h.insert("jimzelda.erase@example.com", map[string]string{"message": "write to zelda.erase@example.com.au"})
	l := h.insert(erasedAddr, map[string]string{"email": erasedAddr, "message": "SECRET-MESSAGE-4711"})
	mention := h.insert("other@example.com", map[string]string{"message": "cc: zelda.erase@example.com."})
	if err := h.app.store.RecordDelivery(context.Background(), l.ID, "email", false, 1, "refused "+erasedAddr); err != nil {
		t.Fatal(err)
	}

	rec := h.apiDo("POST", "/v1/leads/find", adminToken, `{"email":" zelda.erase@EXAMPLE.com "}`)
	var found struct {
		Items     []Match `json:"items"`
		Truncated bool    `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil || rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body, err)
	}
	got := map[string]string{}
	for _, m := range found.Items {
		got[m.ID] = m.Match
	}
	if len(got) != 2 || got[l.ID] != "address" || got[mention.ID] != "mention" || got[keep.ID] != "" {
		t.Fatalf("found %v", got)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "zelda") {
		t.Errorf("a search answers ids only: %s", rec.Body)
	}
	for _, bad := range []string{`{}`, `{"email":"nope"}`, `{"email":"a@b c"}`, `{"email":"a@b.c","x":1}`, `{"email":"a@b.c"}{}`} {
		if rec := h.apiDo("POST", "/v1/leads/find", adminToken, bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}

	if !bytes.Contains(h.dbBytes(), []byte("SECRET-MESSAGE-4711")) {
		t.Fatal("the fixture is not in the database files: the scrub check would prove nothing")
	}
	rec = h.apiDo("DELETE", "/v1/leads/"+l.ID, adminToken, "")
	var out ErasedLead
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Notification != "cancelled" || out.AlreadyErased {
		t.Fatalf("erase: %d %s", rec.Code, rec.Body)
	}
	for _, trace := range []string{"SECRET-MESSAGE-4711", erasedAddr} {
		if bytes.Contains(h.dbBytes(), []byte(trace)) {
			t.Errorf("%q is still in the database files", trace)
		}
	}
	ctx := context.Background()
	for _, table := range []string{"lead_email", "lead_deliveries"} {
		var n int
		if err := h.app.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE lead_id = ?`, l.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d %v", table, n, err)
		}
	}
	// Nothing is sent for it later.
	h.clock.advance(2 * time.Hour)
	if err := h.app.dispatcher.RetryEmails(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range h.mailer.all() {
		if e.SourceID == "lead:"+l.ID {
			t.Error("the notification of an erased lead was sent")
		}
	}

	rec = h.apiDo("DELETE", "/v1/leads/"+l.ID, adminToken, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"already_erased":true`) {
		t.Errorf("repeat: %d %s", rec.Code, rec.Body)
	}
	if rec := h.apiDo("DELETE", "/v1/leads/ld_000000000000000000000000", adminToken, ""); rec.Code != 404 {
		t.Errorf("unknown lead: %d", rec.Code)
	}

	rec = h.apiDo("GET", "/v1/erasures?after=0", adminToken, "")
	var feed struct {
		Items []ErasureEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil || len(feed.Items) != 1 {
		t.Fatal(rec.Body, err)
	}
	e := feed.Items[0]
	if at, err := time.Parse(time.RFC3339, e.At); err != nil || e.Subject != l.ID || e.Class != ClassLead || e.Date != at.Format(time.DateOnly) {
		t.Errorf("tombstone %+v", e)
	}
	if rec := h.apiDo("GET", "/v1/erasures?after=1", adminToken, ""); !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("cursor: %s", rec.Body)
	}

	// The audit trail says who did what, and neither it nor the log holds the address.
	rows, err := h.app.store.db.QueryContext(ctx, `SELECT actor || ' ' || action || ' ' || target || ' ' || count FROM lead_audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		trail = append(trail, s)
	}
	_ = rows.Close()
	want := []string{"user:ops lead.find - 2", "user:ops lead.erase " + l.ID + " 1", "user:ops lead.erase " + l.ID + " 1"}
	if strings.Join(trail, ";") != strings.Join(want, ";") {
		t.Errorf("audit: %q", trail)
	}
	if s := strings.ToLower(logs.String()); strings.Contains(s, "zelda") || !strings.Contains(s, "lead.erase") {
		t.Errorf("log: %s", s)
	}
}

func TestPurgeIsTheIdempotentRestoreHook(t *testing.T) {
	h := newHarness(t, harnessCfg)
	l := h.insert(erasedAddr, nil)
	body := `{"subject":"` + l.ID + `","class":"lead-submission"}`
	for range 2 {
		if rec := h.apiDo("POST", "/v1/purge", adminToken, body); rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	if ok, _ := h.app.store.exists(context.Background(), l.ID); ok {
		t.Fatal("not purged")
	}
	if rec := h.apiDo("POST", "/v1/purge", adminToken, `{"subject":"ld_ffffffffffffffffffffffff","class":"lead-submission"}`); rec.Code != 200 {
		t.Errorf("an unknown lead is a no-op: %d", rec.Code)
	}
	for _, bad := range []string{`{"subject":"x","class":"lead-submission"}`, `{"subject":"` + l.ID + `","class":"other"}`} {
		if rec := h.apiDo("POST", "/v1/purge", adminToken, bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

type blockingMailer struct {
	started chan struct{}
	ended   chan error
}

func (m *blockingMailer) Send(ctx context.Context, _ Email) error {
	m.started <- struct{}{}
	select {
	case <-ctx.Done():
		m.ended <- context.Cause(ctx)
		return ctx.Err()
	case <-time.After(5 * time.Second):
		m.ended <- nil
		return nil
	}
}

func TestEraseStopsWhatIsBeingSentAndQueued(t *testing.T) {
	h := newHarness(t, harnessCfg)
	m := &blockingMailer{started: make(chan struct{}, 1), ended: make(chan error, 1)}
	d := h.app.dispatcher
	d.mailer = m
	form := h.app.cfg.Form("contact")
	l := h.insert(erasedAddr, map[string]string{"email": erasedAddr})
	if !d.Enqueue(l, form) {
		t.Fatal("not queued")
	}
	<-m.started
	rec := h.apiDo("DELETE", "/v1/leads/"+l.ID, adminToken, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"notification_was_under_way":true`) {
		t.Fatal(rec.Code, rec.Body)
	}
	if cause := <-m.ended; !errors.Is(cause, errErased) {
		t.Fatalf("the send was not cancelled: %v", cause)
	}
	d.Flush()
	// Still queued in memory when it was erased: nothing at all is sent.
	if !d.Enqueue(l, form) {
		t.Fatal("not queued")
	}
	d.Flush()
	select {
	case <-m.started:
		t.Fatal("an erased lead was sent")
	default:
	}
}

func TestErasureIsAnnouncedByIDsUntilAccepted(t *testing.T) {
	h := newHarness(t, harnessCfg)
	var mu sync.Mutex
	var bodies []string
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, b.String())
		w.WriteHeader(status)
	}))
	defer srv.Close()
	d := h.app.dispatcher
	d.webhook = &WebhookSender{URL: srv.URL, Secret: []byte("s"), Now: h.clock.now}
	l := h.insert(erasedAddr, map[string]string{"email": erasedAddr})
	ctx := context.Background()
	if _, err := h.app.store.Erase(ctx, "user:ops", l.ID, []string{"webhook"}); err != nil {
		t.Fatal(err)
	}
	for range 3 { // refused, not due yet, accepted
		if err := d.AnnounceErasures(ctx); err != nil {
			t.Fatal(err)
		}
		if err := d.AnnounceErasures(ctx); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(time.Hour)
		mu.Lock()
		if len(bodies) >= 2 {
			status = http.StatusNoContent
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 || bodies[0] != bodies[2] {
		t.Fatalf("deliveries: %q", bodies)
	}
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(events.LeadErasedV1))
	if err != nil {
		t.Fatal(err)
	}
	c := jsch.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("lead.erased.v1.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("lead.erased.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sent, err := jsch.UnmarshalJSON(strings.NewReader(bodies[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(sent); err != nil {
		t.Errorf("the event does not satisfy its schema: %v", err)
	}
	var ev struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &ev); err != nil || ev.Type != EventLeadErased || ev.Data["lead_id"] != l.ID || len(ev.Data) != 3 {
		t.Fatalf("%s %v", bodies[0], err)
	}
	if strings.Contains(strings.ToLower(bodies[0]), "zelda") {
		t.Errorf("personal data in the event: %s", bodies[0])
	}
}

func TestRetentionPurgeLeavesNoTraceAndNoTombstone(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.insert(erasedAddr, map[string]string{"message": "RETENTION-TRACE-99"})
	h.clock.advance(48 * time.Hour)
	if n, err := h.app.Purge(context.Background()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if bytes.Contains(h.dbBytes(), []byte("RETENTION-TRACE-99")) {
		t.Error("the purged lead is still in the database files")
	}
	if es, err := h.app.store.ListErasures(context.Background(), 0); err != nil || len(es) != 0 {
		t.Error(es, err)
	}
}

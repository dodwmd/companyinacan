package leadadapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

const (
	leadA  = "ld_00000000000000000000000a"
	leadB  = "ld_00000000000000000000000b"
	secret = "module-secret"
	casey  = "casey+news@customer.example"
)

// fakeListmonk behaves as Listmonk 6.2.0 did when probed, as far as the adapter calls it.
type fakeListmonk struct {
	optin     string
	subs      map[int64]*subscriber
	onList    map[int64]bool
	next      int64
	mailDown  bool
	subscribe int // public subscriptions
	calls     int
}

func newFakeListmonk() *fakeListmonk {
	return &fakeListmonk{optin: "double", subs: map[int64]*subscriber{}, onList: map[int64]bool{}}
}

func (f *fakeListmonk) byEmail(e string) *subscriber {
	for _, s := range f.subs {
		if strings.EqualFold(s.Email, e) {
			return s
		}
	}
	return nil
}

func (f *fakeListmonk) add(email string, attribs map[string]any) *subscriber {
	f.next++
	s := &subscriber{ID: f.next, Email: email, Attribs: attribs}
	f.subs[s.ID] = s
	return s
}

func data(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }

func (f *fakeListmonk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	public := r.URL.Path == "/api/public/subscription"
	if !public && r.Header.Get("Authorization") != "token can_adapter:tok" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var in struct {
		Email   string         `json:"email"`
		Attribs map[string]any `json:"attribs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch {
	case public:
		f.subscribe++
		s := f.byEmail(in.Email)
		if s == nil {
			s = f.add(in.Email, nil)
		}
		f.onList[s.ID] = true
		if f.mailDown {
			http.Error(w, `{"message":"Error processing request. Please retry."}`, http.StatusInternalServerError)
			return
		}
		data(w, map[string]bool{"has_optin": true})
	case r.URL.Path == "/api/lists":
		data(w, map[string]any{"results": []list{{ID: 3, UUID: "uuid-3", Name: "Newsletter", Type: "public", Optin: f.optin, Status: "active"}}})
	case r.URL.Path == "/api/subscribers" && r.Method == http.MethodPost:
		if f.byEmail(in.Email) != nil {
			http.Error(w, `{"message":"E-mail already exists."}`, http.StatusConflict)
			return
		}
		data(w, f.add(in.Email, in.Attribs))
	case r.URL.Path == "/api/subscribers":
		q := r.URL.Query().Get("search")
		out := []*subscriber{}
		for id, s := range f.subs {
			// Listmonk finds nothing for a "+", and a list role sees its list only.
			if f.onList[id] && !strings.Contains(q, "+") && strings.Contains(s.Email, q) {
				out = append(out, s)
			}
		}
		data(w, map[string]any{"results": out})
	default:
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/subscribers/"), 10, 64)
		if r.Method == http.MethodDelete {
			delete(f.subs, id)
			data(w, true)
			return
		}
		s, ok := f.subs[id]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusBadRequest)
			return
		}
		data(w, s)
	}
}

type fakeSupport struct {
	t      *testing.T
	status int
	opened map[string]int64
	noted  []string
	bodies []string
}

func (f *fakeSupport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := webhook.Verify(secret, r.Header, body, time.Now(), time.Minute); err != nil {
		f.t.Errorf("unsigned request: %v", err)
	}
	f.bodies = append(f.bodies, string(body))
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write(body) // an answer that echoes the personal data it was sent
		return
	}
	var in supportRequest
	_ = json.Unmarshal(body, &in)
	if r.URL.Path == "/hooks/lead-erased" {
		f.noted = append(f.noted, in.LeadID)
		_ = json.NewEncoder(w).Encode(supportAnswer{f.opened[in.LeadID]})
		return
	}
	if f.opened[in.LeadID] == 0 {
		f.opened[in.LeadID] = int64(len(f.opened) + 1)
	}
	_ = json.NewEncoder(w).Encode(supportAnswer{f.opened[in.LeadID]})
}

type rig struct {
	t       *testing.T
	a       *Adapter
	lm      *fakeListmonk
	sup     *fakeSupport
	lmURL   string
	supURL  string
	leads   map[string]Lead
	intake  config.LeadIntake
	creds   Credentials
	eventID int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := &rig{t: t, lm: newFakeListmonk(), sup: &fakeSupport{t: t, opened: map[string]int64{}}, leads: map[string]Lead{},
		intake: config.LeadIntake{SupportForms: []string{"contact"}, NewsletterForms: []string{"contact"}, NewsletterList: "newsletter"},
		creds:  Credentials{"can_adapter", "tok"}}
	lm, sup := httptest.NewServer(r.lm), httptest.NewServer(r.sup)
	t.Cleanup(lm.Close)
	t.Cleanup(sup.Close)
	r.lmURL, r.supURL = lm.URL, sup.URL
	r.a = &Adapter{DB: db, Audit: audit.New(db), Log: slog.New(slog.DiscardHandler), Now: time.Now, Client: webhook.NewHTTPClient(5 * time.Second),
		Intake: func(context.Context) (config.LeadIntake, error) { return r.intake, nil },
		Lead: func(_ context.Context, id string) (Lead, error) {
			l, ok := r.leads[id]
			if !ok {
				return Lead{}, ErrNoLead
			}
			return l, nil
		},
		Newsletter: func() (Credentials, error) { return r.creds, nil },
	}
	return r
}

func (r *rig) lead(id string, consent bool) {
	l := Lead{FormID: "contact", Email: casey, Name: "Casey Customer", Fields: map[string]string{"message": "a private message"}}
	if consent {
		l.Consents = []string{"newsletter"}
	}
	r.leads[id] = l
}

func (r *rig) deliver(module, eventType, leadID string) string {
	r.t.Helper()
	url, path := r.lmURL, "/hooks/lead"
	if eventType == EventErased {
		path = "/hooks/lead-erased"
	}
	if module == Support {
		url = r.supURL
	}
	r.eventID++
	status, msg := r.a.Deliver(context.Background(), store.Delivery{ModuleID: module, HMACSecret: secret,
		Event: store.Event{ID: "evt_" + strconv.Itoa(r.eventID), Type: eventType, Source: "forms", DataJSON: `{"lead_id":"` + leadID + `","form_id":"contact"}`}}, url+path)
	if (status == http.StatusOK) != (msg == "") {
		r.t.Fatalf("status %d with message %q", status, msg)
	}
	for _, pii := range []string{"casey", "Casey", "private message"} {
		if strings.Contains(msg, pii) {
			r.t.Fatalf("the recorded error holds personal data: %q", msg)
		}
	}
	return msg
}

func (r *rig) ok(module, eventType, leadID string) {
	r.t.Helper()
	if msg := r.deliver(module, eventType, leadID); msg != "" {
		r.t.Fatalf("%s %s: %s", module, eventType, msg)
	}
}

// actions are the audited actions; no entry may hold anything of the lead.
func (r *rig) actions() []string {
	r.t.Helper()
	recs, err := r.a.Audit.List(context.Background(), 0, 100)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, x := range recs {
		if s := x.Target + x.Detail; strings.Contains(strings.ToLower(s), "casey") {
			r.t.Fatalf("audit holds personal data: %s", s)
		}
		out = append(out, x.Action)
	}
	return out
}

func TestNeverSubscribesWithoutConsent(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, false)
	r.ok(Newsletter, EventCreated, leadA)
	// A text field of the same name is not consent.
	l := r.leads[leadA]
	l.Fields = map[string]string{"newsletter": "yes"}
	r.leads[leadA] = l
	r.ok(Newsletter, EventCreated, leadA)
	// Consent, but the form is not named.
	r.lead(leadA, true)
	r.intake.NewsletterForms = nil
	r.ok(Newsletter, EventCreated, leadA)
	if r.lm.calls != 0 {
		t.Fatalf("Listmonk was called %d times", r.lm.calls)
	}
}

func TestSingleOptInListIsRefused(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, true)
	r.lm.optin = "single"
	if msg := r.deliver(Newsletter, EventCreated, leadA); !strings.Contains(msg, "not double opt-in") {
		t.Fatalf("msg = %q", msg)
	}
	if len(r.lm.subs) != 0 || r.lm.subscribe != 0 {
		t.Fatal("somebody was subscribed")
	}
}

func TestSubscribeOnceThenEraseDeletes(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, true)
	r.ok(Newsletter, EventCreated, leadA)
	r.ok(Newsletter, EventCreated, leadA)
	if len(r.lm.subs) != 1 || r.lm.subscribe != 1 {
		t.Fatalf("subscribers %d, subscriptions %d", len(r.lm.subs), r.lm.subscribe)
	}
	delete(r.leads, leadA)
	r.ok(Newsletter, EventErased, leadA)
	r.ok(Newsletter, EventErased, leadA)
	if len(r.lm.subs) != 0 {
		t.Fatal("the subscriber is still there")
	}
	if got := r.actions(); !slices.Contains(got, "lead.forward") || !slices.Contains(got, "lead.erase_copy") {
		t.Fatalf("audit %v", got)
	}
}

// A failed confirmation mail is retried, and the record it left behind can be erased meanwhile.
func TestMailFailureIsRetriedAndErasable(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, true)
	r.lm.mailDown = true
	if msg := r.deliver(Newsletter, EventCreated, leadA); !strings.Contains(msg, "SMTP") {
		t.Fatalf("msg = %q", msg)
	}
	r.ok(Newsletter, EventErased, leadA)
	if len(r.lm.subs) != 0 {
		t.Fatal("the subscriber is still there")
	}
}

func TestRecordThatExistedBeforeIsNotDeleted(t *testing.T) {
	r := newRig(t)
	s := r.lm.add(casey, nil)
	r.lm.onList[s.ID] = true
	r.lead(leadA, true)
	r.ok(Newsletter, EventCreated, leadA) // found despite the "+"
	r.ok(Newsletter, EventErased, leadA)
	if len(r.lm.subs) != 1 {
		t.Fatal("a record the lead did not make was deleted")
	}
	if got := r.actions(); !slices.Contains(got, "lead.copy_kept") {
		t.Fatalf("audit %v", got)
	}
}

func TestRecordSharedByTwoLeadsGoesWithTheLast(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, true)
	r.lead(leadB, true)
	r.ok(Newsletter, EventCreated, leadA)
	r.ok(Newsletter, EventCreated, leadB)
	r.ok(Newsletter, EventErased, leadA)
	if len(r.lm.subs) != 1 {
		t.Fatal("deleted while another lead still stands for it")
	}
	r.ok(Newsletter, EventErased, leadB)
	if len(r.lm.subs) != 0 {
		t.Fatal("the subscriber is still there")
	}
}

func TestMissingAPIUserIsVisible(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, true)
	r.creds = Credentials{}
	if msg := r.deliver(Newsletter, EventCreated, leadA); !strings.Contains(msg, "can-newsletter-api") {
		t.Fatalf("msg = %q", msg)
	}
	r.creds = Credentials{"can_adapter", "wrong"}
	if msg := r.deliver(Newsletter, EventCreated, leadA); !strings.Contains(msg, "refused the API user") {
		t.Fatalf("msg = %q", msg)
	}
}

func TestSupportConversationAndErasureNote(t *testing.T) {
	r := newRig(t)
	r.lead(leadA, false)
	r.sup.status = http.StatusInternalServerError
	if msg := r.deliver(Support, EventCreated, leadA); msg != "the support module answered HTTP 500" {
		t.Fatalf("msg = %q", msg)
	}
	r.sup.status = 0
	r.ok(Support, EventCreated, leadA)
	r.ok(Support, EventCreated, leadA)
	if len(r.sup.bodies) != 2 {
		t.Fatalf("%d requests", len(r.sup.bodies))
	}
	delete(r.leads, leadA)
	r.ok(Support, EventErased, leadA)
	if !slices.Equal(r.sup.noted, []string{leadA}) || r.sup.bodies[2] != `{"lead_id":"`+leadA+`"}` {
		t.Fatalf("noted %v, body %s", r.sup.noted, r.sup.bodies[2])
	}
	if got := r.actions(); !slices.Contains(got, "lead.copy_kept") {
		t.Fatalf("audit %v", got)
	}
}

func TestNothingIsAskedWhereNothingWasMade(t *testing.T) {
	r := newRig(t)
	r.intake = config.LeadIntake{}
	r.lead(leadA, true)
	for _, m := range []string{Support, Newsletter} {
		r.ok(m, EventCreated, leadA)
		r.ok(m, EventErased, leadA)
	}
	// A lead erased before it was delivered is not handed on.
	r.intake = config.LeadIntake{SupportForms: []string{"contact"}}
	r.ok(Support, EventCreated, leadB)
	// An event that does not come from forms is ignored.
	status, _ := r.a.Deliver(context.Background(), store.Delivery{ModuleID: Support, HMACSecret: secret,
		Event: store.Event{Type: EventCreated, Source: "crm", DataJSON: `{"lead_id":"` + leadA + `","form_id":"contact"}`}}, r.supURL)
	if status != http.StatusOK || len(r.sup.bodies) != 0 || r.lm.calls != 0 {
		t.Fatalf("calls: support %d, listmonk %d", len(r.sup.bodies), r.lm.calls)
	}
}

// Through the outbox: retried, dead-lettered and requeued like any delivery; the erasure of a lead
// reaches a module that is disabled, once it is enabled again, however old the event.
func TestOutboxRetriesDeadLettersAndErasureOfDisabledModule(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	db := r.a.DB
	m := config.Module{ID: Support}
	m.Subscribes = slices.Grow(m.Subscribes, 1)[:1]
	m.Subscribes[0].Event, m.Subscribes[0].Service, m.Subscribes[0].Port, m.Subscribes[0].Webhook, m.Subscribes[0].Via = EventErased, "support-leads", 8081, "/hooks/lead-erased", config.ViaCanCore
	rec := store.Module{ID: Support, Version: "1", Namespace: "mod-support", ServiceAccount: "support", State: store.StateEnabled, HMACSecret: secret, Manifest: m}
	if err := db.UpsertModule(ctx, rec, nil, []store.Subscription{{ModuleID: Support, Event: EventCreated, Service: "support-leads", Port: 8081, Webhook: "/hooks/lead", Via: config.ViaCanCore}}); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(db)
	bus.Extra = r.a.Targets
	disp := webhook.NewDispatcher(db, http.DefaultClient)
	disp.Log = r.a.Log
	disp.MaxAttempts, disp.Backoff = 2, func(int) time.Duration { return 0 }
	var urls []string
	disp.Adapter = func(ctx context.Context, del store.Delivery, url string) (int, string) {
		urls = append(urls, url)
		return r.a.Deliver(ctx, del, r.supURL+url[strings.LastIndex(url, "/hooks/"):])
	}
	r.lead(leadA, false)
	r.sup.status = http.StatusBadGateway
	if _, err := bus.Publish(ctx, "forms", EventCreated, json.RawMessage(`{"lead_id":"`+leadA+`","form_id":"contact"}`)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := disp.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ds, err := db.ListDeliveries(ctx, 10)
	if err != nil || len(ds) != 1 || ds[0].Status != store.StatusDead || len(urls) != 2 ||
		urls[0] != "http://support-leads.mod-support.svc.cluster.local:8081/hooks/lead" {
		t.Fatalf("%+v %v %v", ds, urls, err)
	}
	r.sup.status = 0
	if err := db.RequeueDelivery(ctx, ds[0].ID, db.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := disp.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if l, err := db.GetLeadLink(ctx, Support, leadA); err != nil || !l.Done {
		t.Fatalf("link %+v %v", l, err)
	}

	if err := db.SetModuleState(ctx, Support, store.StateDisabled, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Publish(ctx, "forms", EventErased, json.RawMessage(`{"lead_id":"`+leadA+`","form_id":"contact"}`)); err != nil {
		t.Fatal(err)
	}
	if n, err := disp.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("delivered to a disabled module: %d %v", n, err)
	}
	if _, err := db.PruneEvents(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetModuleState(ctx, Support, store.StateEnabled, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := disp.RunOnce(ctx); err != nil || n != 1 || !slices.Equal(r.sup.noted, []string{leadA}) {
		t.Fatalf("erasure not delivered: %d %v %v", n, err, r.sup.noted)
	}
}

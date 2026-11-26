package notify

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func (e *env) module(id string, publishes []string, rules ...config.NotifyRule) {
	e.t.Helper()
	m := config.Module{ID: id, Notifications: rules}
	for _, p := range publishes {
		m.Publishes = append(m.Publishes, struct {
			Event  string `yaml:"event"`
			Schema string `yaml:"schema"`
		}{Event: p})
	}
	if err := e.db.UpsertModule(context.Background(), store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, Manifest: m}, nil, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) publish(source, typ, data string) {
	e.t.Helper()
	if _, err := e.bus.Publish(context.Background(), source, typ, json.RawMessage(data)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) inbox(subject string) []Item {
	e.t.Helper()
	items, err := e.s.List(context.Background(), subject, ListOptions{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return items
}

// A flood of public applications must not use up what an approval needs, and is itself digested.
func TestPublicSpamDoesNotCrowdOutApprovals(t *testing.T) {
	p := newPushEnv(t)
	e := p.env
	ctx := context.Background()
	e.module("jobs", []string{"application.received.v1"}, config.NotifyRule{Event: "application.received.v1", Recipients: []string{"role:recruiter"}})
	e.roles["jobs:recruiter"] = []string{"user:rita"}
	e.setEmail("user:rita", "rita@example.com")
	p.subscribe("user:rita", ep1)
	e.bus.SubscribeAll(e.s.HandleEvent)
	for i := range MaxInAppPerHour + 50 {
		e.publish("jobs", "application.received.v1", `{"application_id":"app_`+strconv.Itoa(i)+`","job_id":"job_1"}`)
		e.s.WaitPush()
	}
	if n := len(e.mail.Sent()); n != PublicBurst {
		t.Fatalf("%d e-mails one by one, want %d", n, PublicBurst)
	}
	if n := len(p.send.Calls()); n != PublicBurst {
		t.Fatalf("%d pushes, want %d", n, PublicBurst)
	}
	for _, typ := range []string{"approval.requested", "approval.escalated"} {
		// Approvals carry their module, as the engine sends them.
		_, created, err := e.s.Notify(ctx, Notice{Subject: "user:rita", Type: typ, DedupeKey: typ, Module: "jobs"})
		e.s.WaitPush()
		if err != nil || !created {
			t.Fatalf("%s was dropped: %v %v", typ, created, err)
		}
	}
	if n := len(e.mail.Sent()); n != PublicBurst+2 {
		t.Fatalf("the approvals were not mailed: %d", n)
	}
	if n := len(p.send.Calls()); n != PublicBurst+2 {
		t.Fatalf("the approvals were not pushed: %d", n)
	}
	// The held ones go out as one digest an hour later, not before.
	if n, err := e.s.FlushDigests(ctx); err != nil || n != 0 {
		t.Fatalf("flushed early: %d %v", n, err)
	}
	e.now = e.now.Add(time.Hour + time.Minute)
	if n, err := e.s.FlushDigests(ctx); err != nil || n != 1 {
		t.Fatalf("digest: %d %v", n, err)
	}
	sent := e.mail.Sent()
	last := sent[len(sent)-1]
	if want := MaxInAppPerHour - PublicBurst; last.Count != want || !strings.Contains(last.Subject, strconv.Itoa(want)) {
		t.Fatalf("digest of %d (%q), want %d", last.Count, last.Subject, want)
	}
	if n, err := e.s.FlushDigests(ctx); err != nil || n != 0 {
		t.Fatalf("a second digest: %d %v", n, err)
	}
	// The digest does not use up the hour's allowance of what comes after it.
	before := len(e.mail.Sent())
	e.publish("jobs", "application.received.v1", `{"application_id":"app_later","job_id":"job_1"}`)
	if len(e.mail.Sent()) != before+1 {
		t.Fatal("the next application was not mailed")
	}
}

// A third-party module that publishes a first-party event name sets off nobody else's rules, and
// does not get the project's wording for its own.
func TestThirdPartyCannotRaiseAnotherModulesNotices(t *testing.T) {
	e := newEnv(t)
	e.module("finance", []string{"invoice.overdue.v1"}, config.NotifyRule{Event: "invoice.overdue.v1", Recipients: []string{"role:finance", "field:owner"}})
	e.module("acme-widgets", []string{"invoice.overdue.v1"}, config.NotifyRule{Event: "invoice.overdue.v1", Recipients: []string{"user:user:wes"}})
	e.roles["finance:finance"] = []string{"user:fin"}
	e.bus.SubscribeAll(e.s.HandleEvent)

	e.publish("acme-widgets", "invoice.overdue.v1", `{"invoice_id":1,"owner":"user:victim"}`)
	if n := len(e.inbox("user:fin")) + len(e.inbox("user:victim")); n != 0 {
		t.Fatalf("finance's rule ran for another publisher: %d", n)
	}
	own := e.inbox("user:wes")
	if len(own) != 1 || own[0].Type != "acme-widgets.invoice.overdue" || strings.Contains(own[0].Title+own[0].Body, "Finance") {
		t.Fatalf("%+v", own)
	}
	e.publish("finance", "invoice.overdue.v1", `{"invoice_id":2}`)
	if n := len(e.inbox("user:wes")); n != 1 {
		t.Fatalf("the third party's rule on its own event ran for finance's: %d", n)
	}
	e.publish("can-core", "invoice.overdue.v1", `{"invoice_id":3}`)
	if got := e.inbox("user:fin"); len(got) != 2 || got[0].Type != "invoice.overdue" {
		t.Fatalf("%+v", got)
	}
}

func TestP0IsNotRepeatedWithinTheHour(t *testing.T) {
	e := newEnv(t)
	e.module("work", []string{"work.issue.p0.v1"}, config.NotifyRule{Event: "work.issue.p0.v1", Recipients: []string{"field:assignee"}, Urgent: true})
	e.bus.SubscribeAll(e.s.HandleEvent)
	raise := func(issue, who string) {
		e.publish("work", "work.issue.p0.v1", `{"issue_id":"`+issue+`","project":"WEB","assignee":"`+who+`"}`)
	}
	raise("WEB-1", "user:ann")
	raise("WEB-1", "user:ann")
	raise("WEB-1", "user:ben")
	raise("WEB-1", "user:ann")
	raise("WEB-2", "user:ann")
	if a, b := len(e.inbox("user:ann")), len(e.inbox("user:ben")); a != 2 || b != 1 {
		t.Fatalf("ann %d ben %d", a, b)
	}
	e.now = e.now.Add(time.Hour + time.Minute)
	raise("WEB-1", "user:ann")
	if n := len(e.inbox("user:ann")); n != 3 {
		t.Fatalf("after an hour: %d", n)
	}
}

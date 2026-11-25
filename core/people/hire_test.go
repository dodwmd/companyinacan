package people

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient"
)

func hiredEvent(id, cand string) modclient.Event {
	d, _ := json.Marshal(map[string]any{"candidate_id": cand, "application_id": "app_1", "job_id": "job_1", "job_title": "Engineer", "department": "Engineering", "employment_type": "full_time", "actor": "user:alice", "hired_at": "2026-03-10T09:00:00Z"})
	return modclient.Event{ID: id, Type: "candidate.hired.v1", Source: "jobs", Data: d}
}

func TestCandidateHiredStartsOnboardingWithAChecklist(t *testing.T) {
	e := newEnv(t)
	h := e.s.CanEventHandler()
	e.must(h(e.ctx, hiredEvent("evt_a", "cand_0123456789ab")))

	people, _, err := e.s.ListPeople(e.ctx, e.hr, PersonFilter{Status: StOnboarding})
	e.must(err)
	if len(people) != 1 {
		t.Fatalf("%d people in onboarding", len(people))
	}
	p := people[0]
	if p.Status != StOnboarding || p.Kind != "employee" {
		t.Fatalf("%+v", p)
	}
	for _, v := range []string{p.GivenName, p.FamilyName, p.WorkEmail} {
		if strings.Contains(v, "Engineer") || strings.Contains(v, "alice") {
			t.Fatalf("the hand-off carries no personal data, but the record has %q", v)
		}
	}
	cls, err := e.s.ListChecklists(e.ctx, e.hr, p.ID)
	e.must(err)
	if len(cls) != 1 || cls[0].Kind != "onboarding" || len(cls[0].Tasks) == 0 {
		t.Fatalf("no onboarding checklist: %+v", cls)
	}
}

func TestCandidateHiredIsIdempotentPerCandidate(t *testing.T) {
	e := newEnv(t)
	h := e.s.CanEventHandler()
	for i := 0; i < 3; i++ { // redelivery under a new event id too: the candidate is the key
		e.must(h(e.ctx, hiredEvent("evt_"+string(rune('a'+i)), "cand_0123456789ab")))
	}
	people, _, _ := e.s.ListPeople(e.ctx, e.hr, PersonFilter{})
	if len(people) != 1 {
		t.Fatalf("%d people for one hire", len(people))
	}
	if cls, _ := e.s.ListChecklists(e.ctx, e.hr, people[0].ID); len(cls) != 1 {
		t.Fatalf("%d checklists", len(cls))
	}
	e.must(h(e.ctx, hiredEvent("evt_z", "cand_ffffffffffff")))
	if people, _, _ = e.s.ListPeople(e.ctx, e.hr, PersonFilter{}); len(people) != 2 {
		t.Fatalf("a second candidate must be a second person: %d", len(people))
	}
}

func TestContractorsAreHiredAsContractors(t *testing.T) {
	e := newEnv(t)
	var d map[string]any
	ev := hiredEvent("evt_c", "cand_0123456789ab")
	_ = json.Unmarshal(ev.Data, &d)
	d["employment_type"] = "contract"
	ev.Data, _ = json.Marshal(d)
	e.must(e.s.CanEventHandler()(e.ctx, ev))
	people, _, _ := e.s.ListPeople(e.ctx, e.hr, PersonFilter{})
	if len(people) != 1 || people[0].Kind != "contractor" {
		t.Fatalf("%+v", people)
	}
}

func TestMalformedHireEventsAreDroppedNotRetried(t *testing.T) {
	e := newEnv(t)
	h := e.s.CanEventHandler()
	for _, data := range []string{`{}`, `{"candidate_id":""}`, `{"candidate_id":"has space"}`, `{"candidate_id":"` + strings.Repeat("a", 200) + `"}`, `not json`} {
		ev := modclient.Event{ID: "evt_m", Type: "candidate.hired.v1", Data: json.RawMessage(data)}
		if err := h(e.ctx, ev); err != nil {
			t.Errorf("%s: a malformed event must be dropped (can-core would retry it forever): %v", data, err)
		}
	}
	if people, _, _ := e.s.ListPeople(e.ctx, e.hr, PersonFilter{}); len(people) != 0 {
		t.Fatalf("%d people from malformed events", len(people))
	}
	if err := h(e.ctx, modclient.Event{ID: "evt_o", Type: "lead.created.v1", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestHireDoesNotDuplicateAnExistingDefaultTemplate(t *testing.T) {
	e := newEnv(t)
	h := e.s.CanEventHandler()
	e.must(h(e.ctx, hiredEvent("evt_a", "cand_aaaaaaaaaaaa")))
	e.must(h(e.ctx, hiredEvent("evt_b", "cand_bbbbbbbbbbbb")))
	ts, err := e.s.ListTemplates(e.ctx, e.hr)
	e.must(err)
	n := 0
	for _, t := range ts {
		if t.AutoStatus == string(StOnboarding) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d onboarding templates", n)
	}
	_ = context.Background
}

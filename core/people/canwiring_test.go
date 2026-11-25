package people

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const peoplePAT = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func TestRolesFromPermissions(t *testing.T) {
	cases := []struct {
		perms []string
		want  string
	}{
		{[]string{"people.read", "people.sensitive-read", "people.sensitive-write", "performance.read", "performance.write"}, "employee"},
		{[]string{"people.read", "performance.read", "performance.write"}, "employee,manager"},
		{[]string{"people.read", "people.write", "people.sensitive-read", "people.sensitive-write", "people.admin"}, "employee,hr,admin"},
		{[]string{"people.read"}, "employee"},
		{[]string{"people.write", "people.admin"}, ""}, // no directory read: no role at all
		{[]string{"performance.write"}, ""},
		{[]string{"work.admin", "finance.admin"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := strings.Join(rolesFromPermissions(c.perms), ","); got != c.want {
			t.Errorf("%v -> %q, want %q", c.perms, got, c.want)
		}
	}
}

type canRig struct {
	core *fakecore.Core
	svc  *Service
	api  *httptest.Server
	disp *Dispatcher
}

func newCanRig(t *testing.T) *canRig {
	t.Helper()
	e := newEnv(t)
	core := fakecore.New(t, "people")
	can := core.Client(t)
	*e.clock = time.Now()
	st := &StaticTokens{}
	st.Add("svc-token-0123456789abcdefghij", Principal{Subject: "svc-can-core", Roles: []string{RoleHR, RoleAdmin}})
	auth := MultiAuth{st, &CanAuth{In: &modclient.Introspector{API: can}}}
	recv := &modclient.Receiver{Secret: canSecretForTests, Handle: e.s.CanEventHandler()}
	api := httptest.NewServer((&API{Svc: e.s, Auth: auth, Log: discardLog(), Can: recv}).Handler())
	t.Cleanup(api.Close)
	d := NewDispatcher("", nil, discardLog())
	d.Can = can
	return &canRig{core: core, svc: e.s, api: api, disp: d}
}

const canSecretForTests = "can-hook-secret-0123456789abcdef"

func (r *canRig) get(t *testing.T, tok, path string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", r.api.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestPersonalAccessTokenReachesTheDirectoryByRole(t *testing.T) {
	r := newCanRig(t)
	r.core.AddToken(peoplePAT, fakecore.Token{Subject: "user:alice", Permissions: []string{"people.read", "people.write", "people.sensitive-read", "people.sensitive-write", "people.admin"}})
	if c := r.get(t, peoplePAT, "/v1/people"); c != 200 {
		t.Fatalf("hr token = %d", c)
	}
	r.core.AddToken(peoplePAT+"W", fakecore.Token{Subject: "user:bob", Permissions: []string{"work.admin"}})
	r.core.AddToken(peoplePAT+"F", fakecore.Token{Subject: "user:eve", Permissions: []string{"people.admin"}, Module: "jobs"})
	for name, tok := range map[string]string{"unknown": peoplePAT + "?", "no people permission": peoplePAT + "W", "foreign": peoplePAT + "F"} {
		if c := r.get(t, tok, "/v1/people"); c != 401 {
			t.Errorf("%s = %d", name, c)
		}
	}
	if c := r.get(t, "svc-token-0123456789abcdefghij", "/v1/people"); c != 200 {
		t.Fatalf("service token = %d", c)
	}
	r.core.SetDown(true)
	r.core.AddToken(peoplePAT+"N", fakecore.Token{Subject: "user:new", Permissions: []string{"people.read"}})
	if c := r.get(t, peoplePAT+"N", "/v1/people"); c != 401 {
		t.Fatalf("can-core down: %d", c)
	}
}

func TestSignedHireEventReachesTheModuleAndAnUnsignedOneDoesNot(t *testing.T) {
	r := newCanRig(t)
	ev := hiredEvent("evt_sig", "cand_0123456789ab")
	if code, err := fakecore.Deliver(context.Background(), r.api.URL+"/v1/can/events", "wrong-secret-wrong-secret", ev); err != nil || code != 401 {
		t.Fatalf("%d %v", code, err)
	}
	if people, _, _ := r.svc.ListPeople(context.Background(), hireActor, PersonFilter{}); len(people) != 0 {
		t.Fatal("an unsigned hire event created a person")
	}
	if code, err := fakecore.Deliver(context.Background(), r.api.URL+"/v1/can/events", canSecretForTests, ev); err != nil || code != 204 {
		t.Fatalf("%d %v", code, err)
	}
	if people, _, _ := r.svc.ListPeople(context.Background(), hireActor, PersonFilter{Status: StOnboarding}); len(people) != 1 {
		t.Fatalf("%d people", len(people))
	}
}

func TestPeopleEventsGoThroughCanCoreAndLeaveIsMasked(t *testing.T) {
	r := newCanRig(t)
	ctx := context.Background()
	p, err := r.svc.CreatePerson(ctx, hireActor, PersonInput{Kind: "employee", Status: StActive, GivenName: "Test", FamilyName: "Person"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeStatus(ctx, hireActor, p.ID, StLeave, "", "private note"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.svc.DeliverPending(ctx, r.disp); err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
	var created bool
	for _, e := range r.core.Published() {
		created = created || e.Type == "person.created.v1"
		s := string(e.Data)
		if strings.Contains(s, `"leave"`) || strings.Contains(s, "private note") || strings.Contains(s, "Test") || strings.Contains(s, "Person") {
			t.Errorf("event %s leaks leave or personal data: %s", e.Type, s)
		}
	}
	if !created {
		t.Fatalf("person.created.v1 not published: %+v", r.core.Published())
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

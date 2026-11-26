package provision

// A person this module holds no account for (imported existing staff, accounts made by hand):
// a leaver or mover must remove or report their access, never skip it and report success.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/csvimport"
	"github.com/dodwmd/companyinacan/core/people"
)

const leftOn = "2026-06-10"

func offboarded(party string) map[string]any {
	return map[string]any{"party_id": party, "kind": "employee", "effective_date": leftOn}
}

func step(r Run, connector string) Step {
	for _, s := range r.Steps {
		if s.Connector == connector && (s.Op == opDisableAccount || s.Op == opUpdateAccount || s.Op == opRevokeCore) {
			return s
		}
	}
	return Step{}
}

// scimHarness has a SCIM connector "idp" (it can look accounts up) and the plain fake "app-b"
// (it cannot). No account is recorded for anybody.
func scimHarness(t *testing.T) (*harness, *fakeSCIMServer) {
	t.Helper()
	h := newHarness(t, nil)
	f := newFakeSCIMServer()
	c, _ := newSCIMClient(t, f)
	cs, err := NewConnectorSet(c, h.b)
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Conns = cs
	return h, f
}

// The whole path: a CSV import without joiners, then an offboarding.
func TestImportedPersonWhoLeavesIsDeprovisionedOrFlagged(t *testing.T) {
	ctx := context.Background()
	kr, err := people.ParseKeyring("k1 " + strings.Repeat("ab", 32) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	h, f := scimHarness(t)
	svc, err := people.Open(filepath.Join(t.TempDir(), "people.db"), people.Options{Keyring: kr, Now: h.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	hr := people.Actor{Subject: "svc-hr", Roles: []string{people.RoleHR, people.RoleAdmin}}
	rep, err := svc.ImportPeople(ctx, hr, strings.NewReader("external_id,given_name,family_name,work_email\nE1,Ann,Example,ann@example.com\n"), csvimport.Options{Apply: true})
	if err != nil || !rep.Applied {
		t.Fatalf("import: %+v %v", rep, err)
	}
	id := rep.Items[0].ID
	for _, to := range []people.Status{people.StOffboarding, people.StFormer} {
		if _, err := svc.ChangeStatus(ctx, hr, id, to, leftOn, ""); err != nil {
			t.Fatal(err)
		}
	}
	tokens := &people.StaticTokens{}
	tokens.Add("directory-token-0123456789abcdef", people.Principal{Subject: "svc-prov", Roles: []string{people.RoleHR}})
	srv := httptest.NewServer((&people.API{Svc: svc, Auth: tokens, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(srv.Close)
	h.eng.Dir = &HTTPDirectory{BaseURL: srv.URL, Token: "directory-token-0123456789abcdef", HTTP: &http.Client{Timeout: 5 * time.Second}}
	// Ann has had an account at the identity provider for years.
	f.users["u-ann"] = map[string]any{"userName": "Ann@Example.com", "active": true}

	h.mustEvent(EventPersonOffboarded, offboarded(id))
	h.settle()
	r := h.onlyRun()
	if f.users["u-ann"]["active"] != false {
		t.Fatalf("the account is still active; steps %+v", r.Steps)
	}
	if s := step(r, "idp"); s.State != StepDone {
		t.Errorf("idp step %+v", s)
	}
	if s := step(r, "app-b"); s.State != StepDead || s.ErrorCode != "manual_check_required" {
		t.Errorf("a connector that cannot look up must be flagged: %+v", s)
	}
	if s := step(r, connCore); s.State != StepDead || s.ErrorCode != "no_core_account" {
		t.Errorf("no can-core account to revoke must be flagged: %+v", s)
	}
	if r.State != RunFailed {
		t.Fatalf("the run reports %s", r.State)
	}
	dead, err := h.st.DeadLetters(ctx, 10)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead letters %v %v", dead, err)
	}
	if len(h.b.calls) != 0 {
		t.Error("a connector without a known account was called")
	}
	if ext, state, err := h.st.accountFor(ctx, id, "idp"); err != nil || ext != "u-ann" || state != "disabled" {
		t.Errorf("the account found is not recorded: %q %q %v", ext, state, err)
	}
	for _, o := range h.outbox() {
		if strings.Contains(o, "example.com") {
			t.Errorf("event leaks the login: %s", o)
		}
	}
}

func TestUnlinkedLeaverNeverDisablesADoubtfulMatch(t *testing.T) {
	for name, c := range map[string]struct {
		users map[string]map[string]any
		code  string
		also  func(h *harness)
	}{
		"two accounts":               {users: map[string]map[string]any{"u1": {"userName": "alice@example.com"}, "u2": {"userName": "alice@example.com"}}, code: "ambiguous"},
		"marked as another person's": {users: map[string]map[string]any{"u1": {"userName": "alice@example.com", "externalId": bob}}, code: "identity_mismatch"},
		"recorded for another person": {users: map[string]map[string]any{"u1": {"userName": "alice@example.com"}}, code: "identity_mismatch",
			also: func(h *harness) { _ = h.st.LinkAccount(context.Background(), "test", bob, "idp", "u1") }},
		"login shared with a current person": {users: map[string]map[string]any{"u1": {"userName": "alice@example.com"}}, code: "shared_login",
			also: func(h *harness) { h.dir.put(person(bob, "active", "ALICE@example.com")) }},
	} {
		t.Run(name, func(t *testing.T) {
			h, f := scimHarness(t)
			h.dir.put(person(alice, "former", "alice@example.com"))
			f.users = c.users
			if c.also != nil {
				c.also(h)
			}
			h.mustEvent(EventPersonOffboarded, offboarded(alice))
			h.settle()
			r := h.onlyRun()
			if s := step(r, "idp"); s.State != StepDead || s.ErrorCode != c.code || r.State != RunFailed {
				t.Fatalf("state %s step %+v", r.State, s)
			}
			for _, l := range f.log {
				if strings.HasPrefix(l, "PATCH") {
					t.Fatalf("a doubtful account was changed: %v", f.log)
				}
			}
		})
	}
}

func TestUnlinkedLeaverWithNoAccountSucceeds(t *testing.T) {
	h, f := scimHarness(t)
	cs, _ := NewConnectorSet(&SCIMClient{ConnName: "idp", Base: mustBase(t, f), Token: "downstream-secret-token", HTTP: NewOutboundClient(true, 5*time.Second)})
	h.eng.Conns = cs
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.mustEvent(EventPersonOffboarded, offboarded(alice))
	h.settle()
	r := h.onlyRun()
	if s := step(r, "idp"); r.State != RunSucceeded || s.State != StepSkipped || s.ErrorCode != "no_account_found" {
		t.Fatalf("state %s step %+v", r.State, s)
	}
}

func mustBase(t *testing.T, f *fakeSCIMServer) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL + "/scim/v2"
}

func TestUnlinkedPersonWithoutAWorkEmailIsFlagged(t *testing.T) {
	h, _ := scimHarness(t)
	h.dir.put(person(alice, "former", ""))
	h.mustEvent(EventPersonOffboarded, offboarded(alice))
	h.settle()
	if s := step(h.onlyRun(), "idp"); s.State != StepDead || s.ErrorCode != "manual_check_required" {
		t.Fatalf("step %+v", s)
	}
}

// An administrator links the account and retries.
func TestLinkingTheAccountAndRetryingClearsTheFlag(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.mustEvent(EventPersonOffboarded, offboarded(alice))
	h.settle()
	r := h.onlyRun()
	if r.State != RunFailed || step(r, "app-a").ErrorCode != "manual_check_required" {
		t.Fatalf("state %s %+v", r.State, r.Steps)
	}
	h.linkAccounts(alice, "app-a", "app-b")
	if err := h.eng.RetryRun(context.Background(), "user:admin", r.ID); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if r = h.onlyRun(); r.State != RunSucceeded || h.a.count(opDisableAccount) != 1 || h.b.count(opDisableAccount) != 1 {
		t.Fatalf("state %s ops %v %v", r.State, h.a.ops(), h.b.ops())
	}
}

func TestMoverReportsConnectorsWithoutARecordedAccount(t *testing.T) {
	h, f := scimHarness(t)
	h.dir.put(person(alice, "active", "alice@example.com"))
	f.users["u1"] = map[string]any{"userName": "alice@example.com"}
	if _, _, err := h.eng.Start(context.Background(), StartRequest{Kind: KindMover, PartyID: alice, Trigger: "api", Key: "move-1"}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	r := h.onlyRun()
	if step(r, "idp").ErrorCode != "account_not_linked" || step(r, "app-b").ErrorCode != "manual_check_required" || r.State != RunFailed {
		t.Fatalf("state %s steps %+v", r.State, r.Steps)
	}
	for _, l := range f.log {
		if strings.HasPrefix(l, "PATCH") {
			t.Fatalf("a mover changed an unlinked account: %v", f.log)
		}
	}
}

// A group change for a person with no recorded account used to start nothing at all.
func TestGroupChangeForAnUnlinkedPersonIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.eng.groupMovers(context.Background(), []string{alice})
	h.settle()
	if r := h.onlyRun(); r.Kind != KindMover || r.State != RunFailed {
		t.Fatalf("run %+v", r)
	}
}

// ...but not while the joiner that will record the accounts is still running.
func TestGroupChangeDuringAJoinerStartsNoMover(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "onboarding", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "onboarding"))
	h.eng.groupMovers(context.Background(), []string{alice})
	if r := h.onlyRun(); r.Kind != KindJoiner {
		t.Fatalf("run %+v", r)
	}
}

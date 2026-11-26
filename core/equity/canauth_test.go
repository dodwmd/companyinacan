package equity

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

func TestEquityRolesFromPermissions(t *testing.T) {
	doc, ps := config.Load("../../modules/equity/module.yaml")
	if doc == nil {
		t.Fatalf("manifest: %v", ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	for manifestRole, want := range map[string]string{
		"employee": "employee", "manager": "employee", "hr": "employee,hr",
		"finance": "finance", "board": "", "admin": "finance,admin",
	} {
		if got := strings.Join(rolesFromPermissions(m.Roles[manifestRole]), ","); got != want {
			t.Errorf("%s %v -> %q, want %q", manifestRole, m.Roles[manifestRole], got, want)
		}
	}
	// No subset of any role's permissions gains a role, and none is ever the board.
	all := m.Permissions
	pick := func(mask int) []string {
		var out []string
		for i, p := range all {
			if mask&(1<<i) != 0 {
				out = append(out, p)
			}
		}
		return out
	}
	for full := range 1 << len(all) {
		most := rolesFromPermissions(pick(full))
		if slices.Contains(most, RoleBoard) {
			t.Fatalf("%v is the board", pick(full))
		}
		for sub := range 1 << len(all) {
			if sub&full != sub {
				continue
			}
			for _, r := range rolesFromPermissions(pick(sub)) {
				if !slices.Contains(most, r) {
					t.Fatalf("%v gains %q over its superset %v", pick(sub), r, pick(full))
				}
			}
		}
	}
}

func TestPersonalAccessTokensThroughCanCore(t *testing.T) {
	core := fakecore.New(t, "equity")
	auth := &CanAuth{In: &modclient.Introspector{API: core.Client(t)}}
	const tok = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	core.AddToken(tok, fakecore.Token{Subject: "user:fin", Permissions: []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read"}})
	core.AddToken(tok+"B", fakecore.Token{Subject: "user:board", Permissions: []string{"equity.read", "equity.approve", "equity.sensitive-read", "equity.admin"}})
	core.AddToken(tok+"F", fakecore.Token{Subject: "user:eve", Permissions: []string{"equity.self-read"}, Module: "jobs"})
	who := func(token string) (*Principal, error) {
		r := httptest.NewRequest(http.MethodGet, "/v1/entities", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return auth.Authenticate(r)
	}
	p, err := who(tok)
	if err != nil || p.Subject != "user:fin" || p.PersonID != "" || strings.Join(p.Roles, ",") != "finance" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, token := range map[string]string{"board permissions": tok + "B", "another module's": tok + "F", "unknown": tok + "?"} {
		if p, err := who(token); err == nil {
			t.Errorf("%s: %+v", name, p)
		}
	}
	core.SetDown(true)
	core.AddToken(tok+"D", fakecore.Token{Subject: "user:new", Permissions: []string{"equity.self-read"}})
	if _, err := who(tok + "D"); err == nil {
		t.Fatal("accepted while can-core is down")
	}
}

package wiring

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

// A can-core identity is never own-only or limited to parties, so the finance employee role must
// give it nothing: neither with the manifest as shipped nor with the one an installation recorded
// before (employee: [finance.write]), which stays until the module is upgraded.
func TestAFinanceEmployeeGetsNoFinanceAccessThroughCanCore(t *testing.T) {
	for name, legacy := range map[string]bool{"shipped manifest": false, "manifest recorded before the fix": true} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			_, h := newFinance(t, w)
			if legacy {
				m, err := w.db.GetModule(w.ctx, "finance")
				if err != nil {
					t.Fatal(err)
				}
				m.Manifest.Roles["employee"] = []string{"finance.write"}
				if err := w.reg.Register(w.ctx, m); err != nil {
					t.Fatal(err)
				}
				w.rb.Invalidate()
			}
			createParty := func(token string) int {
				req := httptest.NewRequest("POST", "/v1/parties", strings.NewReader(`{"kind":"customer","name":"Acme"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code
			}
			for _, u := range []string{"root", "emma"} { // the first administrator is the owner
				if _, err := w.db.CreateAdmin(w.ctx, u, "x", "y"); err != nil {
					t.Fatal(err)
				}
			}

			// A token made while she could write finance stops working when employee is all she has.
			w.grant("emma", "finance:finance")
			old := w.token("emma", "finance", "finance.write")
			if code := createParty(old); code != 201 {
				t.Fatalf("control: the finance role cannot write: %d", code)
			}
			if err := w.rb.Revoke(w.ctx, "root", "user:emma", "finance:finance", ""); err != nil {
				t.Fatal(err)
			}
			w.grant("emma", "finance:employee")

			for _, perm := range []string{"finance.read", "finance.write"} {
				if ok, err := w.pats.Holds(w.ctx, "user:emma", perm); err != nil || ok {
					t.Fatalf("an employee holds %s (%v)", perm, err)
				}
				_, err := w.pats.Issue(w.ctx, "user:emma", pat.IssueRequest{Name: "mine", Module: "finance", Permissions: []string{perm}, TTL: time.Hour})
				if !errors.Is(err, pat.ErrForbidden) {
					t.Fatalf("an employee was issued a %s token: %v", perm, err)
				}
			}
			// The module caches an introspection briefly, so ask with a fresh application.
			_, h = newFinance(t, w)
			if legacy {
				m, _ := w.db.GetModule(w.ctx, "finance")
				m.Manifest.Roles["employee"] = []string{"finance.write"}
				if err := w.reg.Register(w.ctx, m); err != nil {
					t.Fatal(err)
				}
				w.rb.Invalidate()
			}
			if code := createParty(old); code != 401 {
				t.Fatalf("an employee's token still reaches finance: %d", code)
			}

			// Single sign-on: not offered, and no ticket.
			tickets := &ticket.Service{DB: w.db, Audit: audit.New(w.db), Holds: w.pats.Holds,
				Launcher: ticket.Launcher{Domain: "example.org", Overrides: map[string]string{"finance": "https://finance.example.org"}}}
			emma, err := w.db.AdminByUsername(w.ctx, "emma")
			if err != nil {
				t.Fatal(err)
			}
			admin := store.Session{IDHash: strings.Repeat("e", 64), UserID: emma.ID, CSRF: "c", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
			if err := w.db.CreateSession(w.ctx, admin); err != nil {
				t.Fatal(err)
			}
			if apps, err := tickets.Apps(w.ctx, "user:emma"); err != nil || len(apps) != 0 {
				t.Fatalf("apps offered to an employee: %+v %v", apps, err)
			}
			if _, err := tickets.Launch(w.ctx, "user:emma", "finance", "", admin); !errors.Is(err, ticket.ErrNotAllowed) {
				t.Fatalf("an employee launched finance: %v", err)
			}
		})
	}
}

//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (g) Equity: an entity with a share class and a pool, an option grant drafted and submitted by HR,
// refused when approved by the wrong role, approved by the board member (a second subject), then
// activated; the vesting endpoint answers. Every run creates its own entity.
//
// The board identity comes from the introstub service of dev/compose/e2e.yml (can-core token
// introspection is not available in compose); the service token never carries the board role.
func TestEquityGrantApprovalAndVesting(t *testing.T) {
	svc := equityAPI(tokenOf(t, "equity", "CAN_EQUITY_API_TOKEN"))
	hr, board := svc.WithToken(tokHR), svc.WithToken(tokBoard)
	u := harness.Unique()

	// The service-account token is hr + finance + admin and can build the structure.
	ent := svc.Post(t, "/v1/entities", map[string]any{"name": "E2E Inc " + u, "jurisdiction": "Delaware, US", "currency": "USD"}).Expect(t, "create entity", 201).Str("id")
	class := svc.Post(t, "/v1/entities/"+ent+"/share-classes", map[string]any{"name": "Common", "kind": "common", "par_minor": 1, "currency": "USD", "authorised": "10000000"}).Expect(t, "create share class", 201).Str("id")
	plan := svc.Post(t, "/v1/entities/"+ent+"/plans", map[string]any{"name": "2026 Plan " + u, "class_id": class, "authorised": "1000000", "adopted_on": "2026-01-02"}).Expect(t, "create plan", 201).Str("id")
	stk := svc.Post(t, "/v1/entities/"+ent+"/stakeholders", map[string]any{"kind": "advisor", "name": "Eve Advisor " + u, "email": "eve-" + u + "@example.test"}).Expect(t, "create stakeholder", 201).Str("id")

	// A grant cannot leave draft for a holder who is not tied to a person: HR (another subject than the
	// creator) attests the holder is external.
	grant := map[string]any{
		"stakeholder_id": stk, "type": "NSO", "quantity": "4848", "strike_minor": 125, "currency": "USD",
		"grant_date": "2026-01-15", "vesting_start": "2026-01-15",
		"schedule":    map[string]any{"kind": "periodic", "months": 48, "cliff_months": 12, "interval": "monthly"},
		"expiry_date": "2036-01-15",
	}
	gid := hr.Post(t, "/v1/plans/"+plan+"/grants", grant).Expect(t, "HR drafts a grant", 201).Str("id")
	hr.Post(t, "/v1/grants/"+gid+"/submit", nil).Expect(t, "submit without an attested holder is refused", 409, 422, 400, 403)
	hr.Post(t, "/v1/stakeholders/"+stk+"/attest-external", nil).Expect(t, "HR attests the holder is external", 200)
	hr.Post(t, "/v1/grants/"+gid+"/submit", nil).Expect(t, "HR submits the grant", 200)

	// Separation of duties and roles.
	hr.Post(t, "/v1/grants/"+gid+"/approve", nil).Expect(t, "the submitter cannot approve", 403)
	svc.Post(t, "/v1/grants/"+gid+"/approve", nil).Expect(t, "the service account (admin, never board) cannot approve", 403)
	board.Post(t, "/v1/grants/"+gid+"/approve", nil).Expect(t, "the board member approves", 200)
	g := hr.Get(t, "/v1/grants/"+gid).Expect(t, "read the grant", 200)
	if g.Str("status") != "approved" {
		t.Fatalf("grant status = %q, want approved: %.300s", g.Str("status"), g.String())
	}
	// Approved terms are frozen.
	hr.Patch(t, "/v1/grants/"+gid, map[string]any{"quantity": "9999"}).Expect(t, "approved terms cannot be edited", 409, 403, 422)
	hr.Post(t, "/v1/grants/"+gid+"/activate", map[string]any{"activated_on": time.Now().UTC().Format("2006-01-02")}).Expect(t, "activate", 200)

	v := hr.Get(t, "/v1/grants/"+gid+"/vesting?as_of=2027-01-15").Expect(t, "vesting at the cliff", 200)
	if v.Str("vested") != "1212" {
		t.Fatalf("vested at the 12-month cliff of a 4848 grant over 48 months = %q, want 1212: %.300s", v.Str("vested"), v.String())
	}
	before := hr.Get(t, "/v1/grants/"+gid+"/vesting?as_of=2026-12-31").Expect(t, "vesting before the cliff", 200)
	if before.Str("vested") != "0" {
		t.Fatalf("vested before the cliff = %q, want 0", before.Str("vested"))
	}

	// The cap table needs the ledger (finance/admin), not a role the board lacks.
	board.Get(t, "/v1/entities/"+ent+"/captable").Expect(t, "board reads the cap table", 200)
	// No credential, no data.
	web.Do(t, harness.Req{Host: "equity.localhost", Path: "/v1/entities"}).Expect(t, "no token", 401)
}

package adminui

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
)

// An upgrade that widens permissions is shown on the catalogue and on a change's preview, and runs
// only after someone with the operate permission approves what was shown.
func TestWideningUpgradeIsShownAndApproved(t *testing.T) {
	m := newMM(t)
	ctx := context.Background()
	rec := reconcile.New(m.db, registry.New(m.db, "http://can-core"), m.audit, m.fk)
	rec.RequireApproval = true
	m.svc.Runner = modmgr.NewRunner(rec)
	path := filepath.Join(m.svc.ModulesDir, "web", "module.yaml")
	b, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(b, "permissions: [leads.export]\n"...), 0o600))

	apply := func() reconcile.Result {
		t.Helper()
		d, ps := reconcile.LoadDesired(m.store.Path, m.svc.ModulesDir)
		if d.Company == nil {
			t.Fatal(ps)
		}
		res, err := m.svc.Runner.Apply(ctx, d)
		must(t, err)
		return res
	}
	held := apply().Held
	if len(held) != 1 {
		t.Fatalf("held %+v", held)
	}

	ops := m.loginAs("ops")
	page := m.do("GET", "/catalogue", nil, ops.cookie).body
	for _, want := range []string{"new permission leads.export", `action="/upgrades/web/approve"`, held[0].Approval} {
		if !strings.Contains(page, want) {
			t.Fatalf("catalogue lacks %q", want)
		}
	}

	// The preview of another change says the upgrade waits and offers to approve it.
	pv, err := m.svc.PreviewPatch(ctx, modmgr.Patch{Ops: []modmgr.Op{modmgr.SetEnabled("solo", true)}}, nil)
	must(t, err)
	if len(pv.Held) != 1 || len(pv.Actions) != 1 || pv.Actions[0].Module != "solo" {
		t.Fatalf("preview: actions %+v held %+v", pv.Actions, pv.Held)
	}
	ch, err := m.svc.Propose(ctx, "ops", modmgr.Proposal{Patch: modmgr.Patch{Ops: []modmgr.Op{modmgr.SetEnabled("solo", true)}}})
	must(t, err)
	change := m.do("GET", "/changes/"+ch.ID, nil, ops.cookie).body
	if !strings.Contains(change, modmgr.UpgradeAck("web", held[0].Approval)) || !strings.Contains(change, "new permission leads.export") {
		t.Fatalf("change page does not offer the approval")
	}

	post := func(s session, approval string) int {
		return m.do("POST", "/upgrades/web/approve", url.Values{"csrf": {s.csrf}, "approval": {approval}}, s.cookie).StatusCode
	}
	if got := post(m.loginAs("viewer"), held[0].Approval); got != http.StatusForbidden {
		t.Fatalf("a viewer approved: %d", got)
	}
	if got := post(ops, "0000000000000000"); got != http.StatusConflict {
		t.Fatalf("an approval of something else: %d", got)
	}
	if got := post(ops, held[0].Approval); got != http.StatusSeeOther {
		t.Fatalf("approve: %d", got)
	}
	res := apply()
	if len(res.Actions) != 1 || res.Actions[0].Module != "web" || len(res.Held) != 0 {
		t.Fatalf("after approval: %+v", res)
	}
	if page := m.do("GET", "/catalogue", nil, ops.cookie).body; strings.Contains(page, "/upgrades/web/approve") {
		t.Fatal("still shown as waiting")
	}
}

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

	must(t, m.db.RequestRotation(ctx, "web", "CAN_WEBHOOK_SECRET", "test"))
	apply()

	ops := m.loginAs("ops")
	page := m.do("GET", "/catalogue", nil, ops.cookie).body
	for _, want := range []string{"NOT APPLIED: secret rotation of CAN_WEBHOOK_SECRET", "new permission leads.export", `action="/upgrades/web/approve"`, held[0].Approval} {
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
	if !strings.Contains(change, modmgr.UpgradeAck("web", held[0].Approval)) || !strings.Contains(change, "new permission leads.export") || !strings.Contains(change, "NOT APPLIED: secret rotation") {
		t.Fatalf("change page does not offer the approval or say what is left out")
	}
	// Applying without the tick is not reported as a success.
	run, err := m.svc.Apply(ctx, "ops", ch.ID)
	must(t, err)
	if run.State != modmgr.RunFailed || !strings.Contains(run.Error, "module web waits for approval, NOT APPLIED: secret rotation") {
		t.Fatalf("run %q %q", run.State, run.Error)
	}
	if page := m.do("GET", "/runs/"+run.ID, nil, ops.cookie).body; !strings.Contains(page, "NOT APPLIED: secret rotation") {
		t.Fatal("the result page does not say what was left out")
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

	// Enabling a module again asks for no approval, so its preview says what that brings.
	d, ps := reconcile.LoadDesired(m.store.Path, m.svc.ModulesDir)
	if d.Company == nil {
		t.Fatal(ps)
	}
	solo := filepath.Join(m.svc.ModulesDir, "solo", "module.yaml")
	enableSolo := modmgr.Patch{Ops: []modmgr.Op{modmgr.SetEnabled("solo", true)}}
	next, err := m.svc.Propose(ctx, "ops", modmgr.Proposal{Patch: enableSolo})
	must(t, err)
	_, err = m.svc.Apply(ctx, "ops", next.ID)
	must(t, err)
	off, err := m.svc.Propose(ctx, "ops", modmgr.Proposal{Patch: modmgr.Patch{Ops: []modmgr.Op{modmgr.SetEnabled("solo", false)}}})
	must(t, err)
	_, err = m.svc.Apply(ctx, "ops", off.ID)
	must(t, err)
	sb, err := os.ReadFile(solo)
	must(t, err)
	must(t, os.WriteFile(solo, append(sb, "permissions: [solo.admin]\n"...), 0o600))
	again, err := m.svc.Propose(ctx, "ops", modmgr.Proposal{Patch: enableSolo})
	must(t, err)
	if body := m.do("GET", "/changes/"+again.ID, nil, ops.cookie).body; !strings.Contains(body, "newly allowed: new permission solo.admin") {
		t.Fatal("the preview of enabling does not list what is newly allowed")
	}
}

package modmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// hooky adds a module whose backup hook needs the unpublished can-hooks image: enabling it stops
// every backup, so applying must be confirmed for it by name.
func hooky(t *testing.T, r *rig) {
	t.Helper()
	d := filepath.Join(r.svc.ModulesDir, "hooky")
	must(t, os.MkdirAll(filepath.Join(d, "chart"), 0o755))
	must(t, os.MkdirAll(filepath.Join(d, "hooks"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "hooks", "backup.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte(manifest("hooky", "Hooky", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: hooky}
requires: {database: postgres}
resources: {ram_mb: 10}
hooks: {image: "ghcr.io/dodwmd/can-hooks:0.1.0", backup: ./hooks/backup.sh, restore: ./hooks/backup.sh}
`)), 0o644))
}

func TestApplyNeedsConfirmationForAModuleThatStopsBackups(t *testing.T) {
	r := newRig(t)
	hooky(t, r)
	ctx := context.Background()
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("hooky", true)}}, Acks: []string{BackupAck("hooky")}})
	must(t, err)
	pv, err := r.svc.Preview(ctx, ch.ID)
	must(t, err)
	if len(pv.BackupStops) != 1 || pv.BackupStops[0].Module != "hooky" || !pv.BackupStops[0].Postgres || pv.BackupStops[0].Why == "" {
		t.Fatalf("BackupStops = %+v", pv.BackupStops)
	}
	// An acknowledgement stored with the proposal is not a confirmation of this apply.
	var ae *AckError
	if _, err := r.svc.Apply(ctx, "root", ch.ID); !errors.As(err, &ae) || len(ae.Missing) != 1 || ae.Missing[0] != "backup:hooky" {
		t.Fatalf("applied without confirmation: %v", err)
	}
	if _, err := r.svc.Apply(ctx, "root", ch.ID, "backup:other"); !errors.Is(err, ErrAckRequired) {
		t.Fatalf("a confirmation for another module was accepted: %v", err)
	}
	snap, _ := r.store.Get(ctx)
	if c, _ := decodeCompany("company.yaml", snap.Data); enabledSet(c)["hooky"] {
		t.Fatal("company.yaml written without confirmation")
	}
	run, err := r.svc.Apply(ctx, "root", ch.ID, "backup:hooky")
	must(t, err)
	if run.State != RunSucceeded {
		t.Fatalf("%+v", run)
	}
}

func TestWizardApplyNeedsTheSameConfirmation(t *testing.T) {
	r := wizRig(t)
	hooky(t, r)
	ctx := context.Background()
	_, err := r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error {
		d.Name, d.Domain, d.Modules = "Acme", "acme.example", []string{"hooky", "web"}
		return nil
	})
	must(t, err)
	if _, err := r.svc.WizardApply(ctx, "root"); !errors.Is(err, ErrAckRequired) {
		t.Fatalf("wizard applied without confirmation: %v", err)
	}
	if _, err := r.svc.WizardApply(ctx, "root", "backup:hooky"); err != nil {
		t.Fatal(err)
	}
}

// The templates must not enable a module that stops backups: nothing would warn before it is picked.
func TestDefaultBundlesHoldNoModuleThatStopsBackups(t *testing.T) {
	svc := &Service{ModulesDir: "../../../modules"}
	cat, err := svc.Catalogue()
	must(t, err)
	for _, b := range DefaultBundles {
		for _, id := range b.Modules {
			e := cat.Get(id)
			if e == nil {
				t.Fatalf("bundle %s names unknown module %s", b.ID, id)
			}
			if why := e.BackupWarning(); why != "" {
				t.Errorf("bundle %s enables %s, which stops all backups: %s", b.ID, id, why)
			}
		}
	}
}

package corecli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestModulesProblemsListsFailedInstallsAndUpgrades(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.Module{
		{ID: "fine", State: store.StateEnabled},
		{ID: "broken", State: store.StateFailed, LastError: "rollout timed out"},
		{ID: "stale", State: store.StateEnabled, LastError: "upgrade failed: password=hunter2"},
		{ID: "off", State: store.StateDisabled},
	} {
		m.Version, m.Namespace, m.ServiceAccount = "1", "mod-"+m.ID, m.ID
		if err := db.UpsertModule(ctx, m, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	var out, errs bytes.Buffer
	if code := Execute([]string{"modules", "problems", "--db", path}, strings.NewReader(""), &out, &errs, func(string) string { return "" }); code != exitOK {
		t.Fatal(code, errs.String())
	}
	got := out.String()
	for _, want := range []string{`"id":"broken","state":"failed"`, `"id":"stale","state":"enabled"`, "rollout timed out"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "fine") || strings.Contains(got, `"off"`) {
		t.Errorf("too much: %s", got)
	}
}

func TestModulesApproveAndPendingProblem(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModule(ctx, store.Module{ID: "forms", Version: "1", Namespace: "mod-forms", ServiceAccount: "forms", State: store.StateEnabled}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPendingUpgrade(ctx, "forms", `{"approval":"0123456789abcdef","widens":["new permission leads.erase"]}`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	run := func(args ...string) (int, string) {
		var out, errs bytes.Buffer
		code := Execute(append(args, "--db", path), strings.NewReader(""), &out, &errs, func(string) string { return "" })
		return code, out.String() + errs.String()
	}
	if code, out := run("modules", "problems"); code != exitOK || !strings.Contains(out, `"state":"pending-approval"`) || !strings.Contains(out, "leads.erase") {
		t.Fatal(code, out)
	}
	for _, bad := range [][2]string{{"forms", "yes"}, {"forms", "ffffffffffffffff"}, {"ghost", "0123456789abcdef"}} {
		if code, out := run("modules", "approve", bad[0], bad[1]); code == exitOK {
			t.Fatal(bad, out)
		}
	}
	if code, out := run("modules", "approve", "forms", "0123456789abcdef"); code != exitOK {
		t.Fatal(code, out)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if m, err := db.GetModule(ctx, "forms"); err != nil || m.ApprovedUpgrade != "0123456789abcdef" {
		t.Fatal(m.ApprovedUpgrade, err)
	}
}

// A rotation asked of a module that waits for approval does not happen, and the command says so.
func TestSecretsRotateOfAHeldModuleSaysItIsNotApplied(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModule(ctx, store.Module{ID: "forms", Version: "1", Namespace: "mod-forms", ServiceAccount: "forms", State: store.StateEnabled}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPendingUpgrade(ctx, "forms", `{"approval":"0123456789abcdef","widens":["new permission leads.erase"]}`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	var out, errs bytes.Buffer
	code := Execute([]string{"admin", "secrets", "rotate", "forms", "CAN_WEBHOOK_SECRET", "--db", path}, strings.NewReader(""), &out, &errs, func(string) string { return "" })
	if code == exitOK || !strings.Contains(errs.String(), "NOT APPLIED") || !strings.Contains(errs.String(), "disable the module") {
		t.Fatal(code, out.String(), errs.String())
	}
}

// A code can come from the plan of a company.yaml that is not synced yet (a new connection).
func TestModulesApproveAcceptsTheCodeOfThePlannedCompany(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModule(ctx, store.Module{ID: "equity", Version: "1", Namespace: "mod-equity", ServiceAccount: "equity", State: store.StateEnabled}, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	old := planModules
	defer func() { planModules = old }()
	planModules = func(_ context.Context, _ func(string) string, _ *store.DB, company []byte) ([]reconcile.Action, error) {
		if string(company) != "next" {
			t.Errorf("planned against %q", company)
		}
		return []reconcile.Action{{Kind: reconcile.Upgrade, Module: "equity", Held: reconcile.HeldApproval, Approval: "0123456789abcdef"}}, nil
	}
	run := func(code string) int {
		var out, errs bytes.Buffer
		return Execute([]string{"modules", "approve", "equity", code, "--company-b64", "bmV4dA==", "--db", path}, strings.NewReader(""), &out, &errs, func(string) string { return "" })
	}
	if run("ffffffffffffffff") == exitOK || run("0123456789abcdef") != exitOK {
		t.Fatal("only the code of the plan approves")
	}
}

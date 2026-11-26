package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/problem"
)

// fromShipped loads company.yaml text against the shipped modules.
func fromShipped(t *testing.T, modules, extra string) Desired {
	t.Helper()
	y := "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider: {type: local}\n" + extra + "modules:\n" + modules
	d, ps := LoadDesiredBytes("company.yaml", []byte(y), "../../../modules")
	if problem.HasErrors(ps) {
		t.Fatalf("%v", ps)
	}
	return d
}

func plan(t *testing.T, g *rig, d Desired) []Action {
	t.Helper()
	a, err := g.r.Plan(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A changed setting or company fact is an upgrade; one that widens what the module does is held.
func TestSettingsAndFactsPlanUpgradesAndWideningIsHeld(t *testing.T) {
	g := newRig(t)
	g.r.RequireApproval = true
	apply(t, g, fromShipped(t, "  finance: {}\n", ""))

	d := fromShipped(t, "  finance: {}\n", "organisation: {address: 1 Anvil Way}\n")
	a := plan(t, g, d)
	if len(a) != 1 || a[0].Kind != Upgrade || a[0].Held != "" || len(a[0].Widens) != 0 {
		t.Fatalf("a changed fact: %+v", a)
	}
	apply(t, g, d)
	if a := plan(t, g, d); len(a) != 0 {
		t.Fatalf("not idempotent: %+v", a)
	}

	// Keeping records longer is not widening; removing that setting again is.
	longer := fromShipped(t, "  finance: {settings: {retention_years: 10, payment_instructions: Pay us}}\n", "")
	if a := plan(t, g, longer); len(a) != 1 || a[0].Held != "" {
		t.Fatalf("%+v", a)
	}
	apply(t, g, longer)
	a = plan(t, g, fromShipped(t, "  finance: {settings: {payment_instructions: Pay us}}\n", ""))
	if len(a) != 1 || a[0].Held != HeldApproval || !strings.Contains(strings.Join(a[0].Widens, "\n"), "setting retention_years becomes 7: financial records are deleted sooner") {
		t.Fatalf("%+v", a)
	}

	// The reporting currency, from a company fact.
	cur := fromShipped(t, "  finance: {settings: {retention_years: 10}}\n", "organisation: {currency: AUD}\n")
	a = plan(t, g, cur)
	if len(a) != 1 || a[0].Held != HeldApproval || !strings.Contains(strings.Join(a[0].Widens, "\n"), `setting base_currency becomes "AUD"`) {
		t.Fatalf("%+v", a)
	}
	if res := apply(t, g, cur); len(res.Held) != 1 || len(res.Actions) != 0 {
		t.Fatalf("%+v", res)
	}
	if err := g.db.ApproveUpgrade(context.Background(), "finance", a[0].Approval); err != nil {
		t.Fatal(err)
	}
	if res := apply(t, g, cur); len(res.Held) != 0 || len(res.Actions) != 1 {
		t.Fatalf("approved: %+v", res)
	}
	if a := plan(t, g, cur); len(a) != 0 {
		t.Fatalf("%+v", a)
	}
}

// An installation made before settings were mapped ran with the defaults: a setting that was
// written and ignored until now waits for approval when it widens.
func TestASettingIgnoredUntilNowIsHeldWhenItWidens(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.r.RequireApproval = true
	d := fromShipped(t, "  files: {settings: {scanner_required: false, trash_days: 60}}\n", "")
	old := d
	old.Schemas = nil // the earlier release
	apply(t, g, old)

	a := plan(t, g, d)
	if len(a) != 1 || a[0].Held != HeldApproval || len(a[0].Widens) != 1 || !strings.Contains(a[0].Widens[0], "setting scanner_required becomes false") {
		t.Fatalf("%+v", a)
	}
	if err := g.db.ApproveUpgrade(ctx, "files", a[0].Approval); err != nil {
		t.Fatal(err)
	}
	apply(t, g, d)

	// While an upgrade is held, a stricter setting does not arrive: that is urgent.
	next := fromShipped(t, "  files: {settings: {inline_images: true}}\n", "")
	a = plan(t, g, next)
	if len(a) != 1 || a[0].Held != HeldApproval || !a[0].Urgent || !strings.Contains(strings.Join(a[0].NotApplied, "\n"), "stricter setting scanner_required") {
		t.Fatalf("%+v", a)
	}
}

// Every entry a list gains is shown whole, and an approval of one list does not cover another:
// not even one that differs only past what a line could show.
func TestAnApprovalCoversTheWholeValue(t *testing.T) {
	g := newRig(t)
	g.r.RequireApproval = true
	apply(t, g, fromShipped(t, "  forms: {}\n", ""))
	long := "https://" + strings.Repeat("a", 90) + ".example"
	of := func(origins ...string) Action {
		t.Helper()
		a := plan(t, g, fromShipped(t, "  forms: {settings: {cors_allowed_origins: [\""+strings.Join(origins, "\", \"")+"\"]}}\n", ""))
		if len(a) != 1 || a[0].Held != HeldApproval || len(a[0].Widens) != len(origins) {
			t.Fatalf("%+v", a)
		}
		for i, o := range origins {
			if !strings.Contains(a[0].Widens[i], `setting cors_allowed_origins adds "`+o+`"`) {
				t.Fatalf("%q is not shown: %q", o, a[0].Widens[i])
			}
		}
		return a[0]
	}
	one := of(long)
	if err := g.db.ApproveUpgrade(context.Background(), "forms", one.Approval); err != nil {
		t.Fatal(err)
	}
	two := of(long, "https://evil.example")
	if two.Approval == one.Approval {
		t.Fatal("adding an entry reuses the approval")
	}
	// Long scalar values that start alike are still told apart.
	finance := newRig(t)
	finance.r.RequireApproval = true
	apply(t, finance, fromShipped(t, "  jobs: {}\n", ""))
	text := func(end string) string {
		a := plan(t, finance, fromShipped(t, "  jobs: {settings: {consent_text: \""+strings.Repeat("I agree. ", 50)+end+"\"}}\n", ""))
		if len(a) != 1 || a[0].Held != HeldApproval {
			t.Fatalf("%+v", a)
		}
		return a[0].Approval
	}
	if text("Yes.") == text("No.") {
		t.Fatal("the approval does not cover the end of a long value")
	}
}

func TestSettingsThatWeakenOrRedirectAreHeld(t *testing.T) {
	for module, c := range map[string][2]string{
		"finance":      {"{signing_record_ip: none}", "{signing_record_ip: truncated}"},
		"work":         {"{}", "{rate_limit_per_minute: 0}"},
		"provisioning": {"{}", "{timezone: Australia/Brisbane}"},
		"forms":        {"{notify_email: [a@acme.example]}", "{notify_email: [a@acme.example, b@elsewhere.example]}"},
		"jobs":         {"{consent_version: v2}", "{consent_version: v3}"},
	} {
		g := newRig(t)
		g.r.RequireApproval = true
		d := fromShipped(t, "  "+module+": {settings: "+c[0]+"}\n", "")
		apply(t, g, d)
		if a := plan(t, g, d); len(a) != 0 {
			t.Fatalf("%s: %+v", module, a)
		}
		if a := plan(t, g, fromShipped(t, "  "+module+": {settings: "+c[1]+"}\n", "")); len(a) != 1 || a[0].Held != HeldApproval {
			t.Errorf("%s %s -> %s is not held: %+v", module, c[0], c[1], a)
		}
	}
}

// Releases before the mapping gave forms its notify_email: upgrading can-core does not hold it.
func TestWhatWasAppliedBeforeTheMappingIsNotNew(t *testing.T) {
	g := newRig(t)
	g.r.RequireApproval = true
	d := fromShipped(t, "  forms: {settings: {notify_email: [a@acme.example]}}\n", "")
	apply(t, g, d)
	// The record such a release leaves: what it was installed with, without the mapped values.
	rec, err := g.db.GetModule(context.Background(), "forms")
	if err != nil {
		t.Fatal(err)
	}
	old := ParseApplied(rec.Applied)
	old.Values, old.Mapped = nil, false
	if _, err := g.db.ExecContext(context.Background(), `UPDATE modules SET applied_json=?, settings_hash='old'`, old.encode()); err != nil {
		t.Fatal(err)
	}
	if a := plan(t, g, d); len(a) != 1 || a[0].Held != "" {
		t.Fatalf("%+v", a)
	}
	if got := WithinValues(d.Schemas["forms"], old, d.Company.Modules["forms"].Settings, d.values("forms")); got["config.notify_email"] == nil {
		t.Fatalf("a restore of such a record keeps the recipients: %v", got)
	}
}

// Wrapped modules and settings that map to nothing do not change what is installed.
func TestSettingsNotAppliedAreWarned(t *testing.T) {
	y := "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  analytics: {settings: {retention_days: 90, site_domain: x.example}}\n  newsletter: {settings: {double_opt_in: false, nonsense: 1, smtp_password: hunter2}}\n"
	_, ps := LoadDesiredBytes("company.yaml", []byte(y), "../../../modules")
	var got []string
	for _, p := range ps {
		if p.Severity != problem.Warning {
			t.Errorf("old configurations must keep validating: %v", p)
		}
		if strings.Contains(p.Message, "hunter2") {
			t.Errorf("a value is quoted: %s", p.Message)
		}
		got = append(got, p.Rule+" "+p.Path)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"semantic.setting-not-applied /modules/analytics/settings/retention_days",
		"semantic.setting-not-applied /modules/analytics/settings/site_domain",
		"semantic.setting-not-applied /modules/newsletter/settings/double_opt_in",
		"semantic.setting-unknown /modules/newsletter/settings/nonsense",
		"semantic.setting-secret /modules/newsletter/settings/smtp_password",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %s:\n%s", want, all)
		}
	}
}

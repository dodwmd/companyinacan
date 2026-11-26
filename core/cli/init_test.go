package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/validate"
)

func TestInitWritesValidCompany(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	code, _, errs := run(t, "init", dir, "--name", "Acme Co", "--domain", "acme.example", "--provider", "local", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	// Valid; the one warning is that no recipient of new messages is set yet.
	if ps := validate.CompanyFile(filepath.Join(dir, "company.yaml"), "../../modules"); problem.HasErrors(ps) || len(ps) != 1 || ps[0].Rule != "semantic.contact-no-notify" {
		t.Fatalf("generated company: %v", ps)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), ".can/") {
		t.Fatalf(".gitignore missing .can/: %v %q", err, gi)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "init", dir, "--name", "X Co", "--domain", "x.example", "--non-interactive")
	if code != ExitFailure || !strings.Contains(errs, "--force") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Fatal("existing file was modified")
	}
	code, _, _ = run(t, "init", dir, "--name", "X Co", "--domain", "x.example", "--non-interactive", "--force")
	if code != ExitOK {
		t.Fatalf("--force: code=%d", code)
	}
}

func TestInitNonInteractiveNeedsFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	code, _, errs := run(t, "init", dir) // stdin is not a terminal in tests
	if code != ExitUsage || !strings.Contains(errs, "--name") || !strings.Contains(errs, "--domain") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("directory created despite usage error")
	}
}

func TestInitRejectsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	code, _, errs := run(t, "init", dir, "--name", "X", "--domain", "Not_A_Domain", "--non-interactive")
	if code != ExitUsage || !strings.Contains(errs, "domain") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "company.yaml")); err == nil {
		t.Fatal("invalid company was written")
	}
}

func TestInitInteractive(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func(any) bool { return true }
	defer func() { stdinIsTerminal = old }()
	dir := filepath.Join(t.TempDir(), "i")
	var out, errb bytes.Buffer
	code := Execute([]string{"init", dir}, strings.NewReader("Interactive Co\ninteractive.example\n\n"), &out, &errb)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "company.yaml"))
	if !strings.Contains(string(b), "Interactive Co") || !strings.Contains(string(b), "type: local") {
		t.Fatalf("unexpected company.yaml:\n%s", b)
	}
}

func TestDefaultTerminalDetectionIgnoresDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer func() { _ = f.Close() }()
	if stdinIsTerminal(f) {
		t.Fatal("/dev/null must not be treated as an interactive terminal")
	}
	if stdinIsTerminal(strings.NewReader("")) {
		t.Fatal("a non-file reader is never a terminal")
	}
}

// `can init` gives a site with a working contact form, and nothing to be warned about.
func TestInitIncludesAContactForm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if code, _, errs := run(t, "init", dir, "--name", "Acme Co", "--domain", "acme.example", "--non-interactive"); code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	out := filepath.Join(t.TempDir(), "public")
	code, _, errs := run(t, "site", "build", dir, "--out", out, "--modules-dir", "../../modules")
	if code != ExitOK || strings.Count(errs, "warning:") != 1 || !strings.Contains(errs, "nobody is emailed") {
		t.Fatalf("site build: code=%d err=%q", code, errs)
	}
	page, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil || !strings.Contains(string(page), `href="/_forms/contact/form"`) {
		t.Fatalf("no link to the form: %v\n%s", err, page)
	}

	// The commented-out lines are valid once uncommented as the comments say.
	b, err := os.ReadFile(filepath.Join(dir, "company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "  forms: {}\n", "", 1)
	for _, line := range []string{"forms: { settings:", "email:", "  relay:"} {
		s = strings.Replace(s, "# "+line, line, 1)
	}
	if ps := validate.CompanyBytes("company.yaml", []byte(s), "../../modules"); len(ps) != 0 || !strings.Contains(s, "\n  forms: { settings: { notify_email: [hello@acme.example] } }") {
		t.Fatalf("uncommented: %v\n%s", ps, s)
	}

	static := filepath.Join(t.TempDir(), "static")
	if code, _, errs := run(t, "init", static, "--name", "S", "--domain", "s.example", "--provider", "static", "--non-interactive"); code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(static, "company.yaml")); strings.Contains(string(b), "forms") {
		t.Fatalf("a static host runs no forms gateway:\n%s", b)
	}
}

func TestSiteBuildWarnsWhenFormsIsOff(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules: {forms: {enabled: false}}\nsite: {contact: {}}\n")
	code, _, errs := run(t, "site", "build", dir, "--out", filepath.Join(t.TempDir(), "o"))
	if code != ExitOK || !strings.Contains(errs, "warning: site.contact is left out") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

// What a release archive or `go install` gives: no modules directory anywhere. A company made by
// `can init` must validate, build and plan there.
func TestInitThenUseWithoutAModulesDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("CAN_MODULES_DIR", "")
	if code, _, errs := run(t, "init", "co", "--name", "Acme Co", "--domain", "acme.example", "--provider", "ssh", "--non-interactive"); code != ExitOK {
		t.Fatalf("init: %d %q", code, errs)
	}
	file := filepath.Join("co", "company.yaml")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "type: ssh\n", "type: ssh\n    options: {host: 203.0.113.7}\n", 1)
	if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory called modules that holds no module (another tool's) is not one.
	if err := os.MkdirAll(filepath.Join("co", "modules", "vpc"), 0o700); err != nil {
		t.Fatal(err)
	}
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{Runner: func(provider.Target) provider.Runner { return newRunner() }}
	}, goodResolver())

	code, out, errs := run(t, "validate", "co")
	if code != ExitOK || !strings.Contains(out, "0 error(s), 1 warning(s)") || !strings.Contains(out, "nobody is emailed") {
		t.Fatalf("validate: %d %q %q", code, out, errs)
	}
	if code, _, errs = run(t, "site", "build", "co", "--out", "public"); code != ExitOK {
		t.Fatalf("site build: %d %q", code, errs)
	}
	code, out, errs = run(t, "up", "co", "--plan")
	if code != ExitOK || !strings.Contains(out, "build the site") || !strings.Contains(errs, "nobody is emailed") {
		t.Fatalf("up --plan: %d %q %q", code, out, errs)
	}
	if code, out, errs = run(t, "apply", "co", "--dry-run"); code != ExitOK || !strings.Contains(out, "install: forms") {
		t.Fatalf("apply --dry-run: %d %q %q", code, out, errs)
	}
	// What `up` reads before it provisions, and for the backup, comes from the same place.
	mods := resolveModulesDir("", "co")
	m, err := loadModule(mods, "forms")
	if err != nil || mods != validate.BuiltinModules || m.Runtime.SiteRoute == nil {
		t.Fatalf("%s: %v", mods, err)
	}
	code, _, errs = run(t, "backup", "install", "--dir", "co", "--plan")
	if code != ExitOK {
		t.Fatalf("backup install --plan: %d %q", code, errs)
	}

	// A local modules directory still wins, and a named one is taken as given.
	local := filepath.Join("co", "modules", "forms")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "module.yaml"), []byte("schema: 1\nid: forms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ = run(t, "validate", "co"); code != ExitFailure || !strings.Contains(out, filepath.Join(local, "module.yaml")) {
		t.Fatalf("the local manifest is not the one read: %d %q", code, out)
	}
	if code, out, _ = run(t, "validate", "co", "--modules-dir", "nowhere"); code != ExitFailure || !strings.Contains(out, "under nowhere") {
		t.Fatalf("a named directory: %d %q", code, out)
	}
}

package adminui

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A module whose backup hook cannot run stops every backup; the apply confirmation and the wizard's
// review say so and need a tick per module.
func addHooky(t *testing.T, m *mm) {
	t.Helper()
	d := filepath.Join(m.svc.ModulesDir, "hooky")
	must(t, os.MkdirAll(filepath.Join(d, "chart"), 0o755))
	must(t, os.MkdirAll(filepath.Join(d, "hooks"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "hooks", "backup.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte(mmManifest("hooky", "Hooky", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: hooky}
requires: {database: postgres}
resources: {ram_mb: 10}
hooks: {image: "ghcr.io/dodwmd/can-hooks:0.1.0", backup: ./hooks/backup.sh, restore: ./hooks/backup.sh}
`)), 0o644))
}

const hookyBox = `name="ack" value="backup:hooky" required`

func TestApplyConfirmationWarnsAboutBackupsAndNeedsTheTick(t *testing.T) {
	m := newMM(t)
	addHooky(t, m)
	root := m.loginAs("root")
	r := m.propose(root, []string{"web", "hooky"})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	change := r.Header.Get("Location")
	page := m.do("GET", change, nil, root.with()...)
	for _, want := range []string{"Enabling hooky stops ALL backups", "its database in the shared Postgres is not backed up", hookyBox} {
		if !strings.Contains(strings.ToLower(page.body), strings.ToLower(want)) {
			t.Errorf("the change page lacks %q", want)
		}
	}
	if r := m.post(root, change+"/apply", url.Values{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("applied without the tick: %d", r.StatusCode)
	}
	if r := m.post(root, change+"/apply", url.Values{"ack": {"backup:hooky"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
}

func TestWizardReviewWarnsAboutBackupsAndNeedsTheTick(t *testing.T) {
	m := newMM(t)
	addHooky(t, m)
	root := m.loginAs("root")
	m.post(root, "/setup/company", url.Values{"name": {"Acme"}, "domain": {"acme.example"}})
	if r := m.post(root, "/setup/modules", url.Values{"enable": {"web", "hooky"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if p := m.do("GET", "/setup/modules", nil, root.with()...); !strings.Contains(p.body, "stops all backups") {
		t.Error("the module step does not warn")
	}
	page := m.do("GET", "/setup/review", nil, root.with()...)
	if !strings.Contains(page.body, "Enabling hooky stops ALL backups") || !strings.Contains(page.body, hookyBox) {
		t.Fatalf("the review step lacks the warning or the tick:\n%s", page.body)
	}
	if r := m.post(root, "/setup/apply", url.Values{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("applied without the tick: %d", r.StatusCode)
	}
	if r := m.post(root, "/setup/apply", url.Values{"ack": {"backup:hooky"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
}

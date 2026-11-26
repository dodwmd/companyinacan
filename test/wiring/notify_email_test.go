package wiring

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A module's POST /v1/notifications cannot be used to mail its own words from the company address,
// in a single notice or in a digest, and obeys the module's email switch.
func TestModuleNotificationEmailCarriesNoModuleText(t *testing.T) {
	w := newWorld(t)
	company := filepath.Join(w.dir, "company.yaml")
	write := func(rules string) {
		if err := os.WriteFile(company, []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\nemail:\n  relay: {host: smtp.relay.example}\n"+rules), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	relay := &capture{}
	mail := email.New(w.db, audit.New(w.db), email.Source{CompanyFile: company}, relay, log)
	now := time.Now()
	ntf := notify.New(w.db, notify.Options{Mailer: email.NotifyMailer{S: mail}, Log: log, Now: func() time.Time { return now }})
	core := httptest.NewServer((&moduleapi.Server{Auth: modauth.New(w.tr, w.db), Registry: w.reg, Bus: events.NewBus(w.db), Log: log, Notify: ntf}).Handler())
	t.Cleanup(core.Close)

	man := config.Module{ID: "crm", Name: "CRM", Version: "1"}
	man.Requires.Capabilities = []string{"notify"}
	if err := w.reg.Register(w.ctx, store.Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled, HMACSecret: "s", Manifest: man}); err != nil {
		t.Fatal(err)
	}
	w.tr.Add("projected-crm", "mod-crm", "crm")
	settings := func(digest string) {
		if err := ntf.SetSettings(w.ctx, notify.Settings{Subject: "user:alice", Email: "alice@acme.example", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(key string) {
		req := httptest.NewRequest("POST", core.URL+"/v1/notifications", strings.NewReader(
			`{"subject":"user:alice","type":"crm.notice","dedupe_key":"`+key+`","data":{"title":"Reset your password","body":"http://evil.example"}}`))
		req.RequestURI = ""
		req.Header.Set("Authorization", "Bearer projected-crm")
		req.Header.Set("Content-Type", "application/json")
		resp, err := core.Client().Do(req)
		if err != nil || resp.StatusCode != 202 {
			t.Fatal(resp, err)
		}
		_ = resp.Body.Close()
	}
	settings("off")
	post("one")
	settings("hourly")
	post("two")
	post("three")
	now = now.Add(2 * time.Hour)
	if _, err := ntf.FlushDigests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mail.RunOnce(w.ctx); err != nil {
		t.Fatal(err)
	}
	if len(relay.msgs) != 2 {
		t.Fatalf("%d messages", len(relay.msgs))
	}
	for _, m := range relay.msgs {
		if strings.Contains(m, "password") || strings.Contains(m, "evil.example") || !strings.Contains(m, "from the crm module") {
			t.Fatalf("module text was mailed:\n%s", m)
		}
	}

	write("  modules:\n    crm: {enabled: false}\n")
	settings("off")
	post("four")
	if _, err := mail.RunOnce(w.ctx); err != nil {
		t.Fatal(err)
	}
	if len(relay.msgs) != 2 {
		t.Fatal("a module with email turned off still sent through its notifications")
	}
}

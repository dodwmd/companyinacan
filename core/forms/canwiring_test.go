package forms

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const formsPAT = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func canHarness(t *testing.T) (*harness, *fakecore.Core) {
	t.Helper()
	core := fakecore.New(t, "forms")
	cfg, err := ParseConfig([]byte(harnessCfg))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = t.TempDir() + "/forms.db"
	h := &harness{t: t, clock: newClock(), mailer: &fakeMailer{}, captcha: &fakeCaptcha{ok: true}}
	app, err := NewApp(cfg, Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: adminToken, APIReadToken: readToken, CaptchaSecret: "cs"},
		Options{Mailer: h.mailer, Captcha: h.captcha, Now: h.clock.now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Can: core.Client(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(ctx)
	})
	h.app, h.pub, h.api = app, app.PublicHandler(), app.APIHandler()
	return h, core
}

func TestPersonalAccessTokenReadsLeadsByPermission(t *testing.T) {
	h, core := canHarness(t)
	core.AddToken(formsPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermLeadsRead}})
	core.AddToken(formsPAT+"X", fakecore.Token{Subject: "user:eve", Permissions: []string{PermLeadsRead}, Module: "work"})
	if rec := h.apiDo("GET", "/v1/leads", formsPAT, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := h.apiDo("GET", "/v1/leads", formsPAT+"X", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token bound to another module: %d", rec.Code)
	}
	if rec := h.apiDo("GET", "/v1/leads", adminToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("the static token must keep working: %d", rec.Code)
	}
	core.SetDown(true)
	core.AddToken(formsPAT+"N", fakecore.Token{Subject: "user:new", Permissions: []string{PermLeadsRead}})
	if rec := h.apiDo("GET", "/v1/leads", formsPAT+"N", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("can-core down: %d", rec.Code)
	}
}

func TestLeadCreatedIsPublishedThroughCanCoreWithoutPersonalData(t *testing.T) {
	h, core := canHarness(t)
	tok := h.token("contact")
	rec := h.postForm("/_forms/contact", goodValues(tok), nil)
	if rec.Code >= 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h.app.dispatcher.Flush()
	evs := core.Published()
	if len(evs) != 1 || evs[0].Type != "lead.created.v1" || !strings.HasPrefix(evs[0].SourceID, "evt_") {
		t.Fatalf("%+v", evs)
	}
	data := string(evs[0].Data)
	for _, pii := range []string{"Ada", "ada@example.com", "Hello there"} {
		if strings.Contains(data, pii) {
			t.Errorf("personal data %q travels in the event: %s", pii, data)
		}
	}
	if !strings.Contains(data, `"lead_id":"ld_`) || !strings.Contains(data, `"form_id":"contact"`) {
		t.Fatalf("%s", data)
	}
	// the lead itself is stored with everything, readable through the capability
	if rec := h.apiDo("GET", "/v1/leads", adminToken, ""); !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Fatalf("lead not stored: %s", rec.Body)
	}
}

func TestLeadIsKeptWhenCanCoreIsDown(t *testing.T) {
	h, core := canHarness(t)
	core.SetDown(true)
	h.app.dispatcher.sleep = func(context.Context, time.Duration) error { return nil }
	tok := h.token("contact")
	if rec := h.postForm("/_forms/contact", goodValues(tok), nil); rec.Code >= 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h.app.dispatcher.Flush()
	if len(core.Published()) != 0 {
		t.Fatal("published while down")
	}
	if rec := h.apiDo("GET", "/v1/leads", adminToken, ""); !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Fatalf("a can-core outage lost the lead: %s", rec.Body)
	}
}

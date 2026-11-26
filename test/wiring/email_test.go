package wiring

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
)

type capture struct {
	mu   sync.Mutex
	to   []string
	msgs []string
}

func (c *capture) Send(_ context.Context, _ email.Relay, to string, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.to, c.msgs = append(c.to, to), append(c.msgs, string(msg))
	return nil
}

// A public form post -> forms -> can-core's email.send (projected token, real forms manifest) ->
// outbox -> relay, to the configured notify_email only.
func TestFormSubmissionProducesAnEmail(t *testing.T) {
	w := newWorld(t)
	_, can, _ := w.register("forms")

	company := filepath.Join(w.dir, "company.yaml")
	if err := os.WriteFile(company, []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\nmodules:\n  forms: {settings: {notify_email: [owner@personal.example]}}\nemail:\n  relay: {host: smtp.relay.example}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	relay := &capture{}
	mail := email.New(w.db, audit.New(w.db), email.Source{CompanyFile: company}, relay, log)
	core := httptest.NewServer((&moduleapi.Server{Auth: modauth.New(w.tr, w.db), Registry: w.reg, Bus: events.NewBus(w.db), Log: log, Email: mail}).Handler())
	t.Cleanup(core.Close)
	can.BaseURL = core.URL

	cfg, err := forms.ParseConfig([]byte("forms:\n  - id: contact\n    name: Contact\n    min_fill_seconds: 0\n    notify_email: [owner@personal.example]\n    fields:\n      - {name: email, type: email, required: true}\n      - {name: message, type: text, required: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = filepath.Join(t.TempDir(), "forms.db")
	app, err := forms.NewApp(cfg, forms.Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: "admin-token-0123456789"}, forms.Options{Can: can, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	app.PublicHandler().ServeHTTP(r, httptest.NewRequest("GET", "/_forms/contact/token", nil))
	var tk struct{ Token string }
	_ = json.Unmarshal(r.Body.Bytes(), &tk)
	req := httptest.NewRequest("POST", "/_forms/contact", strings.NewReader("email=ada%40visitor.example&message=please+call+me&_t="+tk.Token))
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = httptest.NewRecorder()
	app.PublicHandler().ServeHTTP(r, req)
	if r.Code >= 400 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = app.Close(ctx)
	if _, err := mail.RunOnce(w.ctx); err != nil {
		t.Fatal(err)
	}
	if len(relay.to) != 1 || relay.to[0] != "owner@personal.example" {
		t.Fatalf("recipients: %v", relay.to)
	}
	for _, want := range []string{"Reply-To: <ada@visitor.example>", "please call me", "Subject: New Contact submission"} {
		if !strings.Contains(relay.msgs[0], want) {
			t.Errorf("message lacks %q:\n%s", want, relay.msgs[0])
		}
	}
	// Once sent, can-core keeps neither the address nor the content.
	var kept string
	if err := w.db.QueryRow(`SELECT group_concat(recipient||reply_to||subject||body, '') FROM email_outbox`).Scan(&kept); err != nil || kept != "" {
		t.Fatal(kept, err)
	}
	w.noPersonalDataInCanCore("ada@visitor.example", "please call me")
}

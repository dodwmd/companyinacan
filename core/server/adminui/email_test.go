package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
)

type refusingRelay struct{}

func (refusingRelay) Send(context.Context, email.Relay, string, []byte) error {
	return &email.SendError{Stage: "RCPT TO", Code: 550, Permanent: true}
}

func TestEmailDeliveryPage(t *testing.T) {
	p := newPUI(t)
	company := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(company, []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\nemail:\n  relay: {host: smtp.relay.example}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := email.New(p.db, p.audit, email.Source{CompanyFile: company}, refusingRelay{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.Now = p.db.Now
	root := p.loginAs("root")

	if r := p.do("GET", "/email", nil, root.with()...); r.StatusCode != 503 {
		t.Fatal("without the service:", r.StatusCode)
	}
	p.ui.Email = svc
	ids, _, err := svc.Send(context.Background(), email.Request{Source: "forms", To: []string{"salesperson@acme.example"}, Subject: "very private subject", Text: "very private body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	must(t, p.rb.Grant(context.Background(), "user:root", "user:alice", "operator", ""))
	must(t, p.rb.Grant(context.Background(), "user:root", "user:carol", "auditor", ""))
	alice, carol := p.loginAs("alice"), p.loginAs("carol")

	r := p.do("GET", "/email", nil, alice.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "s***@acme.example") || !strings.Contains(r.body, "RCPT TO: the relay answered 550") || !strings.Contains(r.body, "smtp.relay.example") {
		t.Fatal(r.StatusCode, r.body)
	}
	for _, hidden := range []string{"salesperson", "very private"} {
		if strings.Contains(r.body, hidden) {
			t.Errorf("the page shows %q", hidden)
		}
	}
	recs, err := p.audit.Query(context.Background(), audit.Filter{ActionPrefix: "email.view", Limit: 5})
	if err != nil || len(recs) != 1 || recs[0].Actor != "user:alice" {
		t.Fatal("the view is not audited:", recs, err)
	}

	if r := p.do("GET", "/email", nil, carol.with()...); r.StatusCode != 403 {
		t.Fatal("auditor:", r.StatusCode)
	}
	if r := p.do("POST", "/email/"+ids[0]+"/requeue", url.Values{}, alice.with()...); r.StatusCode != 403 {
		t.Fatal("without csrf:", r.StatusCode)
	}
	if r := p.do("POST", "/email/eml_missing/requeue", url.Values{"csrf": {alice.csrf}}, alice.with()...); r.StatusCode != 404 {
		t.Fatal("unknown id:", r.StatusCode)
	}
	if r := p.do("POST", "/email/"+ids[0]+"/requeue", url.Values{"csrf": {alice.csrf}}, alice.with()...); r.StatusCode != 303 {
		t.Fatal("requeue:", r.StatusCode, r.body)
	}
	if m, err := svc.Get(context.Background(), "forms", ids[0]); err != nil || m.Status != email.StatusPending {
		t.Fatal(m, err)
	}
}

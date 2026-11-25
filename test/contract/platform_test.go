package contract

// Epic E11a: approvals, notifications and RBAC. Event schemas with fixtures, the events the engine
// really emits validated against them, and the company.yaml / module.yaml additions.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/schemas"
	sevents "github.com/dodwmd/companyinacan/schemas/events"
)

var platformEvents = map[string][]byte{
	"approval.requested.v1": sevents.ApprovalRequestedV1,
	"approval.decided.v1":   sevents.ApprovalDecidedV1,
	"notification.sent.v1":  sevents.NotificationSentV1,
}

func TestPlatformEventFixtures(t *testing.T) {
	for name, raw := range platformEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", raw, "")
			counts := map[string]int{}
			for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
				files, _ := filepath.Glob(filepath.Join(root, "testdata", "events", name, kind+"-*.json"))
				for _, f := range files {
					counts[kind]++
					b, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					err = validateJSON(t, s, b)
					if wantValid && err != nil {
						t.Errorf("%s should be valid: %v", filepath.Base(f), err)
					}
					if !wantValid && err == nil {
						t.Errorf("%s should be rejected", filepath.Base(f))
					}
				}
			}
			if counts["valid"] < 2 || counts["invalid"] < 8 {
				t.Errorf("fixtures: %v", counts)
			}
		})
	}
}

// Every event the engine and the notification service really publish validates against its schema.
func TestPlatformEventsEmittedMatchSchemas(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	al := audit.New(db)
	rb := rbac.New(db, al, rbac.Options{Now: func() time.Time { return now }})
	bus := events.NewBus(db)
	ntf := notify.New(db, notify.Options{Now: func() time.Time { return now }, Mailer: &notify.FakeMailer{}, Bus: bus, Roles: rb})
	eng := approvals.New(db, al, approvals.Deps{Roles: rb, Perms: rb, Notifier: ntf, Bus: bus, Now: func() time.Time { return now }})
	if err := eng.SyncPolicies(ctx, map[string]config.ApprovalPolicy{"expense": {Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{User: "user:boss"}}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := ntf.SetSettings(ctx, notify.Settings{Subject: "user:boss", Email: "boss@example.com", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "off"}); err != nil {
		t.Fatal(err)
	}
	r, _, err := eng.Create(ctx, approvals.CreateRequest{ModuleID: "expenses", Type: "expense", Requester: "user:emp", Ref: "e1", Title: "Secret", Amount: 5, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Decide(ctx, r.ID, "user:boss", "approve", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, type, time, source, data_json FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]int{}
	for rows.Next() {
		var id, typ, src, data string
		var ts int64
		if err := rows.Scan(&id, &typ, &ts, &src, &data); err != nil {
			t.Fatal(err)
		}
		raw, ok := platformEvents[typ]
		if !ok {
			continue
		}
		seen[typ]++
		env, _ := json.Marshal(map[string]any{"id": id, "type": typ, "time": time.Unix(ts, 0).UTC().Format(time.RFC3339), "source": src, "data": json.RawMessage(data)})
		if err := validateJSON(t, compileSchema(t, typ+".json", raw, ""), env); err != nil {
			t.Errorf("%s: %v\n%s", typ, err, env)
		}
	}
	for typ := range platformEvents {
		if seen[typ] == 0 {
			t.Errorf("no %s event was emitted", typ)
		}
	}
}

func TestCompanyAndModuleSchemasAcceptThePlatformFields(t *testing.T) {
	company := compileSchema(t, "company.json", schemas.CompanyV1, "")
	ok := `{"schema":1,"name":"Acme","domain":"acme.example.com","provider":{"type":"local"},
	  "approvals":{"expense":{"steps":[{"name":"manager","approvers":[{"manager_of_requester":true}],"fallback":[{"role":"finance"}]},
	    {"name":"finance","approvers":[{"role":"finance"}],"quorum":2,"when":{"amount_gte":100000}},{"name":"legal","parallel":true,"approvers":[{"user":"user:lena"}]}],
	    "expires_after":"7d","escalate_after":"48h","escalate_to":[{"role":"director"}]}},
	  "notifications":{"webhooks":{"ops":"https://hooks.example.com/T1"}}}`
	if err := validateJSON(t, company, []byte(ok)); err != nil {
		t.Fatalf("valid company rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"approver with two fields": `{"expense":{"steps":[{"name":"a","approvers":[{"role":"x","user":"user:y"}]}]}}`,
		"no steps":                 `{"expense":{"steps":[]}}`,
		"bad duration":             `{"expense":{"steps":[{"name":"a","approvers":[{"role":"x"}]}],"expires_after":"soon"}}`,
		"unknown policy key":       `{"expense":{"steps":[{"name":"a","approvers":[{"role":"x"}]}],"allow_self_approval":true}}`,
		"bad user subject":         `{"expense":{"steps":[{"name":"a","approvers":[{"user":"alice"}]}]}}`,
	} {
		doc := `{"schema":1,"name":"Acme","domain":"acme.example.com","provider":{"type":"local"},"approvals":` + bad + `}`
		if validateJSON(t, company, []byte(doc)) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if validateJSON(t, company, []byte(`{"schema":1,"name":"Acme","domain":"acme.example.com","provider":{"type":"local"},"notifications":{"webhooks":{"ops":"http://insecure.example.com"}}}`)) == nil {
		t.Error("an http webhook channel was accepted")
	}
}

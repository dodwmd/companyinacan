package identity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
)

const (
	party = "pers_0123456789abcdef01234567"
	other = "pers_ffffffffffffffffffffffff"
	subj  = "user:sso-abc"
)

type env struct {
	t   *testing.T
	db  *store.DB
	svc *Service
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, now: time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)}
	db.Now = func() time.Time { return e.now }
	e.svc = &Service{DB: db, Audit: audit.New(db), Now: func() time.Time { return e.now }}
	return e
}

func provisioner() store.Module {
	m := config.Module{ID: "provisioning", Version: "1"}
	m.Provides = append(m.Provides, struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
		Status     string `yaml:"status"`
	}{Capability: "identity.provision"})
	return store.Module{ID: "provisioning", Manifest: m}
}

func (e *env) offboard(source, partyID, date, subject string) {
	e.t.Helper()
	d := map[string]any{"party_id": partyID, "kind": "employee", "effective_date": date}
	if subject != "" {
		d["subject_ref"] = SubjectRef(subject)
	}
	b, _ := json.Marshal(d)
	e.svc.HandleOffboarded(context.Background(), events.Envelope{ID: "evt_1", Type: "person.offboarded.v1", Source: source, Data: b})
}

func (e *env) seed() {
	e.t.Helper()
	for _, q := range []string{
		`INSERT INTO admin_users(id,username,password_hash,totp_secret,created_at,auth_source) VALUES (7,'sso-abc','!','',1,'sso'),(8,'sso-keep','!','',1,'sso')`,
		`INSERT INTO sessions(id_hash,user_id,csrf_token,ip,created_at,expires_at,source) VALUES ('h1',7,'c','1.1.1.1',1,99999999999,'sso'),('h2',7,'c','1.1.1.1',1,99999999999,'sso'),('h3',8,'c','1.1.1.1',1,99999999999,'sso')`,
		`INSERT INTO api_tokens(id,name,module_id,subject,permissions,token_hash,created_by,created_at,expires_at,revoked_at) VALUES
		 ('pat_1','a','people','user:sso-abc','[]','x1','user:sso-abc',1,99999999999,0),
		 ('pat_2','b','jobs','user:sso-abc','[]','x2','user:sso-abc',1,99999999999,0),
		 ('pat_3','c','people','user:sso-keep','[]','x3','user:sso-keep',1,99999999999,0)`,
	} {
		if _, err := e.db.ExecContext(context.Background(), q); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestRevokeHappyPathAndIdempotent(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.offboard("people", party, "2026-06-10", subj)
	for i := 0; i < 2; i++ { // the second call changes nothing and still succeeds
		res, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (res.Tokens != 2 || res.Sessions != 2) {
			t.Fatalf("result %+v", res)
		}
	}
	var live, sess, keepSess int
	_ = e.db.QueryRow(`SELECT count(*) FROM api_tokens WHERE revoked_at=0`).Scan(&live)
	_ = e.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=7`).Scan(&sess)
	_ = e.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=8`).Scan(&keepSess)
	if live != 1 || sess != 0 || keepSess != 1 {
		t.Fatalf("live tokens %d, sessions %d, other sessions %d", live, sess, keepSess)
	}
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action='identity.revoke' AND target=?`, party).Scan(&n)
	if n != 2 {
		t.Fatalf("audit entries %d", n)
	}
}

func TestRevokeRefusals(t *testing.T) {
	crm := func() store.Module { return store.Module{ID: "crm"} }
	cases := []struct {
		name  string
		setup func(e *env)
		mod   func() store.Module
		req   Request
		want  error
	}{
		{"not offboarded", func(e *env) {}, provisioner, Request{party, subj}, ErrNotOffboarded},
		{"event from another source is ignored", func(e *env) { e.offboard("jobs", party, "2026-06-01", subj) }, provisioner, Request{party, subj}, ErrNotOffboarded},
		{"effective date in the future", func(e *env) { e.offboard("people", party, "2026-06-11", subj) }, provisioner, Request{party, subj}, ErrNotOffboarded},
		{"subject not linked to the person", func(e *env) { e.offboard("people", party, "2026-06-01", subj) }, provisioner, Request{party, "user:sso-keep"}, ErrSubjectMismatch},
		{"person without account link", func(e *env) { e.offboard("people", party, "2026-06-01", "") }, provisioner, Request{party, subj}, ErrSubjectMismatch},
		{"other party", func(e *env) { e.offboard("people", party, "2026-06-01", subj) }, provisioner, Request{other, subj}, ErrNotOffboarded},
		{"bad subject shape", func(e *env) { e.offboard("people", party, "2026-06-01", subj) }, provisioner, Request{party, "admin"}, ErrInvalid},
		{"bad party shape", func(e *env) {}, provisioner, Request{"x", subj}, ErrInvalid},
		{"module not entitled", func(e *env) { e.offboard("people", party, "2026-06-01", subj) }, crm, Request{party, subj}, ErrNotEntitled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.seed()
			c.setup(e)
			_, err := e.svc.Revoke(context.Background(), c.mod(), c.req)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			var live int
			_ = e.db.QueryRow(`SELECT count(*) FROM api_tokens WHERE revoked_at=0`).Scan(&live)
			if live != 3 {
				t.Fatalf("a refused call revoked tokens: %d live", live)
			}
		})
	}
}

func TestRevokeRateLimited(t *testing.T) {
	e := newEnv(t)
	e.svc.PerMinute = 3
	e.offboard("people", party, "2026-06-01", subj)
	var last error
	for i := 0; i < 5; i++ {
		_, last = e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj})
	}
	if !errors.Is(last, ErrRateLimited) {
		t.Fatalf("err = %v", last)
	}
	e.now = e.now.Add(61 * time.Second)
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); err != nil {
		t.Fatalf("window did not reset: %v", err)
	}
}

func TestMalformedOffboardedEventsAreIgnored(t *testing.T) {
	e := newEnv(t)
	for _, d := range []string{`{}`, `{"party_id":"bad","effective_date":"2026-06-01"}`, `{"party_id":"` + party + `","effective_date":"soon"}`, `not json`, `{"party_id":"` + party + `","effective_date":"2026-06-01","subject_ref":"zz"}`} {
		e.svc.HandleOffboarded(context.Background(), events.Envelope{Source: "people", Type: "person.offboarded.v1", Data: json.RawMessage(d)})
	}
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM offboarded_parties`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows recorded", n)
	}
}

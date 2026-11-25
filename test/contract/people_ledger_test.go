package contract

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/people"
)

// The erasure tombstones must be accepted by the real E08 ledger.
func TestErasureTombstonesFitTheBackupLedger(t *testing.T) {
	kr, err := people.ParseKeyring("k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	if err != nil {
		t.Fatal(err)
	}
	s, err := people.Open(filepath.Join(t.TempDir(), "p.db"), people.Options{Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	hr := people.Actor{Subject: "svc", Roles: []string{people.RoleHR, people.RoleAdmin}}
	p, err := s.CreatePerson(ctx, hr, people.PersonInput{Kind: "employee", GivenName: "Eve", FamilyName: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeStatus(ctx, hr, p.ID, people.StFormer, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestErasure(ctx, hr, p.ID); err != nil {
		t.Fatal(err)
	}
	ents, err := s.ListErasures(ctx, hr, 0)
	if err != nil || len(ents) != len(people.Classes) {
		t.Fatalf("tombstones = %v, %v", ents, err)
	}
	l := &backup.Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	for _, e := range ents {
		if _, added, err := l.Append(e.Subject, e.Class); err != nil || !added {
			t.Fatalf("ledger rejected %+v: added=%v err=%v", e, added, err)
		}
	}
	es, err := l.Read()
	if err != nil || len(es) != len(ents) || backup.VerifyChain(es) != nil {
		t.Fatalf("ledger = %v, %v", es, err)
	}
}

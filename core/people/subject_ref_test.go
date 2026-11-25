package people

import (
	"strings"
	"testing"
)

// person.offboarded.v1 carries a reference to the person's linked account (E13d) so can-core can
// check an access revocation, and never the account subject itself.
func TestOffboardedEventCarriesOnlyAHashOfTheLinkedAccount(t *testing.T) {
	e := newEnv(t)
	linked, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Lin", FamilyName: "Ked", SSOSubject: "user:sso-linked"})
	e.must(err)
	unlinked, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Un", FamilyName: "Linked"})
	e.must(err)
	for _, id := range []string{linked.ID, unlinked.ID} {
		for _, st := range []Status{StOffboarding, StFormer} {
			_, err := e.s.ChangeStatus(e.ctx, e.hr, id, st, "", "")
			e.must(err)
		}
	}
	evs, err := e.s.AllEvents(e.ctx)
	e.must(err)
	seen := 0
	for _, ev := range evs {
		if ev.Type != EventPersonOffboarded {
			continue
		}
		seen++
		ref, has := ev.Data["subject_ref"]
		switch ev.Data["party_id"] {
		case linked.ID:
			if ref != SubjectRef("user:sso-linked") || len(SubjectRef("x")) != 32 {
				t.Errorf("subject_ref = %v", ref)
			}
		case unlinked.ID:
			if has {
				t.Errorf("a person with no account link has subject_ref %v", ref)
			}
		}
		for k, v := range ev.Data {
			if s, ok := v.(string); ok && strings.Contains(s, "sso-linked") {
				t.Errorf("event field %s carries the subject", k)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("offboarded events: %d", seen)
	}
}

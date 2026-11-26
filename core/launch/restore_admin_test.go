package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// `can up` after a restore must neither reset the preserved admin nor create another one (the
// restored accounts may be named differently).
func TestUpAfterARestoreKeepsTheAdmin(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "admin bootstrap", err: errors.New("admin user already exists")},
		{contains: "restore status", out: "normal restored abc123\n"},
	}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Admin != nil || !strings.Contains(res.AdminNote, "restored") {
		t.Fatalf("admin=%v note=%q", res.Admin, res.AdminNote)
	}
	for _, c := range k.calls {
		if a := strings.Join(c.Args, " "); strings.Contains(a, "reset-password") || strings.Contains(a, "admin bootstrap") {
			t.Fatal("an account was created or reset on a restored server:", a)
		}
	}
}

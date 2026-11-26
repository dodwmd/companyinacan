package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// `can up` after a restore must not reset the admin password and TOTP seed the restore preserved.
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
		if strings.Contains(strings.Join(c.Args, " "), "reset-password") {
			t.Fatal("the restored admin was reset")
		}
	}
}

package backup

import (
	"errors"
	"os"
	"testing"
)

// The progress file holds nothing derived from the restic password.
func TestRestoreStateHoldsNothingOfThePassword(t *testing.T) {
	a, b := newRestoreEnv(t), newRestoreEnv(t)
	b.o.Kit.Restic.Password = "another-password-entirely-0123456789"
	if a.o.fingerprint() != b.o.fingerprint() {
		t.Error("the fingerprint depends on the password")
	}
	a.r.failOnce["apply"] = errors.New("x")
	_ = a.run()
	raw, err := os.ReadFile(a.o.StatePath)
	if err != nil || len(raw) == 0 {
		t.Fatal(err)
	}
}

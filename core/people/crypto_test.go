package people

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hexKeyA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	hexKeyB = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
)

func TestSealOpenRoundTripAndBinding(t *testing.T) {
	kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := kr.Seal("1980-01-02", "pers_x:date_of_birth")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "1980") || !strings.HasPrefix(sealed, "v1.k1.") {
		t.Fatalf("unexpected sealed form %q", sealed)
	}
	got, err := kr.Open(sealed, "pers_x:date_of_birth")
	if err != nil || got != "1980-01-02" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Ciphertext moved to another person or field must not open (row-swap attack).
	if _, err := kr.Open(sealed, "pers_y:date_of_birth"); err == nil {
		t.Error("opened under a different person")
	}
	if _, err := kr.Open(sealed, "pers_x:national_id"); err == nil {
		t.Error("opened under a different field")
	}
	if _, err := kr.Open(sealed[:len(sealed)-2]+"AA", "pers_x:date_of_birth"); err == nil {
		t.Error("tampered ciphertext opened")
	}
	again, _ := kr.Seal("1980-01-02", "pers_x:date_of_birth")
	if again == sealed {
		t.Error("nonce reuse")
	}
}

func TestKeyRotation(t *testing.T) {
	old, _ := ParseKeyring("k1 " + hexKeyA + "\n")
	sealed, _ := old.Seal("secret", "a")
	rotated, err := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ActiveID() != "k2" {
		t.Fatalf("active = %s", rotated.ActiveID())
	}
	if v, err := rotated.Open(sealed, "a"); err != nil || v != "secret" {
		t.Fatalf("old data must open with the retired key: %q %v", v, err)
	}
	if !rotated.NeedsRotation(sealed) {
		t.Error("data under k1 should need rotation")
	}
	n, _ := rotated.Seal("x", "a")
	if rotated.NeedsRotation(n) {
		t.Error("fresh data should not need rotation")
	}
	dropped, _ := ParseKeyring("k2 " + hexKeyB + "\n")
	if _, err := dropped.Open(sealed, "a"); err == nil {
		t.Error("a key that was removed must not open its data")
	}
}

func TestParseKeyringRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      "",
		"short key":  "k1 abcd\n",
		"bad hex":    "k1 " + strings.Repeat("zz", 32) + "\n",
		"dup id":     "k1 " + hexKeyA + "\nk1 " + hexKeyB + "\n",
		"bad id":     "K.1 " + hexKeyA + "\n",
		"no id":      hexKeyA + "\n",
		"only comms": "# nothing\n",
	} {
		if _, err := ParseKeyring(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadKeyringFileRefusesOpenPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(p, []byte("k1 "+hexKeyA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(p); err == nil {
		t.Fatal("group/world readable key file accepted")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(p); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing key file accepted")
	}
}

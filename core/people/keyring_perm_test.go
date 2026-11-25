package people

import (
	"os"
	"path/filepath"
	"testing"
)

// A mounted Secret in a non-root pod is readable through fsGroup, so mode 0440 (group read) must
// load; anything writable, executable or reachable by others must still be refused.
func TestLoadKeyringAcceptsGroupReadOnlyForMountedSecrets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(p, []byte("k1 "+hexKeyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]bool{0o400: true, 0o440: true, 0o600: true, 0o640: false, 0o460: false, 0o404: false, 0o444: false, 0o640 | 0o010: false} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyring(p)
		if (err == nil) != want {
			t.Errorf("mode %o: accepted=%v, want %v (%v)", mode, err == nil, want, err)
		}
	}
}

package contract

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A JSON reader keeps the last of two members with one name without saying so, so a schema edited
// twice looks right and is not what it reads as.
func TestSchemasHaveNoDuplicateKeys(t *testing.T) {
	found := 0
	err := filepath.WalkDir(filepath.Join("..", "..", "schemas"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // walked from the repository
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		var value func() error
		value = func() error {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			open, ok := tok.(json.Delim)
			if !ok {
				return nil
			}
			seen := map[string]bool{}
			for dec.More() {
				if open == '{' {
					k, err := dec.Token()
					if err != nil {
						return err
					}
					name, _ := k.(string)
					if seen[name] {
						t.Errorf("%s: %q appears twice in one object (near byte %d)", path, name, dec.InputOffset())
					}
					seen[name] = true
				}
				if err := value(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		found++
		return value()
	})
	if err != nil || found < 2 {
		t.Fatalf("read %d schemas: %v", found, err)
	}
}

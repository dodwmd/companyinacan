package contract

import (
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every service applies the migrations above the highest number it has recorded, so two files with
// one number (two branches merged side by side) would break or silently skip one of them.
func TestMigrationNumbersAreUnique(t *testing.T) {
	seen := map[string]string{} // "dir|number" -> file
	found := 0
	err := filepath.WalkDir(filepath.Join("..", "..", "core"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") || filepath.Base(filepath.Dir(path)) != "migrations" {
			return err
		}
		num, _, _ := strings.Cut(filepath.Base(path), "_")
		n, err := strconv.Atoi(num)
		if err != nil {
			t.Errorf("%s: a migration name starts with its number", path)
			return nil
		}
		key := filepath.Dir(path) + "|" + strconv.Itoa(n)
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s have the same migration number", other, path)
		}
		seen[key] = path
		found++
		return nil
	})
	if err != nil || found < 10 {
		t.Fatalf("found %d migrations: %v", found, err)
	}
}

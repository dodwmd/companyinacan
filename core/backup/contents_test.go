package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup/backuptest"
)

// A recorded module with a restore hook and a dump is complete; a never-provisioned database
// module is not reported as an unbacked database.
func TestCheckContentsComplete(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		StateDump: string(backuptest.StateDB(t,
			backuptest.Module{ID: "stub", State: "enabled", Database: "none", RestoreHook: "./hooks/restore.sh"},
			backuptest.Module{ID: "crm", State: "failed", Database: "mariadb"})),
		SecretsDump: `{"version":1,"secrets":[{"namespace":"mod-crm","name":"can-db","data":{"DB_PASSWORD":"eA=="}}]}`,
		CompanyDump: "c",
		DumpedList:  "stub none\n",
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := CheckContents(context.Background(), dir, []string{StateDump, SecretsDump, CompanyDump, DumpedList, "modules/stub/probe.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Problems(); len(p) != 0 {
		t.Fatal(p)
	}
	if _, err := CheckContents(context.Background(), dir, []string{StateDump, CompanyDump, DumpedList}); err == nil {
		t.Fatal("DUMPED names a module whose files are not in the set")
	}
}

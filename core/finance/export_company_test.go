package finance

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/modbackup"
)

// `can-finance export` (specs/070-company-export) opens the sealed bank account numbers only with
// --include-sealed, with the binding the service seals them with.
func TestCompanyExportOfBankNumbers(t *testing.T) {
	e := sealedEnv(t)
	b := e.bankAccount(e.token("ada", ledAdm, nil, nil, false))
	var db string
	if err := e.app.store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "finance.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: "+db+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	export := func(name string, args ...string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		var log bytes.Buffer
		env := func(k string) string {
			if k == "CAN_FINANCE_SEAL_KEY" {
				return sealHex
			}
			return ""
		}
		if code := modbackup.Main("can-finance", Migrations(), append([]string{"export", "-config", cfg, "-out", out}, args...), env, &log, &log); code != 0 {
			t.Fatalf("exit %d: %s", code, log.String())
		}
		f, err := os.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		tr := tar.NewReader(f)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				t.Fatal("no bank_accounts.jsonl")
			}
			if err != nil {
				t.Fatal(err)
			}
			if h.Name == "records/bank_accounts.jsonl" {
				body, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			}
		}
	}
	if got := export("plain.tar"); strings.Contains(got, "DE89370400440532013000") || !strings.Contains(got, `"sealed_number":"[sealed]"`) {
		t.Errorf("without the flag: %s", got)
	}
	if got := export("open.tar", "-include-sealed"); !strings.Contains(got, `"sealed_number":"DE89370400440532013000"`) {
		t.Errorf("account %d with the flag: %s", b.ID, got)
	}
}

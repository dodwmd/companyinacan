package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/site"
)

// Every valid company fixture and the shipped example must render: a schema-valid site: section
// can never make the renderer fail (the schema/renderer agreement is table-tested in core/site; only
// logo files, whose existence is not a schema matter, can still fail a build).
func TestValidCompaniesRender(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "testdata", "valid", "company-*.yaml"))
	files = append(files, filepath.Join(root, "examples", "acme-co", "company.yaml"))
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			doc, ps := config.Load(f)
			if doc == nil {
				t.Fatal(ps)
			}
			c, err := config.DecodeCompany(doc)
			if err != nil {
				t.Fatal(err)
			}
			out, err := site.Render(c, site.Options{Assets: os.DirFS(filepath.Join(root, "testdata", "site"))})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(string(out.Files["index.html"]), c.Name) {
				t.Error("company name missing from index.html")
			}
		})
	}
}

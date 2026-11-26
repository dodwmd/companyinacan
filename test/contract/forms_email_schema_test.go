package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// The forms notify_email setting accepts what can-core will send to, and nothing more.
func TestFormsNotifyEmailSchemaMatchesCanCore(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "modules", "forms", "settings.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]struct {
			Items struct{ Pattern string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(s.Properties["notify_email"].Items.Pattern)
	for _, a := range []string{
		"a@b.example", "o'neil+x@sub.acme.example", "a.b@c.example", "a@b", "a%x@b.example", "a!x@b.example", ".a@b.example", "a.@b.example",
		"a..b@c.example", "a@-b.example", "a@b-.example", "A <a@b.example>", "a@b.example,c@d.example", "ü@b.example", "a b@c.example", "a@b..example",
	} {
		if re.MatchString(a) != config.ValidAddress(a) {
			t.Errorf("%q: schema says %v, can-core says %v", a, re.MatchString(a), config.ValidAddress(a))
		}
	}
}

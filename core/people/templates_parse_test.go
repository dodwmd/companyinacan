package people

import "testing"

// Pages are parsed on first use: this keeps a broken one a test failure, not a request error.
func TestAllTemplatesParse(t *testing.T) {
	u := &UI{}
	u.once.Do(u.init)
	if err := u.tpl.ParseAll(); err != nil {
		t.Fatal(err)
	}
}

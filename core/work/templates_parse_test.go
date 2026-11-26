package work

import (
	"testing"
	"time"
)

// Pages are parsed on first use: this keeps a broken one a test failure, not a request error.
func TestAllTemplatesParse(t *testing.T) {
	u, err := newWebUI(nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.pages.ParseAll(); err != nil {
		t.Fatal(err)
	}
}

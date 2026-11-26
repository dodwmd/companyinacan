package adminui

import "testing"

// Pages are parsed on first use: this keeps a broken one a test failure, not a request error.
func TestAllTemplatesParse(t *testing.T) {
	s := &Server{}
	s.once.Do(s.init)
	if err := s.tpl.ParseAll(); err != nil {
		t.Fatal(err)
	}
}

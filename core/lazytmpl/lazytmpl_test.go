package lazytmpl

import (
	"bytes"
	"sync"
	"testing"
	"testing/fstest"
)

func TestSet(t *testing.T) {
	fsys := fstest.MapFS{
		"p/layout.html":   {Data: []byte(`{{define "layout"}}[{{template "body" .}}]{{end}}`)},
		"p/partials.html": {Data: []byte(``)},
		"p/a.html":        {Data: []byte(`{{define "body"}}a {{.}}{{end}}`)},
		"p/bad.html":      {Data: []byte(`{{define "body"}}{{`)},
	}
	s := &Set{FS: fsys, Dir: "p"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm, err := s.Get("a.html")
			if err != nil {
				t.Error(err)
				return
			}
			var b bytes.Buffer
			if err := tm.ExecuteTemplate(&b, "layout", "<x>"); err != nil || b.String() != "[a &lt;x&gt;]" {
				t.Errorf("render %q: %v", b.String(), err)
			}
		}()
	}
	wg.Wait()
	for _, n := range []string{"missing.html", "layout.html", "../a.html"} {
		if _, err := s.Get(n); err == nil {
			t.Errorf("%s: want an error", n)
		}
	}
	if err := s.ParseAll(); err == nil {
		t.Error("ParseAll must report the broken page")
	}
}

package work

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Rendering is linear in the input and bounded per field: pathological text cannot pin the CPU.
func TestRenderTextIsLinearOnPathologicalInput(t *testing.T) {
	nested := strings.Repeat("- a\n", 12500)
	cases := map[string]string{
		"multibyte":       strings.Repeat("é", 50000),
		"open links":      strings.Repeat("[a](", 12500),
		"open brackets":   strings.Repeat("[", 50000),
		"bold markers":    strings.Repeat("**", 25000),
		"backtick runs":   strings.Repeat("`", 50000),
		"backtick pairs":  strings.Repeat("`a", 25000),
		"bracket parens":  strings.Repeat("[a]", 16000),
		"close links":     strings.Repeat("[a](b", 10000),
		"many short":      strings.Repeat("x\n", 25000),
		"many bullets":    nested,
		"fences":          strings.Repeat("```\n", 12500),
		"mixed multibyte": strings.Repeat("[é](**`é", 6000),
	}
	for name, in := range cases {
		start := time.Now()
		out := renderText(in)
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: %v for %d bytes", name, d, len(in))
		}
		if len(out) > 8*len(in)+1000 {
			t.Errorf("%s: output %d bytes for %d in", name, len(out), len(in))
		}
		for _, h := range liveHazards(string(out)) {
			t.Errorf("%s: %s", name, h)
		}
	}
}

func TestRenderTextTruncatesLongFieldsAndSaysSo(t *testing.T) {
	out := string(renderText(strings.Repeat("word ", 20000)))
	if !strings.Contains(out, `class="hint"`) || strings.Count(out, "word") > uiMaxRenderBytes/4 {
		t.Fatalf("not truncated: %d bytes", len(out))
	}
}

func TestIssuePageWithHugeCommentsRendersQuickly(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	nasty := strings.Repeat("[é](**`é", 2500) // 20000 bytes
	for i := 0; i < 30; i++ {
		f.h.mustDo(201, "POST", "/v1/issues/"+f.acmeKey+"/comments", bootstrap, map[string]any{"body": nasty[:19000] + strconv.Itoa(i)})
	}
	f.h.mustDo(200, "PATCH", "/v1/issues/"+f.acmeKey, bootstrap, map[string]any{"description": strings.Repeat("é", 49000)})
	start := time.Now()
	r := b.do("GET", "/ui/issues/"+f.acmeKey, nil)
	if d := time.Since(start); r.Code != 200 || d > 20*time.Second {
		t.Fatalf("issue page: %d in %v", r.Code, d)
	}
	if !strings.Contains(r.Body, "Comment by") {
		t.Fatal("comments missing")
	}
}

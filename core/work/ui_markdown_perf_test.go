package work

import (
	"html/template"
	"math"
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
		// Linear rendering takes a few milliseconds here even with -race; a byte-by-byte quadratic
		// scan of these inputs takes hundreds of milliseconds or more. The fastest of three runs is
		// compared, so a busy machine does not trip the bound.
		var out template.HTML
		if d := fastestOf(3, func() { out = renderText(in) }); d > 250*time.Millisecond {
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

// fastestOf runs f n times and returns the shortest duration, which is insensitive to load spikes.
func fastestOf(n int, f func()) time.Duration {
	best := time.Duration(math.MaxInt64)
	for range n {
		start := time.Now()
		f()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
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
	// About 30 ms with -race; the page budget keeps it there. Fastest of three, so load does not count.
	code, page := 0, ""
	d := fastestOf(3, func() {
		r := b.do("GET", "/ui/issues/"+f.acmeKey, nil)
		code, page = r.Code, r.Body
	})
	if code != 200 || d > time.Second {
		t.Fatalf("issue page: %d in %v", code, d)
	}
	if !strings.Contains(page, "Comment by") {
		t.Fatal("comments missing")
	}
}

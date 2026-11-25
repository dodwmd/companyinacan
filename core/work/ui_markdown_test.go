package work

import (
	"strings"
	"testing"
)

func TestRenderTextSubset(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                      "<p>plain</p>",
		"a **b** `c` d":              "<p>a <strong>b</strong> <code>c</code> d</p>",
		"one\ntwo\n\nthree":          "<p>one<br>two</p>\n<p>three</p>",
		"- a\n- **b**\n\nafter":      "<ul>\n<li>a</li>\n<li><strong>b</strong></li>\n</ul>\n<p>after</p>",
		"```\n<b>x</b>\n```":         "<pre><code>&lt;b&gt;x&lt;/b&gt;</code></pre>",
		"[t](https://e.com/a?b=1&c)": `<p><a href="https://e.com/a?b=1&amp;c" rel="noopener noreferrer nofollow">t</a></p>`,
		"héllo 日本 <b>":               "<p>héllo 日本 &lt;b&gt;</p>",
		"unterminated **bold":        "<p>unterminated **bold</p>",
		"[open](https://e.com":       "<p>[open](https://e.com</p>",
	} {
		if got := strings.TrimSpace(string(renderText(in))); got != want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestRenderTextNeverEmitsActiveContent(t *testing.T) {
	hostile := []string{
		`<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `[a](javascript:alert(1))`, `[a](JaVaScRiPt:alert(1))`,
		"[a](java\tscript:alert(1))", `[a](data:text/html,x)`, `[a](vbscript:x)`, `[a](//evil.example/)`, `[a](/relative)`,
		`[a](https://u:p@evil.example/)`, `[a](https://e.com/"onmouseover="x)`, `[a](https://e.com/'>)`, `[a](https://e.com/<x>)`,
		"[a](https://e.com/\n)", `![x](https://e.com/i.png)`, `**<script>**`, "`<script>`", "[<script>](https://e.com)",
		`[a](https://e.com/ ' onclick='x)`, `[a]( javascript:alert(1))`, `<a href="https://e.com">x</a>`, "```\n</code></pre><script>x</script>\n```",
	}
	for _, in := range hostile {
		out := string(renderText(in))
		low := strings.ToLower(out)
		for _, bad := range []string{"<script", "<img", "<iframe", `href="javascript`, `href="vbscript`, `href="data:`, `href="//`, `href="/`, `href="https://u`, `" onclick`} {
			if strings.Contains(low, bad) {
				t.Errorf("renderText(%q) = %s contains %q", in, out, bad)
			}
		}
		// Only the tags this renderer owns may appear.
		rest := out
		for _, ok := range []string{"<p>", "</p>", "<br>", "<ul>", "</ul>", "<li>", "</li>", "<pre>", "</pre>", "<code>", "</code>", "<strong>", "</strong>", "</a>"} {
			rest = strings.ReplaceAll(rest, ok, "")
		}
		for strings.Contains(rest, `<a href="`) {
			i := strings.Index(rest, `<a href="`)
			j := strings.Index(rest[i:], `" rel="noopener noreferrer nofollow">`)
			if j < 0 {
				t.Errorf("renderText(%q) = %s: anchor without rel", in, out)
				break
			}
			rest = rest[:i] + rest[i+j+len(`" rel="noopener noreferrer nofollow">`):]
		}
		if strings.Contains(rest, "<") {
			t.Errorf("renderText(%q) = %s: unexpected tag in %q", in, out, rest)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for _, ok := range []string{"https://example.com", "http://example.com/a?b=c#d", "https://example.com:8443/x"} {
		if _, good := safeURL(ok); !good {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "ftp://x/", "javascript:alert(1)", "https:///x", "//x.com", "https://u@x.com", "https://x.com/a b", strings.Repeat("a", 3000)} {
		if _, good := safeURL(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
}

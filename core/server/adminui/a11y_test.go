package adminui

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This is a static, axe-style guard-rail over the generated markup: it catches the regressions that
// are mechanical to detect. It does not replace a manual screen-reader and keyboard pass.

var (
	reTag     = regexp.MustCompile(`(?s)<([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^<>]*?)?)\s*/?>`)
	reAttr    = regexp.MustCompile(`([a-zA-Z:-]+)(?:="([^"]*)")?`)
	reHeading = regexp.MustCompile(`<h([1-6])[\s>]`)
	reStrip   = regexp.MustCompile(`(?s)<[^>]*>`)
)

type tag struct {
	name  string
	attrs map[string]string
	start int
	end   int
}

func tags(html string) []tag {
	var out []tag
	for _, m := range reTag.FindAllStringSubmatchIndex(html, -1) {
		t := tag{name: strings.ToLower(html[m[2]:m[3]]), attrs: map[string]string{}, start: m[0], end: m[1]}
		for _, a := range reAttr.FindAllStringSubmatch(html[m[4]:m[5]], -1) {
			t.attrs[strings.ToLower(a[1])] = a[2]
		}
		out = append(out, t)
	}
	return out
}

// textAfter returns the text content between an opening tag and its closing tag.
func textAfter(html string, t tag) string {
	rest := html[t.end:]
	i := strings.Index(rest, "</"+t.name)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(reStrip.ReplaceAllString(rest[:i], ""))
}

func a11yProblems(html string) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	ts := tags(html)
	count := map[string]int{}
	ids := map[string]bool{}
	labels := map[string]bool{}
	for _, t := range ts {
		count[t.name]++
		if id, ok := t.attrs["id"]; ok {
			if ids[id] {
				add("duplicate id %q", id)
			}
			ids[id] = true
		}
		if t.name == "label" {
			labels[t.attrs["for"]] = true
		}
	}
	if !regexp.MustCompile(`(?i)^<!doctype html>`).MatchString(html) {
		add("missing doctype")
	}
	for _, t := range ts {
		if t.name == "html" && t.attrs["lang"] == "" {
			add("html has no lang")
		}
	}
	if !strings.Contains(html, `name="viewport"`) {
		add("no viewport meta")
	}
	if m := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(html); m == nil || strings.TrimSpace(m[1]) == "" || strings.HasPrefix(strings.TrimSpace(m[1]), "-") {
		add("empty title")
	}
	if count["h1"] != 1 {
		add("want exactly one h1, got %d", count["h1"])
	}
	last := 0
	for _, m := range reHeading.FindAllStringSubmatch(html, -1) {
		n, _ := strconv.Atoi(m[1])
		if last == 0 && n != 1 {
			add("first heading is h%d", n)
		}
		if last != 0 && n > last+1 {
			add("heading level skips from h%d to h%d", last, n)
		}
		last = n
	}
	if count["main"] != 1 || count["header"] < 1 || count["footer"] < 1 {
		add("landmarks: main=%d header=%d footer=%d", count["main"], count["header"], count["footer"])
	}
	if !ids["main"] || !strings.Contains(html, `href="#main"`) {
		add("no skip link to #main")
	}
	for i, t := range ts {
		switch t.name {
		case "nav":
			if t.attrs["aria-label"] == "" && t.attrs["aria-labelledby"] == "" {
				add("nav without a label")
			}
		case "input", "select", "textarea":
			ty := t.attrs["type"]
			if t.name == "input" && (ty == "hidden" || ty == "submit") {
				continue
			}
			id := t.attrs["id"]
			if id == "" || !labels[id] {
				if t.attrs["aria-label"] == "" && t.attrs["aria-labelledby"] == "" {
					add("%s name=%q has no label", t.name, t.attrs["name"])
				}
			}
			if ty == "password" && t.attrs["autocomplete"] == "" {
				add("password field without autocomplete")
			}
		case "img":
			if _, ok := t.attrs["alt"]; !ok {
				add("img without alt")
			}
		case "button":
			if textAfter(html, t) == "" && t.attrs["aria-label"] == "" {
				add("button without a name")
			}
		case "a":
			if t.attrs["href"] == "" {
				add("anchor without href")
			} else if textAfter(html, t) == "" && t.attrs["aria-label"] == "" {
				add("link %q has no text", t.attrs["href"])
			}
		case "th":
			if t.attrs["scope"] == "" {
				add("th without scope")
			}
		case "table":
			rest := html[t.end:]
			j := strings.Index(rest, "</table>")
			if j < 0 || !strings.Contains(rest[:j], "<caption") {
				add("table without caption")
			}
		case "form":
			if strings.EqualFold(t.attrs["method"], "post") {
				j := strings.Index(html[t.end:], "</form>")
				if j < 0 || !strings.Contains(html[t.end:t.end+j], `name="csrf"`) {
					add("post form %q without csrf field", t.attrs["action"])
				}
			}
		}
		if v, ok := t.attrs["tabindex"]; ok {
			if n, _ := strconv.Atoi(v); n > 0 {
				add("positive tabindex")
			}
		}
		_ = i
	}
	for l := range labels {
		if !ids[l] {
			add("label for=%q points at nothing", l)
		}
	}
	return p
}

func TestPagesPassStaticAccessibilityChecklist(t *testing.T) {
	pages, _ := renderAll(t)
	if len(pages) < 20 {
		t.Fatalf("only %d pages rendered", len(pages))
	}
	for name, html := range pages {
		for _, p := range a11yProblems(html) {
			t.Errorf("%s: %s", name, p)
		}
	}
}

func TestChecklistCatchesProblems(t *testing.T) {
	bad := `<!doctype html><html><head><title>x</title></head><body><h1>a</h1><h3>b</h3><input type="text" name="q"><img src="x"><button></button><table><tr><th>h</th></tr></table><form method="post"></form></body></html>`
	got := strings.Join(a11yProblems(bad), "|")
	for _, want := range []string{"no lang", "skips from h1 to h3", "has no label", "img without alt", "button without a name", "th without scope", "table without caption", "without csrf", "no viewport", "landmarks", "skip link"} {
		if !strings.Contains(got, want) {
			t.Errorf("checker missed %q in %q", want, got)
		}
	}
}

// --- colour contrast of the stylesheet ------------------------------------------------------

func luminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	var c [3]float64
	for i := range c {
		v, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		f := float64(v) / 255
		if f <= 0.03928 {
			c[i] = f / 12.92
		} else {
			c[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func ratio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestStylesheetColoursMeetWCAGAA checks every text/background pair the stylesheet declares for
// both colour schemes. Pairs are declared in the stylesheet itself as `/* contrast: --a on --b */`.
func TestStylesheetColoursMeetWCAGAA(t *testing.T) {
	b, err := os.ReadFile("static/admin.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	pairs := regexp.MustCompile(`/\* contrast: (--[a-z0-9-]+) on (--[a-z0-9-]+) \*/`).FindAllStringSubmatch(css, -1)
	if len(pairs) < 8 {
		t.Fatalf("only %d contrast pairs declared", len(pairs))
	}
	// Split into the light block (:root) and the dark block (inside prefers-color-scheme: dark).
	darkAt := strings.Index(css, "prefers-color-scheme: dark")
	if darkAt < 0 {
		t.Fatal("no dark scheme")
	}
	vars := func(block string) map[string]string {
		m := map[string]string{}
		for _, v := range regexp.MustCompile(`(--[a-z0-9-]+):\s*(#[0-9a-fA-F]{3,6})`).FindAllStringSubmatch(block, -1) {
			m[v[1]] = v[2]
		}
		return m
	}
	light := vars(css[:darkAt])
	dark := vars(css[darkAt:])
	for k, v := range light {
		if _, ok := dark[k]; !ok {
			dark[k] = v
		}
	}
	for scheme, m := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range pairs {
			fg, bg := m[p[1]], m[p[2]]
			if fg == "" || bg == "" {
				t.Errorf("%s: undefined colour in %s on %s", scheme, p[1], p[2])
				continue
			}
			if r := ratio(fg, bg); r < 4.5 {
				t.Errorf("%s: %s (%s) on %s (%s) has contrast %.2f, want >= 4.5", scheme, p[1], fg, p[2], bg, r)
			}
		}
	}
	if !strings.Contains(css, "prefers-reduced-motion") {
		t.Error("stylesheet ignores prefers-reduced-motion")
	}
	if !strings.Contains(css, ":focus-visible") {
		t.Error("no visible focus style")
	}
}

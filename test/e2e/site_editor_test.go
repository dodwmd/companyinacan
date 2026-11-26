//go:build e2e

package e2e

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

var (
	siteInput    = regexp.MustCompile(`<input type="(text|checkbox|hidden)"[^>]*?name="([^"]+)"[^>]*?>`)
	siteValue    = regexp.MustCompile(`value="([^"]*)"`)
	siteTextarea = regexp.MustCompile(`(?s)<textarea [^>]*?name="([^"]+)"[^>]*>\n(.*?)</textarea>`)
	siteDraft    = regexp.MustCompile(`^/site/drafts/[0-9a-f]{24}$`)
	siteRestore  = regexp.MustCompile(`action="(/site/history/[0-9]+/restore)"`)
)

// editorForm is what a browser would send for the website editor's untouched form.
func editorForm(t *testing.T, page string) url.Values {
	t.Helper()
	i := strings.Index(page, `<form method="post" action="/site"`)
	if i < 0 {
		t.Fatalf("no editor form: %.600s", page)
	}
	page = page[i:]
	v := url.Values{}
	for _, m := range siteInput.FindAllStringSubmatch(page, -1) {
		if m[1] == "checkbox" && !strings.Contains(m[0], " checked") {
			continue
		}
		val := ""
		if x := siteValue.FindStringSubmatch(m[0]); x != nil {
			val = html.UnescapeString(x[1])
		}
		v.Add(m[2], val)
	}
	for _, m := range siteTextarea.FindAllStringSubmatch(page, -1) {
		v.Add(m[1], html.UnescapeString(m[2]))
	}
	return v
}

// The website editor through the edge: edit, preview, publish, see it on the public site, roll back.
func TestWebsiteEditorPublishAndRollback(t *testing.T) {
	s := harness.NewSession(env)
	const host = "admin.localhost"
	adminLogin(t, s)
	origin := map[string]string{"Origin": "http://" + host + ":" + env.Port}
	get := func(path string, want int) *harness.Response {
		return s.Do(t, harness.Req{Host: host, Path: path}).Expect(t, "GET "+path, want)
	}
	post := func(path string, form url.Values, want int) *harness.Response {
		return s.Do(t, harness.Req{Method: "POST", Host: host, Path: path, Headers: origin, Body: form}).Expect(t, "POST "+path, want)
	}
	public := func(path string, want int) string {
		return web.Do(t, harness.Req{Host: "localhost", Path: path}).Expect(t, "public "+path, want).String()
	}

	editor := get("/site", 200).String()
	form := editorForm(t, editor)
	before := form.Get("tagline")
	tagline := "Edited in the admin UI " + harness.Unique()
	form.Set("tagline", tagline)
	privacy := ""
	for i := range 12 {
		if p := fmt.Sprintf("pages.%d", i); form.Get(p+".slug") == "privacy" {
			privacy = p
			break
		}
	}
	if privacy == "" {
		t.Fatal("the editor offers no privacy page")
	}
	form.Set(privacy+".body", "We keep your email.\n\n## Questions\n- write to us")

	draft := post("/site", form, 303).Header.Get("Location")
	if !siteDraft.MatchString(draft) {
		t.Fatalf("preview went to %q", draft)
	}
	if strings.Contains(public("/", 200), tagline) {
		t.Fatal("a preview was published")
	}
	frame := get(draft+"/frame", 200)
	if csp := frame.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox;") || !strings.Contains(frame.String(), tagline) || strings.Contains(frame.String(), "<script") {
		t.Fatalf("preview frame: %q %.300s", csp, frame.String())
	}
	csrf := harness.CSRF(get(draft, 200).String())
	post(draft+"/publish", url.Values{"csrf": {csrf}}, 303)

	if home := public("/", 200); !strings.Contains(home, tagline) || !strings.Contains(home, `href="/privacy"`) || !strings.Contains(home, `href="/_forms/contact/form"`) {
		t.Fatalf("the public site does not show the change: %.600s", home)
	}
	if p := public("/privacy", 200); !strings.Contains(p, "<h2>Questions</h2>") {
		t.Fatalf("privacy page: %.600s", p)
	}

	// Roll back to what was there before.
	history := get("/site/history", 200).String()
	restore := siteRestore.FindStringSubmatch(history)
	if restore == nil {
		t.Fatalf("nothing to restore: %.600s", history)
	}
	back := post(restore[1], url.Values{"csrf": {csrf}}, 303).Header.Get("Location")
	post(back+"/publish", url.Values{"csrf": {csrf}}, 303)
	if home := public("/", 200); strings.Contains(home, tagline) || strings.Contains(home, `href="/privacy"`) {
		t.Fatalf("not rolled back: %.600s", home)
	}
	public("/privacy", 404)
	if got := editorForm(t, get("/site", 200).String()).Get("tagline"); got != before {
		t.Fatalf("tagline %q, want %q", got, before)
	}
	if audit := get("/audit?action=site.", 200).String(); !strings.Contains(audit, "site.publish") {
		t.Error("publishing is not in the audit log")
	}
}

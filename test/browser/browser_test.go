//go:build browser

package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/audits"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

//go:embed checks.js
var checksJS string

// viewport is a device the whole suite runs on.
type viewport struct {
	Name          string
	Width, Height int64
	Scale         float64
	Phone         bool
	UserAgent     string
}

var viewports = []viewport{
	{Name: "desktop", Width: 1366, Height: 900, Scale: 1},
	// 360 CSS pixels is the narrowest common Android width.
	{Name: "android", Width: 360, Height: 780, Scale: 3, Phone: true,
		UserAgent: "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36"},
}

var chromeCtx context.Context

// startChrome starts one headless Chrome for the run. E2E_CHROME names the binary (default: the
// first Chrome or Chromium chromedp finds). The sandbox stays on unless E2E_CHROME_NO_SANDBOX=1.
func startChrome() (func(), error) {
	opts := append(slices.Clone(chromedp.DefaultExecAllocatorOptions[:]),
		chromedp.Flag("lang", "en-US"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1366, 900),
	)
	if p := os.Getenv("E2E_CHROME"); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	if os.Getenv("E2E_CHROME_NO_SANDBOX") == "1" {
		opts = append(opts, chromedp.NoSandbox)
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(alloc)
	stop := func() { cancel(); cancelAlloc() }
	var product string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		_, product, _, _, _, err = cdpbrowser.GetVersion().Do(ctx)
		return err
	}))
	if err != nil {
		stop()
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "browser: %s\n", product)
	chromeCtx = ctx
	return stop, nil
}

// session is one browser profile (its own cookie jar) with one tab, watched for everything a
// careful person would see in the developer tools.
type session struct {
	t   *testing.T
	vp  viewport
	ctx context.Context

	mu        sync.Mutex
	problems  []string
	status    int64 // of the last main document
	allowed   []*regexp.Regexp
	downloads map[string]string // guid -> suggested name
	done      chan string       // guid of a finished download
	dir       string
}

// newSession opens a fresh profile on the viewport; it is closed when the test ends.
func newSession(t *testing.T, vp viewport) *session {
	t.Helper()
	// A profile and a window of its own (chromedp's own new browser context asks for a tab, which
	// current headless Chrome refuses without a window).
	root := cdpExecutorBrowser(chromeCtx)
	profile, err := target.CreateBrowserContext().Do(root)
	if err != nil {
		t.Fatalf("cannot open a browser profile: %v", err)
	}
	tab, err := target.CreateTarget("about:blank").WithBrowserContextID(profile).WithNewWindow(true).Do(root)
	if err != nil {
		t.Fatalf("cannot open a browser window: %v", err)
	}
	ctx, cancel := chromedp.NewContext(chromeCtx, chromedp.WithTargetID(tab))
	t.Cleanup(func() {
		cancel()
		_ = target.DisposeBrowserContext(profile).Do(root)
	})
	s := &session{t: t, vp: vp, ctx: ctx, downloads: map[string]string{}, done: make(chan string, 16), dir: t.TempDir()}
	chromedp.ListenTarget(ctx, s.listen)
	actions := []chromedp.Action{
		network.Enable(), runtime.Enable(), cdplog.Enable(), page.Enable(), audits.Enable(),
		emulation.SetDeviceMetricsOverride(vp.Width, vp.Height, vp.Scale, vp.Phone),
	}
	if vp.Phone {
		actions = append(actions, emulation.SetTouchEmulationEnabled(true), emulation.SetUserAgentOverride(vp.UserAgent))
	}
	// The first Run must not carry a deadline: it would bound the life of the tab.
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatalf("cannot open a browser tab: %v", err)
	}
	if err := cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName).
		WithBrowserContextID(profile).WithDownloadPath(s.dir).WithEventsEnabled(true).Do(root); err != nil {
		t.Fatalf("cannot allow downloads: %v", err)
	}
	chromedp.ListenBrowser(ctx, s.listenBrowser)
	return s
}

// in moves the session's reporting to a subtest.
func (s *session) in(t *testing.T) *session {
	prev := s.t
	s.t = t
	t.Cleanup(func() { s.t = prev })
	s.drain()
	return s
}

func (s *session) note(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

func (s *session) listen(ev any) {
	switch e := ev.(type) {
	case *runtime.EventConsoleAPICalled:
		if e.Type == runtime.APITypeError || e.Type == runtime.APITypeWarning || e.Type == runtime.APITypeAssert {
			var parts []string
			for _, a := range e.Args {
				parts = append(parts, string(a.Value)+a.Description)
			}
			s.note("console %s: %s", e.Type, strings.Join(parts, " "))
		}
	case *runtime.EventExceptionThrown:
		s.note("uncaught exception: %s %s", e.ExceptionDetails.Text, e.ExceptionDetails.URL)
	case *cdplog.EventEntryAdded:
		if e.Entry.Level == cdplog.LevelError || e.Entry.Level == cdplog.LevelWarning {
			s.note("console %s (%s): %s %s", e.Entry.Level, e.Entry.Source, e.Entry.Text, e.Entry.URL)
		}
	case *audits.EventIssueAdded:
		b, _ := json.Marshal(e.Issue.Details)
		// Chrome's advice that a field is named like an autocomplete token ("name", "title"): a
		// naming heuristic, not something the person using the page meets.
		if strings.Contains(string(b), "FormInputAssignedAutocompleteValueToIdOrNameAttributeError") {
			return
		}
		s.note("browser issue %s: %s", e.Issue.Code, b)
	case *network.EventResponseReceived:
		if e.Type == network.ResourceTypeDocument {
			s.mu.Lock()
			s.status = e.Response.Status
			s.mu.Unlock()
		}
	case *page.EventJavascriptDialogOpening:
		s.note("unexpected dialog: %s", e.Message)
	}
}

func (s *session) listenBrowser(ev any) {
	switch e := ev.(type) {
	case *cdpbrowser.EventDownloadWillBegin:
		s.mu.Lock()
		s.downloads[e.GUID] = e.SuggestedFilename
		s.mu.Unlock()
	case *cdpbrowser.EventDownloadProgress:
		// The events are browser wide: a download is this profile's when its file landed here.
		if e.State == cdpbrowser.DownloadProgressStateCompleted {
			if _, err := os.Stat(filepath.Join(s.dir, e.GUID)); err == nil {
				select {
				case s.done <- e.GUID:
				default:
				}
			}
		}
	}
}

func (s *session) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.problems
	s.problems = nil
	return p
}

// allow declares that a message matching re is expected from here on (a deliberate 401, say).
func (s *session) allow(re string) { s.allowed = append(s.allowed, regexp.MustCompile(re)) }

func (s *session) run(actions ...chromedp.Action) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Run(ctx, actions...); err != nil {
		s.screenshot()
		s.t.Fatalf("browser action failed at %s: %v\npage text: %.600s\nreported: %s", s.url(), err, redact(s.text()), strings.Join(s.drain(), "\n"))
	}
}

// navigate runs actions that load a new document and waits for it.
func (s *session) navigate(actions ...chromedp.Action) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if _, err := chromedp.RunResponse(ctx, actions...); err != nil {
		s.screenshot()
		s.t.Fatalf("navigation failed from %s: %v\npage text: %.600s\nreported: %s", s.url(), err, redact(s.text()), strings.Join(s.drain(), "\n"))
	}
}

// goTo opens an address and checks the page it shows.
func (s *session) goTo(address string) {
	s.t.Helper()
	s.navigate(chromedp.Navigate(address))
	s.check()
}

// click follows a link or presses a button that loads a new page, then checks that page.
func (s *session) click(sel string) {
	s.t.Helper()
	s.navigate(chromedp.Click(sel, chromedp.ByQuery))
	s.check()
}

// submit presses the submit button of the form with this action.
func (s *session) submit(action string) {
	s.t.Helper()
	s.click(`form[action="` + action + `"] button:not([type="button"])`)
}

// fill types into a field as a person would (select, date and time fields are set directly: what
// is typed into them depends on the locale of the machine).
func (s *session) fill(sel, value string) {
	s.t.Helper()
	var kind string
	s.run(chromedp.Evaluate(fmt.Sprintf(`(()=>{const e=document.querySelector(%q); if(!e) return ""; return e.tagName==="SELECT"?"select":(e.type||"text")})()`, sel), &kind))
	set := func(v string) {
		var got string
		s.run(chromedp.Evaluate(fmt.Sprintf(`(()=>{const e=document.querySelector(%q); e.value=%q; return e.value})()`, sel, v), &got))
		if got != v {
			s.t.Fatalf("%s does not take the value %q (it holds %q)", sel, v, got)
		}
	}
	switch kind {
	case "":
		s.screenshot()
		s.t.Fatalf("no field %s on %s: %.600s", sel, s.url(), s.text())
	case "select", "select-one", "date", "time":
		set(value)
	default:
		set("")
		s.run(chromedp.SendKeys(sel, value, chromedp.ByQuery))
	}
}

func (s *session) url() string {
	var u string
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.Location(&u))
	return redact(u)
}

var secretInURL = regexp.MustCompile(`(/public/sign/|/s/|[?&]t=)[A-Za-z0-9_-]{16,}`)

// redact keeps single-use tokens of the throwaway stack out of CI logs all the same.
func redact(s string) string {
	return personalToken.ReplaceAllString(secretInURL.ReplaceAllString(s, "${1}..."), "canpat_...")
}

var personalToken = regexp.MustCompile(`canpat_[A-Za-z0-9_-]+`)

func (s *session) path() string {
	u, err := url.Parse(s.rawURL())
	if err != nil {
		return ""
	}
	return u.Path
}

func (s *session) rawURL() string {
	var u string
	s.run(chromedp.Location(&u))
	return u
}

func (s *session) text() string {
	var v string
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &v))
	return v
}

// has fails the test unless the visible text of the page contains every want.
func (s *session) has(want ...string) {
	s.t.Helper()
	got := s.text()
	for _, w := range want {
		if !strings.Contains(got, w) {
			s.screenshot()
			s.t.Fatalf("%s does not show %q (HTTP %d): %.900s", s.url(), w, s.lastStatus(), got)
		}
	}
}

func (s *session) lastStatus() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *session) exists(sel string) bool {
	s.t.Helper()
	var ok bool
	s.run(chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, sel), &ok))
	return ok
}

// check reports everything wrong with the page now shown: error status, console and CSP messages,
// and, for HTML, the layout and accessibility checks in both colour schemes.
func (s *session) check() {
	s.t.Helper()
	time.Sleep(150 * time.Millisecond) // let late console and issue events arrive
	var kind string
	s.run(chromedp.Evaluate(`document.contentType`, &kind))
	var found []string
	if st := s.lastStatus(); st >= 400 {
		found = append(found, fmt.Sprintf("the page answered HTTP %d", st))
	}
	if kind == "text/html" {
		for _, scheme := range []string{"light", "dark"} {
			var list []string
			s.run(
				emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}}),
				chromedp.Evaluate(fmt.Sprintf(`%s({width:%d,touch:%v})`, strings.TrimSuffix(strings.TrimSpace(checksJS), ";"), s.vp.Width, s.vp.Phone), &list),
			)
			for _, p := range list {
				if !slices.Contains(found, p) {
					found = append(found, p)
				}
			}
		}
	}
	for _, p := range s.drain() {
		// A text file shown as it is has no place to say it has no icon.
		if kind != "text/html" && strings.Contains(p, "/favicon.ico") {
			continue
		}
		found = append(found, p)
	}
	u := s.url()
	bad := false
	for _, p := range found {
		p = redact(p)
		if slices.ContainsFunc(s.allowed, func(re *regexp.Regexp) bool { return re.MatchString(p) }) {
			continue
		}
		bad = true
		s.t.Errorf("[%s] %s: %s", s.vp.Name, u, p)
	}
	if bad {
		s.screenshot()
	}
}

// screenshot saves the page to E2E_ARTIFACTS (the CI job uploads them when it fails).
func (s *session) screenshot() {
	dir := os.Getenv("E2E_ARTIFACTS")
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	var png []byte
	var u string
	if err := chromedp.Run(ctx, chromedp.Location(&u), chromedp.FullScreenshot(&png, 100)); err != nil {
		return
	}
	// The QR code or secret pages of enrolment are never visited, and the stack is thrown away.
	name := regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(redact(strings.TrimPrefix(u, "http://")), "-")
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, s.vp.Name+"-"+strings.Trim(name, "-")+".png"), png, 0o600)
}

// download clicks something that saves a file and returns its suggested name and contents.
func (s *session) download(sel string) (string, []byte) {
	s.t.Helper()
	s.drain()
	s.run(chromedp.Click(sel, chromedp.ByQuery))
	select {
	case guid := <-s.done:
		b, err := os.ReadFile(filepath.Join(s.dir, guid)) //nolint:gosec // our own temporary directory
		if err != nil {
			s.t.Fatalf("download: %v", err)
		}
		s.mu.Lock()
		name := s.downloads[guid]
		s.mu.Unlock()
		for _, p := range s.drain() {
			// A navigation that turns into a download is reported by Chrome as an aborted frame; not a fault.
			if !slices.ContainsFunc(s.allowed, func(re *regexp.Regexp) bool { return re.MatchString(p) }) {
				s.t.Errorf("[%s] while downloading %s: %s", s.vp.Name, name, redact(p))
			}
		}
		return name, b
	case <-time.After(15 * time.Second):
		s.screenshot()
		s.t.Fatalf("no download started or finished after clicking %s on %s: %s", sel, s.url(), strings.Join(s.drain(), "\n"))
		return "", nil
	}
}

// waitPath waits for a navigation the page makes by itself (a meta refresh).
func (s *session) waitPath(path string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var u, ready string
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		err := chromedp.Run(ctx, chromedp.Location(&u), chromedp.Evaluate(`document.readyState`, &ready))
		cancel()
		if p, perr := url.Parse(u); err == nil && perr == nil && p.Path == path && ready == "complete" {
			return
		}
		if time.Now().After(deadline) {
			s.screenshot()
			s.t.Fatalf("the browser did not arrive at %s; it is at %s: %.600s %s", path, redact(u), s.text(), strings.Join(s.drain(), "\n"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// links returns the same-origin links of the current page.
func (s *session) links() []string {
	s.t.Helper()
	var out []string
	s.run(chromedp.Evaluate(`[...document.querySelectorAll('a[href]')]
		.filter(a => a.origin === location.origin && !a.hasAttribute('download'))
		.map(a => a.origin + a.pathname + a.search)`, &out))
	return out
}

var idLike = regexp.MustCompile(`/([0-9]+|[A-Za-z0-9_-]{16,}|[A-Z][A-Z0-9]*-[0-9]+|[A-Z][A-Z0-9]{2,9})(/|$)`)

// crawl visits the pages reachable by links from start, one of each shape (ids aside), and checks
// each. Links that download or leave are skipped.
func (s *session) crawl(start string, limit int, skip *regexp.Regexp) {
	s.t.Helper()
	shape := func(u string) string {
		p, _ := url.Parse(u)
		x := p.Path
		for i := 0; i < 3; i++ {
			x = idLike.ReplaceAllString(x, "/:id$2")
		}
		var keys []string
		for k := range p.Query() {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return x + "?" + strings.Join(keys, "&")
	}
	seen := map[string]bool{}
	queue := []string{start}
	visited := 0
	for len(queue) > 0 && visited < limit {
		u := queue[0]
		queue = queue[1:]
		if seen[shape(u)] || (skip != nil && skip.MatchString(u)) {
			continue
		}
		seen[shape(u)] = true
		visited++
		s.goTo(u)
		queue = append(queue, s.links()...)
	}
	s.t.Logf("crawled %d pages from %s", visited, start)
}

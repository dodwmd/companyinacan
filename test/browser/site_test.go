//go:build browser

package browser

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	cdppage "github.com/chromedp/cdproto/page"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

const frameScript = `document.body.innerText`

// frameText reads the text of the preview frame through the DevTools protocol: no page script can,
// the frame being an opaque origin. Chrome may give a sandboxed frame a process of its own, which
// makes it a target of its own; otherwise it is a child frame of the page.
func frameText(s *session) string {
	s.t.Helper()
	var text string
	s.run(chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := cdppage.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		for _, f := range tree.ChildFrames {
			world, err := cdppage.CreateIsolatedWorld(f.Frame.ID).Do(ctx)
			if err != nil {
				return nil // navigating
			}
			res, _, err := runtime.Evaluate(frameScript).WithContextID(world).WithReturnByValue(true).Do(ctx)
			if err == nil {
				text = string(res.Value)
			}
		}
		return nil
	}))
	if text != "" {
		return text
	}
	targets, err := chromedp.Targets(s.ctx)
	if err != nil {
		s.t.Fatalf("targets: %v", err)
	}
	for _, info := range targets {
		if info.Type != "iframe" || !strings.Contains(info.URL, "/frame") {
			continue
		}
		// Not cancelled here: that would close the frame. It ends with the session.
		ctx, _ := chromedp.NewContext(s.ctx, chromedp.WithTargetID(info.TargetID)) //nolint:govet // see above
		ctx, stop := context.WithTimeout(ctx, 5*time.Second)
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(frameScript, &got)); err == nil {
			text += got
		}
		stop()
	}
	return text
}

// waitFrame waits until the preview frame shows want.
func waitFrame(s *session, want string) {
	s.t.Helper()
	var got string
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if got = frameText(s); strings.Contains(got, want) {
			return
		}
	}
	s.screenshot()
	s.t.Fatalf("the preview frame does not show %q: %.600s %s", want, got, strings.Join(s.drain(), "\n"))
}

// websiteEditor: edit the site, look at the preview in its sandboxed frame, publish, visit the
// public pages, roll back. The session cookie is SameSite=Strict and the frame is sandboxed, which
// only a real browser can show to work together.
func websiteEditor(t *testing.T, r *run) {
	s := r.admin.in(t)
	var mu sync.Mutex
	frames := map[string]int64{}
	listening, stop := context.WithCancel(s.ctx)
	defer stop()
	chromedp.ListenTarget(listening, func(ev any) {
		if e, ok := ev.(*network.EventResponseReceived); ok && strings.Contains(e.Response.URL, "/frame") {
			mu.Lock()
			frames[e.Response.URL] = e.Response.Status
			mu.Unlock()
		}
	})
	public := func(path string) string { return "http://localhost:" + env.Port + path }

	s.goTo(adminURL() + "/site")
	s.has("Website", "Theme", "Extra pages")
	headline := "Browser edit " + r.u
	// A refused value is reported next to its field, and nothing is lost.
	s.allow(`HTTP 400|status of 400`)
	s.fill("#f-hero-headline", headline)
	s.fill("#f-brand-colours-0", "orange!")
	s.submit("/site")
	s.allowed = s.allowed[:len(s.allowed)-1]
	s.has("Please fix these before previewing", "Use a colour such as #2457c5.")
	if !s.exists(`#f-brand-colours-0[aria-invalid="true"]`) {
		t.Error("the field with the error is not marked")
	}
	s.fill("#f-brand-colours-0", "#b4440e")
	s.fill("#f-pages-0-body", "We keep your email for "+r.u+".")
	s.submit("/site")
	draft := s.path()
	if !strings.HasPrefix(draft, "/site/drafts/") {
		t.Fatalf("no preview: %.600s", s.text())
	}
	if !s.exists(`iframe[sandbox=""][name="preview"]`) {
		t.Fatal("no sandboxed preview frame")
	}
	waitFrame(s, headline)
	s.run(chromedp.Click(`a[target="preview"][href$="?page=privacy"]`, chromedp.ByQuery))
	waitFrame(s, "We keep your email for "+r.u)
	mu.Lock()
	for u, status := range frames {
		if status != 200 {
			t.Errorf("the preview frame %s answered %d", u, status)
		}
	}
	// A frame in a process of its own reports its later loads to itself; its text above showed them.
	if len(frames) < 1 {
		t.Errorf("frame responses seen: %v", frames)
	}
	mu.Unlock()
	s.check()

	publish := func(draft string) {
		if !s.exists(`form[action="` + draft + `/publish"]`) {
			t.Fatalf("no publish button (a step-up is not expected right after signing in): %.600s", s.text())
		}
		s.submit(draft + "/publish")
		s.has("Published.")
	}
	publish(draft)

	s.goTo(public("/"))
	if !s.exists(`footer a[href="/privacy"]`) {
		t.Fatal("the public home page does not link to the privacy page")
	}
	s.click(`footer a[href="/privacy"]`)
	s.has("Privacy policy", "We keep your email for "+r.u)

	s.goTo(adminURL() + "/site/history")
	s.has("Website history")
	var restore string
	s.run(chromedp.Evaluate(`document.querySelector('form[action$="/restore"]').getAttribute('action')`, &restore))
	s.submit(restore)
	publish(s.path())
	s.goTo(public("/"))
	if s.exists(`footer a[href="/privacy"]`) {
		t.Error("the rollback left the privacy page linked")
	}
	web.Do(t, harness.Req{Host: "localhost", Path: "/privacy"}).Expect(t, "the privacy page is gone", 404)
}

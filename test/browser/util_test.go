//go:build browser

package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// cdpExecutorBrowser makes a DevTools command go to the browser instead of the tab.
func cdpExecutorBrowser(ctx context.Context) context.Context {
	return cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
}

// downloadAddress opens an address that answers with a file.
func downloadAddress(s *session, address string) (string, []byte) {
	s.t.Helper()
	s.run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(address).Do(ctx)
		return err
	}))
	end := time.After(15 * time.Second)
	for {
		select {
		case guid := <-s.done:
			b, err := os.ReadFile(filepath.Join(s.dir, guid)) //nolint:gosec // our own temporary directory
			if err != nil {
				continue // another session's download
			}
			s.drain()
			return guid, b
		case <-end:
			s.screenshot()
			s.t.Fatalf("no download from %s: %.400s %s", redact(address), s.text(), strings.Join(s.drain(), "\n"))
			return "", nil
		}
	}
}

// projectKey derives a work project key (upper-case letters) from the run's suffix.
func projectKey(u string) string {
	return "B" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r + 17 // digits become letters
		}
		return r
	}, u[len(u)-5:]))
}

// Package browser drives the docker compose stack with a real headless Chrome, at a desktop and
// an Android phone viewport (specs/054-browser-e2e). It complements test/e2e, which speaks HTTP:
// here cookies, SameSite, Origin and Sec-Fetch-Site, CSP, forms, uploads, downloads and layout are
// the browser's, not a test client's.
//
// Everything is behind the `browser` build tag, so chromedp is a test-only dependency: no
// production binary imports it and `go test ./...` needs no browser. Run it with `make e2e-up`
// and then `make e2e-browser`.
package browser

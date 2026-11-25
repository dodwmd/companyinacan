package adminui

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io/fs"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/notify/webpush"
)

// Static, Lighthouse-style checks of the PWA surface (specs/025-pwa-webpush): installability
// metadata, mobile basics, caching, and the shape of the service worker.

// uiPushOK accepts every push; no push service is ever contacted.
type uiPushOK struct{ sent int }

func (u *uiPushOK) Send(context.Context, webpush.Subscription, []byte, webpush.Options) (webpush.Result, error) {
	u.sent++
	return webpush.Result{Status: 201}, nil
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(assets, "static/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cssVar(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`--` + name + `:\s*(#[0-9a-fA-F]{6});`).FindStringSubmatch(readStatic(t, "admin.css"))
	if m == nil {
		t.Fatalf("css variable --%s not found", name)
	}
	return strings.ToLower(m[1])
}

// darkBg is --bg inside the prefers-color-scheme: dark block.
func darkBg(t *testing.T) string {
	t.Helper()
	css := readStatic(t, "admin.css")
	i := strings.Index(css, "prefers-color-scheme: dark")
	m := regexp.MustCompile(`--bg:\s*(#[0-9a-fA-F]{6});`).FindStringSubmatch(css[i:])
	if i < 0 || m == nil {
		t.Fatal("dark --bg not found")
	}
	return strings.ToLower(m[1])
}

type manifestDoc struct {
	Name, ShortName, ID, StartURL, Scope, Display, ThemeColor, BackgroundColor string
	Screenshots                                                                any
	Icons                                                                      []struct {
		Src, Sizes, Type, Purpose string
	}
	Shortcuts []struct {
		Name, URL string
		Icons     []struct{ Src string }
	}
}

func loadManifest(t *testing.T) manifestDoc {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(readStatic(t, "manifest.webmanifest")), &raw); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	var m manifestDoc
	b, _ := json.Marshal(raw)
	// field names are snake_case in the file
	b = regexp.MustCompile(`"(short_name|start_url|theme_color|background_color)"`).ReplaceAllFunc(b, func(s []byte) []byte {
		parts := strings.Split(strings.Trim(string(s), `"`), "_")
		for i := range parts {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
		return []byte(`"` + strings.Join(parts, "") + `"`)
	})
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.Screenshots = raw["screenshots"]
	return m
}

func TestManifestInstallabilityAndColours(t *testing.T) {
	m := loadManifest(t)
	for k, v := range map[string]string{"name": m.Name, "short_name": m.ShortName, "id": m.ID, "start_url": m.StartURL, "scope": m.Scope} {
		if v == "" {
			t.Errorf("manifest lacks %s", k)
		}
	}
	if m.Display != "standalone" || m.Scope != "/" || m.StartURL != "/" || m.ID != "/" {
		t.Errorf("display %q scope %q start %q id %q", m.Display, m.Scope, m.StartURL, m.ID)
	}
	if len(m.ShortName) > 12 {
		t.Errorf("short_name %q is longer than the 12 characters home screens show", m.ShortName)
	}
	if m.ThemeColor != cssVar(t, "accent") || m.BackgroundColor != cssVar(t, "bg") {
		t.Errorf("manifest colours %s/%s differ from the stylesheet's --accent %s / --bg %s", m.ThemeColor, m.BackgroundColor, cssVar(t, "accent"), cssVar(t, "bg"))
	}
	if m.Screenshots != nil {
		t.Error("screenshots are deliberately omitted")
	}
	have := map[string]bool{}
	for _, ic := range m.Icons {
		if !strings.HasPrefix(ic.Src, "/static/") {
			t.Errorf("icon %s is outside /static/", ic.Src)
			continue
		}
		b, err := fs.ReadFile(assets, strings.TrimPrefix(ic.Src, "/"))
		if err != nil {
			t.Errorf("icon %s is not embedded: %v", ic.Src, err)
			continue
		}
		if ic.Type == "image/png" {
			cfg, err := png.DecodeConfig(bytes.NewReader(b))
			if err != nil {
				t.Errorf("%s: %v", ic.Src, err)
				continue
			}
			if want := strconv.Itoa(cfg.Width) + "x" + strconv.Itoa(cfg.Height); ic.Sizes != want {
				t.Errorf("%s declares %s but is %s", ic.Src, ic.Sizes, want)
			}
		}
		if strings.Contains(ic.Purpose, " ") {
			t.Errorf("%s: purpose %q combines any and maskable; ship separate icons", ic.Src, ic.Purpose)
		}
		have[ic.Purpose+" "+ic.Sizes] = true
	}
	for _, want := range []string{"any 192x192", "any 512x512", "maskable 192x192", "maskable 512x512"} {
		if !have[want] {
			t.Errorf("manifest lacks icon %q", want)
		}
	}
	if len(m.Shortcuts) < 2 {
		t.Fatalf("shortcuts = %d", len(m.Shortcuts))
	}
	served := map[string]bool{}
	for _, r := range Routes() {
		served[strings.TrimPrefix(r, "GET ")] = true
	}
	for _, sc := range m.Shortcuts {
		if sc.Name == "" || !strings.HasPrefix(sc.URL, "/") || !served[sc.URL] {
			t.Errorf("shortcut %+v is not a served in-scope path", sc)
		}
	}
}

func TestPagesCarryMobileAndPWAHeadTags(t *testing.T) {
	p := newPUI(t)
	s := p.login()
	for _, path := range []string{"/login", "/", "/approvals", "/notifications", "/catalogue"} {
		var body string
		if path == "/login" {
			body = p.do("GET", path, nil).body
		} else {
			body = p.do("GET", path, nil, s.with()...).body
		}
		for _, want := range []string{
			`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
			`<link rel="manifest" href="/manifest.webmanifest">`,
			`<meta name="theme-color" content="` + cssVar(t, "accent") + `">`,
			`<meta name="theme-color" media="(prefers-color-scheme: dark)" content="` + darkBg(t) + `">`,
			`<meta name="color-scheme" content="light dark">`,
			`<html lang="en">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %s", path, want)
			}
		}
	}
}

func TestStylesheetIsMobileReady(t *testing.T) {
	css := readStatic(t, "admin.css")
	for _, want := range []string{
		"env(safe-area-inset-left)", "env(safe-area-inset-bottom)", "prefers-color-scheme: dark", "@media (pointer: coarse)", "table.rows",
		"max-width: 40rem", "min-height: 2.75rem",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("admin.css lacks %q", want)
		}
	}
	// Touch targets of 44 CSS pixels (2.75rem) for coarse pointers, including the compact table buttons.
	coarse := css[strings.Index(css, "@media (pointer: coarse)"):]
	if !strings.Contains(coarse, "min-height: 2.75rem") {
		t.Error("coarse-pointer rules do not enforce 44px targets")
	}
}

func TestStaticAssetsAreHashedAndImmutable(t *testing.T) {
	p := newPUI(t)
	s := p.login()
	page := p.do("GET", "/notifications", nil, s.with()...).body
	m := regexp.MustCompile(`href="(/static/admin\.css\?v=([0-9a-f]{8,}))"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("stylesheet link is not content-hashed:\n%s", page)
	}
	if r := p.do("GET", m[1], nil); r.StatusCode != 200 || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset: %d %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
	for _, path := range []string{"/static/admin.css", "/static/admin.css?v=deadbeef00"} {
		if cc := p.do("GET", path, nil).Header.Get("Cache-Control"); strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=3600") {
			t.Errorf("%s: unversioned or wrong-hash asset must not be immutable: %q", path, cc)
		}
	}
	if !strings.Contains(page, `src="/static/push.js?v=`) {
		t.Error("push.js is not content-hashed")
	}
}

func TestServiceWorkerOnlyHandlesPushAndNotificationClick(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/sw.js", nil) // public: the browser fetches it with or without a session
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	if r.Header.Get("Service-Worker-Allowed") != "/" {
		t.Errorf("Service-Worker-Allowed = %q", r.Header.Get("Service-Worker-Allowed"))
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q; the worker script must revalidate", cc)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "connect-src") {
		t.Errorf("the worker's own CSP must allow no network: %q", csp)
	}
	body := r.body
	listeners := regexp.MustCompile(`addEventListener\("([a-z]+)"`).FindAllStringSubmatch(body, -1)
	var got []string
	for _, l := range listeners {
		got = append(got, l[1])
	}
	if strings.Join(got, ",") != "push,notificationclick" {
		t.Fatalf("listeners = %v, want exactly push and notificationclick", got)
	}
	// No request interception and no cache of any kind: nothing authenticated can be stored.
	for _, bad := range []string{"fetch", "caches", "Cache", "importScripts", "eval(", "new Function", "XMLHttpRequest", "indexedDB", "localStorage", "skipWaiting", "sync"} {
		if strings.Contains(body, bad) {
			t.Errorf("sw.js mentions %q", bad)
		}
	}
	if !strings.Contains(body, "self.location.origin") || !strings.Contains(body, "showNotification") {
		t.Error("sw.js must keep notification links same-origin and always show a notification")
	}
}

var scriptTagRe = regexp.MustCompile(`(?is)<script\b([^>]*)>`)

func TestOnlyTheNotificationsPageLoadsAScript(t *testing.T) {
	p := newPUI(t)
	s := p.login()
	for _, path := range []string{"/", "/settings", "/approvals", "/modules", "/audit", "/events", "/catalogue", "/tokens", "/access", "/nope"} {
		r := p.do("GET", path, nil, s.with()...)
		csp := r.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'none'") || strings.Contains(csp, "worker-src") || strings.Contains(csp, "connect-src") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if scriptTagRe.MatchString(r.body) {
			t.Errorf("%s contains a script element", path)
		}
	}
	r := p.do("GET", "/notifications", nil, s.with()...)
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "worker-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("notifications CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") || strings.Contains(csp, "nonce") || strings.Contains(csp, "*") || strings.Contains(csp, "data:") {
		t.Errorf("notifications CSP is looser than needed: %q", csp)
	}
	tags := scriptTagRe.FindAllStringSubmatch(r.body, -1)
	if len(tags) != 1 || !strings.Contains(tags[0][1], `src="/static/push.js?v=`) || !strings.Contains(tags[0][1], "defer") {
		t.Fatalf("script tags = %v, want one external deferred push.js", tags)
	}
	if regexp.MustCompile(`(?i)\son[a-z]+=|javascript:| style=`).MatchString(r.body) {
		t.Error("inline event handler, javascript: URL or style attribute")
	}
	// The script is served as JavaScript and is small and free of dangerous sinks.
	js := p.do("GET", "/static/push.js", nil)
	if js.StatusCode != 200 || !strings.HasPrefix(js.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("%d %v", js.StatusCode, js.Header)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "eval(", "document.write", "new Function", "localStorage", "sessionStorage", "postMessage"} {
		if strings.Contains(js.body, bad) {
			t.Errorf("push.js uses %q", bad)
		}
	}
	if !strings.Contains(js.body, "X-CSRF-Token") || !strings.Contains(js.body, `"/sw.js"`) || !strings.Contains(js.body, "userVisibleOnly: true") {
		t.Error("push.js must send the CSRF header, register /sw.js and subscribe user-visible only")
	}
	if len(js.body) > 6000 {
		t.Errorf("push.js is %d bytes; keep it small", len(js.body))
	}
}

func TestPushPageAndMobileTablesPassTheAccessibilityChecklist(t *testing.T) {
	p := newPUI(t)
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	sub := webpush.Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/x", P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
	if _, err := p.ntf.Subscribe(context.Background(), "user:root", sub, "Chrome on Android"); err != nil {
		t.Fatal(err)
	}
	p.request("user:dave", "Laptop")
	s := p.login()
	cell := regexp.MustCompile(`<td>`)
	for _, path := range []string{"/notifications", "/approvals", "/catalogue"} {
		body := p.do("GET", path, nil, s.with()...).body
		for _, prob := range a11yProblems(body) {
			t.Errorf("%s: %s", path, prob)
		}
		// Every cell of a stacked-row table carries the label the phone layout prints before it,
		// except the action cells (a form with its own button).
		for _, tb := range regexp.MustCompile(`(?s)<table class="rows">.*?</table>`).FindAllString(body, -1) {
			for _, c := range cell.FindAllStringIndex(tb, -1) {
				if !strings.HasPrefix(tb[c[1]:], "<form") && !strings.HasPrefix(tb[c[1]:], "</td>") {
					t.Errorf("%s: a cell of a stacked table has no data-label near %.60q", path, tb[c[1]:])
				}
			}
		}
	}
}

func TestPushSectionWorksWithoutJavaScript(t *testing.T) {
	p := newPUI(t)
	s := p.login()
	// The page explains and offers the JS-only button as hidden; nothing else needs script.
	page := p.do("GET", "/notifications", nil, s.with()...).body
	for _, want := range []string{`id="push"`, `data-csrf="` + s.csrf + `"`, `id="push-enable"`, `id="push-disable"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if !regexp.MustCompile(`id="push-enable"[^>]*\shidden`).MatchString(page) && !regexp.MustCompile(`\shidden[^>]*id="push-enable"`).MatchString(page) {
		t.Error("the JS-only button must be hidden until the script confirms support")
	}
	if strings.Contains(page, `action="/notifications/push/test"`) {
		t.Error("the test button is offered with no device registered")
	}
	// Register a device through the same service the API uses, then the server-rendered controls appear.
	ctx := context.Background()
	devs := []string{"https://fcm.googleapis.com/fcm/send/capability-one", "https://updates.push.services.mozilla.com/wpush/v2/capability-two"}
	for _, ep := range devs {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		if _, err := p.ntf.Subscribe(ctx, "user:root", webpush.Subscription{Endpoint: ep, P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}, "Chrome on Android"); err != nil {
			t.Fatal(err)
		}
	}
	page = p.do("GET", "/notifications", nil, s.with()...).body
	if !strings.Contains(page, "Chrome on Android") || !strings.Contains(page, "fcm.googleapis.com") || !strings.Contains(page, `action="/notifications/push/test"`) {
		t.Fatalf("devices not rendered:\n%s", page)
	}
	if strings.Contains(page, "capability-one") || strings.Contains(page, "capability-two") {
		t.Fatal("the push endpoint capability is shown on the page")
	}
	idm := regexp.MustCompile(`action="/notifications/push/(\d+)/remove"`).FindStringSubmatch(page)
	if idm == nil {
		t.Fatal("no remove form")
	}
	// Test button: CSRF required, then one notification per device, then rate limited.
	if r := p.do("POST", "/notifications/push/test", url.Values{}, s.with()...); r.StatusCode != 403 {
		t.Fatalf("test without CSRF = %d", r.StatusCode)
	}
	r := p.do("POST", "/notifications/push/test", url.Values{"csrf": {s.csrf}}, s.with()...)
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "flash.push_test_sent") || p.push.sent != 2 {
		t.Fatalf("%d %s sent=%d", r.StatusCode, r.Header.Get("Location"), p.push.sent)
	}
	r = p.do("POST", "/notifications/push/test", url.Values{"csrf": {s.csrf}}, s.with()...)
	if r.StatusCode != 429 {
		t.Fatalf("second test = %d, want 429", r.StatusCode)
	}
	// Remove: CSRF required; someone else's device is a 404.
	if r := p.do("POST", "/notifications/push/"+idm[1]+"/remove", url.Values{}, s.with()...); r.StatusCode != 403 {
		t.Fatalf("remove without CSRF = %d", r.StatusCode)
	}
	other := p.loginAs("alice")
	if r := p.do("POST", "/notifications/push/"+idm[1]+"/remove", url.Values{"csrf": {other.csrf}}, other.with()...); r.StatusCode != 404 {
		t.Fatalf("alice removing root's device = %d, want 404", r.StatusCode)
	}
	r = p.do("POST", "/notifications/push/"+idm[1]+"/remove", url.Values{"csrf": {s.csrf}}, s.with()...)
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "flash.push_removed") {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if d, _ := p.ntf.Subscriptions(ctx, "user:root"); len(d) != 1 {
		t.Fatalf("devices left = %d", len(d))
	}
}

func TestPushPreferenceTableAndDeepLinkTarget(t *testing.T) {
	p := newPUI(t)
	s := p.login()
	page := p.do("GET", "/notifications", nil, s.with()...).body
	if !strings.Contains(page, `name="channel" value="webpush"`) {
		t.Error("no per-type Web Push preference table")
	}
	r := p.do("POST", "/notifications/prefs", url.Values{"csrf": {s.csrf}, "type": {"approval.requested"}, "channel": {"webpush"}}, s.with()...)
	if r.StatusCode != 303 {
		t.Fatalf("%d", r.StatusCode)
	}
	// The deep link a push opens is a normal authenticated page.
	id := p.request("user:dave", "Laptop")
	if r := p.do("GET", "/approvals/"+id, nil); r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
		t.Fatalf("signed-out deep link: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

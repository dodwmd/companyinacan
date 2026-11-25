// Package ticket is single sign-on from the admin UI into the module web UIs
// (specs/036-module-ui-sso). A signed-in administrator opens a module from the Apps page; can-core
// mints a one-time LAUNCH TICKET bound to that one module (the audience) and the browser carries it
// to the module's UI, which redeems it over the module API and starts a session of its own. The
// ticket is not a credential for anything else: it lives 60 seconds at most, works once, only for
// the module it was minted for, and is stored as a hash. The module never receives a long-lived
// bearer; it keeps a session handle it uses to ask whether the person is still allowed.
//
// Everything fails closed and every failure to redeem looks the same ({"active": false}).
package ticket

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Lifetimes, limits and shapes.
const (
	// MaxTicketTTL is the longest a ticket can be redeemed after it was minted.
	MaxTicketTTL = 60 * time.Second
	// SessionLifetime caps a module session that began with a ticket: after it the handle no longer
	// revalidates and the person comes back through the launcher.
	SessionLifetime = 12 * time.Hour
	// MintPerMinute is the number of tickets one person may mint per minute.
	MintPerMinute = 30
	TicketPrefix  = "cant_"
	HandlePrefix  = "cans_"
	BindingPrefix = modclient.BindingPrefix
	maxName       = 80
	maxTracked    = 10000
)

var (
	// ErrNotAllowed covers every reason a person cannot open a module (unknown or disabled module
	// or account, no UI, no permission): the caller cannot tell them apart.
	ErrNotAllowed = errors.New("not allowed to open this app")
	// ErrRateLimited means too many tickets were minted in the last minute.
	ErrRateLimited = errors.New("too many launches; retry later")
	// ErrCookieScope means the launching browser could not be given a cookie the module would
	// receive: the admin UI was reached on a name that shares nothing with the module's.
	ErrCookieScope = errors.New("the admin UI's address and the app's address share no cookie scope")
	// ErrInvalid is a malformed request (return path).
	ErrInvalid = errors.New("invalid launch request")

	secretRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	moduleRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	subjectRe = regexp.MustCompile(`^user:[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
	// returnRe is the allowlist for the place to land in the module, relative to the module's UI
	// base: a slash and letters, digits, slashes, underscores and hyphens. No dots (so no dot
	// segments), no query, no fragment, no scheme.
	returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)
	hostRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	pathRe   = regexp.MustCompile(`^(/[a-z0-9-]{1,32}){0,2}$`)
)

// LooksLikeTicket reports whether s has the shape of a launch ticket.
func LooksLikeTicket(s string) bool {
	return strings.HasPrefix(s, TicketPrefix) && secretRe.MatchString(s[len(TicketPrefix):])
}

// LooksLikeHandle reports whether s has the shape of a session handle.
func LooksLikeHandle(s string) bool {
	return strings.HasPrefix(s, HandlePrefix) && secretRe.MatchString(s[len(HandlePrefix):])
}

// ValidReturnPath reports whether p may be stored as the landing path of a ticket ("" is fine).
func ValidReturnPath(p string) bool {
	return p == "" || (returnRe.MatchString(p) && !strings.Contains(p, "//"))
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

// Launcher decides where a module's UI lives. The address comes only from can-core's own
// configuration (the module manifest and the operator's overrides), never from a request.
type Launcher struct {
	// Domain is the company domain: a module UI must be on it or on a subdomain of it.
	Domain string
	// Overrides maps a module id to the base URL of its UI (operator configuration,
	// CAN_MODULE_UI_URLS); it wins over the manifest.
	Overrides map[string]string
	// AllowHTTP permits http and localhost addresses (plain-HTTP local development only).
	AllowHTTP bool
}

// Base returns the base URL of the module's UI, with no trailing slash. ok is false when the module
// does not declare runtime.web_login (its UI cannot take tickets, whatever else it serves) or has
// no acceptable address. An operator's address without a path gets the manifest's base_path.
func (l Launcher) Base(m store.Module) (string, bool) {
	wl := m.Manifest.Runtime.WebLogin
	if wl == nil {
		return "", false
	}
	raw := strings.TrimSuffix(l.Overrides[m.ID], "/")
	switch {
	case raw != "":
		if u, err := url.Parse(raw); err == nil && u.Path == "" {
			raw += wl.BasePath
		}
	case wl.ViaSubdomain && m.Manifest.Runtime.Subdomain != "" && l.Domain != "":
		raw = "https://" + m.Manifest.Runtime.Subdomain + "." + l.Domain + wl.BasePath
	default:
		return "", false
	}
	return l.validate(raw)
}

// Acceptable reports whether an operator's address for a module UI would be used.
func (l Launcher) Acceptable(raw string) bool {
	_, ok := l.validate(strings.TrimSuffix(raw, "/"))
	return ok
}

func (l Launcher) validate(raw string) (string, bool) {
	if strings.ContainsAny(raw, "?#\\ \t\r\n") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !l.AllowHTTP) {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || !hostRe.MatchString(host) {
		return "", false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	if !pathRe.MatchString(u.Path) {
		return "", false
	}
	local := l.AllowHTTP && (host == "localhost" || host == "127.0.0.1")
	d := strings.ToLower(l.Domain)
	inDomain := d != "" && (host == d || strings.HasSuffix(host, "."+d))
	if !local && !inDomain {
		return "", false
	}
	// The admin UI's own host would put a module's pages in the admin UI's origin.
	if host == "admin" || strings.HasPrefix(host, "admin.") {
		return "", false
	}
	out := u.Scheme + "://" + host
	if p := u.Port(); p != "" {
		out += ":" + p
	}
	return out + u.Path, true
}

// App is one module a person may open.
type App struct {
	ID, Name string
}

// Service mints, redeems and revalidates launch tickets.
type Service struct {
	DB       *store.DB
	Audit    *audit.Log
	Launcher Launcher
	// Holds reports whether a subject holds an RBAC permission (the same lookup personal access
	// tokens use). Without it nobody holds anything.
	Holds func(ctx context.Context, subject, permission string) (bool, error)
	Now   func() time.Time
	// FailFree is how many invalid redeems per module and minute are answered at full speed
	// (default 60); past it each waits FailPenalty (default 200 ms). A valid redeem is never
	// delayed. Sleep replaces the wait in tests.
	FailFree    int
	FailPenalty time.Duration
	Sleep       func(context.Context, time.Duration)

	mu    sync.Mutex
	mints map[string]*window
	fails map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// count adds one to the key's per-minute counter in m and returns the new value.
func (s *Service) count(m *map[string]*window, key string) int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if *m == nil || len(*m) > maxTracked {
		*m = map[string]*window{}
	}
	w := (*m)[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		(*m)[key] = w
	}
	w.n++
	return w.n
}

// permissions are the module's manifest permissions the subject holds right now.
func (s *Service) permissions(ctx context.Context, subject string, m store.Module) ([]string, error) {
	if s.Holds == nil {
		return nil, nil
	}
	var out []string
	for _, p := range m.Manifest.Permissions {
		ok, err := pat.HeldBy(ctx, s.Holds, subject, m.ID, p)
		if err != nil {
			return nil, err
		}
		if ok && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// person returns the active account behind a subject and the name to show for it ("" when the
// account is gone or disabled).
func (s *Service) person(ctx context.Context, subject string) (string, bool) {
	if !subjectRe.MatchString(subject) {
		return "", false
	}
	u, err := s.DB.AdminByUsername(ctx, strings.TrimPrefix(subject, "user:"))
	if err != nil || s.DB.AdminDisabled(ctx, u.ID) {
		return "", false
	}
	name := u.Username
	if id, err := s.DB.SSOIdentityByUser(ctx, u.ID); err == nil && strings.TrimSpace(id.Name) != "" {
		name = id.Name
	}
	return cleanName(name), true
}

func cleanName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxName {
			break
		}
	}
	return b.String()
}

func (s *Service) enabledModule(ctx context.Context, id string) (store.Module, bool) {
	if !moduleRe.MatchString(id) {
		return store.Module{}, false
	}
	m, err := s.DB.GetModule(ctx, id)
	if err != nil || m.State != store.StateEnabled {
		return store.Module{}, false
	}
	return m, true
}

// Apps lists the enabled modules with a UI that subject holds a permission in, by id.
func (s *Service) Apps(ctx context.Context, subject string) ([]App, error) {
	mods, err := s.DB.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	var out []App
	for _, m := range mods {
		if m.State != store.StateEnabled {
			continue
		}
		if _, ok := s.Launcher.Base(m); !ok {
			continue
		}
		perms, err := s.permissions(ctx, subject, m)
		if err != nil {
			return nil, err
		}
		if len(perms) == 0 {
			continue
		}
		name := m.Manifest.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, App{ID: m.ID, Name: name})
	}
	slices.SortFunc(out, func(a, b App) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// LaunchResult is a minted ticket as a URL to send the browser to, plus the nonce that browser
// must hold (BindingCookie).
type LaunchResult struct {
	URL string
	// ID is the ticket's record id (safe to log); the ticket value is only inside URL.
	ID string
	// Module is the audience and Base the base URL of its UI.
	Module, Base string
	// Binding is the nonce that ties the ticket to the launching browser. It goes into a cookie and
	// nowhere else.
	Binding string
}

// BindingCookie is the cookie the launching browser must be given along with the URL: the module
// receives it and presents it at redeem, so a ticket opened in any other browser (an attacker's
// link sent to a victim) is worthless. adminHost is the Host the admin UI was reached on. The
// cookie is host-only when both are the same host (plain-http development on localhost, where ports
// do not separate cookies) and carries Domain=<company domain> otherwise; when neither can work
// the result is ErrCookieScope.
func (s *Service) BindingCookie(r LaunchResult, adminHost string) (*http.Cookie, error) {
	u, err := url.Parse(r.Base)
	if err != nil || u.Hostname() == "" {
		return nil, ErrCookieScope
	}
	if h, _, err := net.SplitHostPort(adminHost); err == nil {
		adminHost = h
	}
	admin, mod := strings.ToLower(adminHost), strings.ToLower(u.Hostname())
	secure := u.Scheme == "https"
	c := &http.Cookie{Name: modclient.LaunchCookieName(r.Module, secure), Value: r.Binding, Path: u.Path + "/login/ticket",
		MaxAge: int(MaxTicketTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}
	if admin == mod {
		return c, nil
	}
	d := strings.ToLower(s.Launcher.Domain)
	under := func(h string) bool { return h == d || strings.HasSuffix(h, "."+d) }
	// A browser refuses Domain= for a single label (such as "localhost").
	if !strings.Contains(d, ".") || !under(admin) || !under(mod) {
		return nil, ErrCookieScope
	}
	c.Domain = d
	return c, nil
}

// Launch mints a ticket for subject to open module and returns the module's ticket URL. returnPath
// is where to land inside the module (see ValidReturnPath). notAfter, when not zero, is the latest
// the module session may live (the identity provider's revalidation deadline of the launching
// sign-in). The ticket value is in the URL only.
func (s *Service) Launch(ctx context.Context, subject, module, returnPath string, notAfter time.Time) (LaunchResult, error) {
	if !subjectRe.MatchString(subject) || (!notAfter.IsZero() && !notAfter.After(s.now())) {
		return LaunchResult{}, ErrNotAllowed
	}
	if s.count(&s.mints, subject) > MintPerMinute {
		return LaunchResult{}, ErrRateLimited
	}
	if !ValidReturnPath(returnPath) {
		return LaunchResult{}, ErrInvalid
	}
	m, ok := s.enabledModule(ctx, module)
	if !ok {
		return LaunchResult{}, ErrNotAllowed
	}
	base, ok := s.Launcher.Base(m)
	if !ok {
		return LaunchResult{}, ErrNotAllowed
	}
	if _, ok := s.person(ctx, subject); !ok {
		return LaunchResult{}, ErrNotAllowed
	}
	perms, err := s.permissions(ctx, subject, m)
	if err != nil {
		return LaunchResult{}, err
	}
	if len(perms) == 0 {
		return LaunchResult{}, ErrNotAllowed
	}
	now := s.now()
	tk, binding := randomSecret(TicketPrefix), randomSecret(modclient.BindingPrefix)
	rec := store.LoginTicket{ID: randomID("tkt_"), TokenHash: hashOf(tk), Subject: subject, ModuleID: m.ID, ReturnPath: returnPath,
		BindingHash: hashOf(binding), NotAfter: notAfter, CreatedAt: now, ExpiresAt: now.Add(MaxTicketTTL)}
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.DB.InsertLoginTicket(ctx, tx, rec); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: subject, Action: "ui.ticket.mint", Target: m.ID, Detail: map[string]any{"ticket_id": rec.ID}})
		return err
	})
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{URL: base + "/login/ticket?t=" + tk, ID: rec.ID, Module: m.ID, Base: base, Binding: binding}, nil
}

// Peeked is who a ticket would sign in. Its zero value is "inactive".
type Peeked struct {
	Subject string
	Name    string
}

// Peek names the account behind a ticket without consuming it, so the module can show it before
// the person confirms. It answers only for what Redeem would accept, the browser's nonce included,
// so a ticket alone learns nothing.
func (s *Service) Peek(ctx context.Context, callerModule, ticket, binding string) (Peeked, bool) {
	if !LooksLikeTicket(ticket) || !modclient.BindingShaped(binding) {
		s.penalise(ctx, callerModule)
		return Peeked{}, false
	}
	subject, ok, err := s.DB.PeekLoginTicket(ctx, hashOf(ticket), callerModule, hashOf(binding), s.now())
	if err != nil || !ok {
		s.penalise(ctx, callerModule)
		return Peeked{}, false
	}
	if _, ok := s.enabledModule(ctx, callerModule); !ok {
		return Peeked{}, false
	}
	name, ok := s.person(ctx, subject)
	if !ok {
		return Peeked{}, false
	}
	return Peeked{Subject: subject, Name: name}, true
}

// Redeemed is what a module learns when it redeems a ticket. Its zero value is "inactive".
type Redeemed struct {
	Subject string
	// Name is the person's display name; never an email address or group.
	Name        string
	Permissions []string
	ReturnPath  string
	// Handle is the opaque session handle the module keeps (in memory) to revalidate the person.
	Handle         string
	SessionExpires time.Time
}

// penalise slows invalid redeems of one module past a per-minute quota; it never runs for a valid
// ticket.
func (s *Service) penalise(ctx context.Context, module string) {
	free, pen := s.FailFree, s.FailPenalty
	if free <= 0 {
		free = 60
	}
	if pen <= 0 {
		pen = 200 * time.Millisecond
	}
	if s.count(&s.fails, module) <= free {
		return
	}
	if s.Sleep != nil {
		s.Sleep(ctx, pen)
		return
	}
	t := time.NewTimer(pen)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Redeem exchanges a ticket for the person and their permissions, on behalf of the calling module.
// binding is the nonce cookie of the browser that presented the ticket. ok is false for every kind
// of failure (unknown, malformed, used, expired, another module's, another browser, the person gone
// or without permission) and nothing tells them apart.
func (s *Service) Redeem(ctx context.Context, callerModule, ticket, binding string) (Redeemed, bool) {
	if !LooksLikeTicket(ticket) || !modclient.BindingShaped(binding) {
		s.penalise(ctx, callerModule)
		return Redeemed{}, false
	}
	now := s.now()
	var rec store.LoginTicket
	var found bool
	err := s.DB.InTx(ctx, func(tx *sql.Tx) (err error) {
		rec, found, err = s.DB.RedeemLoginTicket(ctx, tx, hashOf(ticket), callerModule, hashOf(binding), now)
		return err
	})
	if err != nil || !found {
		s.penalise(ctx, callerModule)
		return Redeemed{}, false
	}
	m, ok := s.enabledModule(ctx, callerModule)
	if !ok {
		return Redeemed{}, false
	}
	name, ok := s.person(ctx, rec.Subject)
	if !ok {
		return Redeemed{}, false
	}
	perms, err := s.permissions(ctx, rec.Subject, m)
	if err != nil || len(perms) == 0 {
		return Redeemed{}, false
	}
	handle := randomSecret(HandlePrefix)
	exp := now.Add(SessionLifetime)
	if !rec.NotAfter.IsZero() && rec.NotAfter.Before(exp) {
		exp = rec.NotAfter
	}
	if !exp.After(now) {
		return Redeemed{}, false
	}
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.DB.InsertLaunchSession(ctx, tx, store.LaunchSession{HandleHash: hashOf(handle), TicketID: rec.ID, Subject: rec.Subject, ModuleID: m.ID, CreatedAt: now, ExpiresAt: exp}); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: "module:" + m.ID, Action: "ui.ticket.redeem", Target: rec.ID, Detail: map[string]any{"subject": rec.Subject}})
		return err
	})
	if err != nil {
		return Redeemed{}, false
	}
	return Redeemed{Subject: rec.Subject, Name: name, Permissions: perms, ReturnPath: rec.ReturnPath, Handle: handle, SessionExpires: exp}, true
}

// Validated is the current standing of a person in a module. Its zero value is "inactive".
type Validated struct {
	Subject     string
	Name        string
	Permissions []string
}

// Revalidate answers whether the person behind a session handle is still allowed in the calling
// module and with what: permissions are recomputed from RBAC now. It is false when the handle is
// unknown, another module's, revoked or past its lifetime, or the person is disabled or holds no
// permission any more.
func (s *Service) Revalidate(ctx context.Context, callerModule, handle string) (Validated, bool) {
	if !LooksLikeHandle(handle) {
		s.penalise(ctx, callerModule)
		return Validated{}, false
	}
	ls, err := s.DB.LaunchSessionByHash(ctx, hashOf(handle))
	if err != nil {
		s.penalise(ctx, callerModule)
		return Validated{}, false
	}
	if ls.ModuleID != callerModule || !ls.RevokedAt.IsZero() || !s.now().Before(ls.ExpiresAt) {
		return Validated{}, false
	}
	m, ok := s.enabledModule(ctx, callerModule)
	if !ok {
		return Validated{}, false
	}
	name, ok := s.person(ctx, ls.Subject)
	if !ok {
		return Validated{}, false
	}
	perms, err := s.permissions(ctx, ls.Subject, m)
	if err != nil || len(perms) == 0 {
		return Validated{}, false
	}
	return Validated{Subject: ls.Subject, Name: name, Permissions: perms}, true
}

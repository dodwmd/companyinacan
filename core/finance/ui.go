package finance

// The server-rendered web UI of the finance module (epic E15b, specs/031-finance-ui). html/template,
// an embedded stylesheet, no JavaScript. It is a client of the Store exactly like the JSON API:
// every page calls the same Store methods, which scope every query to the caller's token and
// decide approvals in one place (authorizeApproval), so there is no second authorisation path.
// The UI decides only presentation, plus the same scope-per-route check the API's guard makes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

//go:embed ui/templates ui/static ui/public
var uiAssets embed.FS

// UI serves the web UI.
type UI struct {
	Store *Store
	Auth  Authenticator // the chain the API uses; it validates the token presented at login
	Log   *slog.Logger
	// Secure sets the Secure attribute (and the __Host- cookie prefix). False only for plain-HTTP
	// local development.
	Secure bool
	// TrustProxy takes the client address for throttling failed logins from X-Forwarded-For.
	TrustProxy bool
	Now        func() time.Time
	// Sessions is can-core's launch-ticket API (single sign-on from the admin UI, specs/036-module-ui-sso).
	// Nil: /login/ticket does not exist.
	Sessions modclient.SessionAPI
	// NoTokenLogin switches off the pasted-token sign-in (ui.token_login: false).
	NoTokenLogin bool

	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	RecheckEvery    time.Duration

	Cfg              *Config // seller, base currency, retention, public link base
	Seller           Seller
	LinkSecret       []byte
	ESign            ESigner // nil: contracts can only be sent for manual signature
	NotificationsURL string

	once     sync.Once
	sessions *session.Manager[Principal]
	tpl      map[string]*template.Template
	tplMu    sync.Mutex
	funcs    template.FuncMap
	failIP   *rateLimiter
	perUser  *rateLimiter
}

type route struct {
	pattern string
	perm    string // scope the route needs ("" = a session is enough)
	own     bool   // an own-only (employee) token may use it
	h       uiHandler
}

func (u *UI) routes() []route {
	r, w, a, d := PermRead, PermWrite, PermApprove, PermAdmin
	lr, lw := PermLedgerRead, PermLedgerWrite
	return []route{
		{"GET /{$}", "", true, u.home},

		{"GET /parties", r, false, u.parties},
		{"GET /parties/new", w, false, u.partyNew},
		{"POST /parties", w, false, u.partyCreate},
		{"GET /parties/{id}", r, false, u.party},
		{"GET /parties/{id}/edit", w, false, u.partyEdit},
		{"POST /parties/{id}", w, false, u.partyUpdate},
		{"GET /parties/{id}/erase", d, false, u.partyEraseForm},
		{"POST /parties/{id}/erase", d, false, u.partyErase},

		{"GET /invoices", r, false, u.invoices},
		{"GET /invoices/new", w, false, u.invoiceNew},
		{"POST /invoices", w, false, u.invoiceCreate},
		{"GET /invoices/{id}", r, false, u.invoice},
		{"GET /invoices/{id}/edit", w, false, u.invoiceEdit},
		{"POST /invoices/{id}", w, false, u.invoiceUpdate},
		{"POST /invoices/{id}/delete", w, false, u.invoiceDelete},
		{"POST /invoices/{id}/request-approval", w, false, u.invoiceRequestApproval},
		{"POST /invoices/{id}/approve", a, false, u.invoiceApprove},
		{"POST /invoices/{id}/send", w, false, u.invoiceSend},
		{"POST /invoices/{id}/void", a, false, u.invoiceVoid},
		{"GET /invoices/{id}/credit-note", w, false, u.creditNoteForm},
		{"POST /invoices/{id}/credit-note", w, false, u.creditNoteCreate},
		{"POST /invoices/{id}/payments", w, false, u.paymentCreate},
		{"POST /invoices/{id}/link", w, false, u.linkCreate},
		{"POST /invoices/{id}/link/revoke", w, false, u.linkRevoke},
		{"GET /invoices/{id}/html", r, false, u.invoiceHTML},
		{"GET /invoices/{id}/pdf", r, false, u.invoicePDF},
		{"POST /invoices/{id}/email", w, false, u.invoiceEmail},
		{"POST /invoices/{id}/reminders", w, false, u.invoiceRemindersPost},
		{"POST /parties/{id}/statement", w, false, u.partyStatement},
		{"POST /parties/{id}/reminders", w, false, u.partyRemindersPost},

		{"GET /exports/invoices.csv", r, false, u.exportInvoices},
		{"GET /exports/payments.csv", r, false, u.exportPayments},
		{"GET /exports/expenses.csv", r, false, u.exportExpenses},

		{"GET /recurring", r, false, u.recurring},
		{"GET /recurring/new", w, false, u.recurringNew},
		{"POST /recurring", w, false, u.recurringCreate},
		{"POST /recurring/{id}/status", w, false, u.recurringStatus},

		{"GET /contracts", r, false, u.contracts},
		{"GET /contracts/new", w, false, u.contractNew},
		{"POST /contracts", w, false, u.contractCreate},
		{"GET /contracts/{id}", r, false, u.contract},
		{"GET /contracts/{id}/versions/{n}", r, false, u.contractVersion},
		{"POST /contracts/{id}/send", w, false, u.contractSend},
		{"POST /contracts/{id}/terminate", w, false, u.contractTerminate},
		{"POST /contracts/{id}/dates", w, false, u.contractDates},
		{"POST /contracts/{id}/signing", w, false, u.signingSend},
		{"POST /contracts/{id}/signing/revoke", w, false, u.signingRevoke},
		{"POST /contracts/{id}/signing/signers/{signer}/resend", w, false, u.signingResend},
		{"GET /contracts/{id}/signing/certificate", r, false, u.signingCertificate},

		{"GET /timesheets", r, true, u.timesheets},
		{"POST /timesheets", w, true, u.timesheetOpen},
		{"GET /timesheets/{id}", r, true, u.timesheet},
		{"POST /timesheets/{id}/entries", w, true, u.timesheetEntries},
		{"POST /timesheets/{id}/submit", w, true, u.timesheetSubmit},
		{"POST /timesheets/{id}/approve", a, false, u.timesheetApprove},
		{"POST /timesheets/{id}/reject", a, false, u.timesheetReject},

		{"GET /expenses", r, true, u.expenses},
		{"GET /expenses/new", w, true, u.expenseNew},
		{"POST /expenses", w, true, u.expenseCreate},
		{"GET /expenses/{id}", r, true, u.expense},
		{"POST /expenses/{id}/submit", w, true, u.expenseSubmit},
		{"POST /expenses/{id}/approve", a, false, u.expenseApprove},
		{"POST /expenses/{id}/reject", a, false, u.expenseReject},
		{"POST /expenses/{id}/reimburse", w, false, u.expenseReimburse},

		{"GET /purchase-orders", r, false, u.purchaseOrders},
		{"GET /purchase-orders/new", w, false, u.poNew},
		{"POST /purchase-orders", w, false, u.poCreate},
		{"GET /purchase-orders/{id}", r, false, u.purchaseOrder},
		{"POST /purchase-orders/{id}/request-approval", w, false, u.poRequestApproval},
		{"POST /purchase-orders/{id}/approve", a, false, u.poApprove},
		{"POST /purchase-orders/{id}/status", w, false, u.poStatus},

		{"GET /approvals", a, false, u.approvals},
		{"GET /ledger/accounts", lr, false, u.ledgerAccounts},
		{"GET /ledger/journals", lr, false, u.ledgerJournals},
		{"GET /ledger/journals/{id}", lr, false, u.ledgerJournal},
		{"GET /ledger/trial-balance", lr, false, u.ledgerTrialBalance},
		{"GET /bank/reconciliation", lr, false, u.bankReconciliation},
		{"POST /bank/transactions/{id}/confirm", lw, false, u.bankConfirm},
		{"POST /bank/transactions/{id}/ignore", lw, false, u.bankIgnore},
		{"GET /reports/aging", r, false, u.aging},
		{"GET /reports/statement", r, false, u.statement},
		{"GET /settings", r, false, u.settings},
		{"GET /settings/reminders", r, false, u.dunning},
		{"POST /settings/reminders", d, false, u.dunningPost},
	}
}

// publicRoutes need no session.
func (u *UI) publicRoutes() []struct {
	pattern string
	h       http.HandlerFunc
} {
	return []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /login", u.loginPage},
		{"POST /login", u.loginPost},
		{"GET /login/ticket", u.ticketPage},
		{"POST /login/ticket", u.ticketPost},
		{"GET /static/{file}", u.static},
		{"GET /manifest.webmanifest", u.manifest},
	}
}

// UIRoutes lists the "METHOD /path" patterns the UI serves, with the scope each needs; the
// contract test compares them with specs/031-finance-ui/contracts/ui-routes.md.
func UIRoutes() (patterns []string, scope map[string]string) {
	u := &UI{}
	scope = map[string]string{}
	for _, p := range u.publicRoutes() {
		patterns = append(patterns, p.pattern)
		scope[p.pattern] = "-"
	}
	patterns = append(patterns, "POST /logout")
	scope["POST /logout"] = "session+own"
	for _, r := range u.routes() {
		patterns = append(patterns, r.pattern)
		s := strings.TrimPrefix(r.perm, "finance.")
		if s == "" {
			s = "session"
		}
		if r.own {
			s += "+own"
		}
		scope[r.pattern] = s
	}
	return patterns, scope
}

func (u *UI) init() {
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
	if u.IdleTimeout <= 0 {
		u.IdleTimeout = uiDefaultIdle
	}
	if u.AbsoluteTimeout <= 0 {
		u.AbsoluteTimeout = uiDefaultAbsolute
	}
	if u.RecheckEvery <= 0 {
		u.RecheckEvery = uiRecheckDefault
	}
	if u.Cfg == nil {
		u.Cfg = DefaultConfig()
	}
	u.sessions = u.newSessions()
	u.failIP = newRateLimiter(uiFailedLogins, time.Minute, 10000, u.Now)
	u.perUser = newRateLimiter(600, time.Minute, 10000, u.Now)
	funcs := template.FuncMap{
		"t":     u.msg,
		"money": FormatMinor,
		"amt":   func(m int64, cur string) string { return FormatMinor(m, cur) + " " + cur },
		"milli": FormatMilli,
		"bp":    FormatBP,
		"ppm":   FormatPPM,
		"hm":    FormatHM,
		"when":  func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"whenp": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"deref":   deref,
		"deref64": func(p *int64) int64 { return *p },
		"st":      func(kind, v string) string { return u.msgOr(kind+"."+v, v) },
		"inc":     func(i int) int { return i + 1 },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
	}
	u.funcs = funcs
	u.tpl = map[string]*template.Template{}
}

// template returns the parsed page template, parsing it on first use. Parsing every page at start
// costs about 4 MiB of heap on a module budgeted at 20 MiB idle (specs/031-finance-ui/research.md
// R2), and most installations never open most pages.
func (u *UI) template(name string) (*template.Template, error) {
	u.tplMu.Lock()
	defer u.tplMu.Unlock()
	if t, ok := u.tpl[name]; ok {
		return t, nil
	}
	if name == "layout.html" || name == "partials.html" {
		return nil, fs.ErrNotExist
	}
	t, err := template.New(name).Funcs(u.funcs).ParseFS(uiAssets, "ui/templates/layout.html", "ui/templates/partials.html", "ui/templates/"+name)
	if err != nil {
		return nil, err
	}
	u.tpl[name] = t
	return t, nil
}

// Handler returns the UI handler.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	mux := http.NewServeMux()
	for _, p := range u.publicRoutes() {
		mux.HandleFunc(p.pattern, p.h)
	}
	mux.HandleFunc("POST /logout", u.authedFunc("", true, u.logout))
	for _, r := range u.routes() {
		mux.HandleFunc(r.pattern, u.authedFunc(r.perm, r.own, r.h))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ss, p, ok := u.sessions.Current(w, r); ok {
			u.renderError(w, r, &uiCtx{u: u, ss: ss, p: &p, r: r, w: w}, http.StatusNotFound, "err.not_found_title", "err.not_found")
			return
		}
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session.SetHeaders(w.Header())
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				u.Log.Error("panic in ui handler", "panic", p)
				if rec.status == http.StatusOK {
					u.renderError(rec, r, nil, http.StatusInternalServerError, "err.internal_title", "err.internal")
				}
			}
			// The query string is left out: it can hold names and ids.
			u.Log.Info("ui request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !session.SameOrigin(r) {
				u.renderError(rec, r, nil, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, 256<<10)
		}
		mux.ServeHTTP(rec, r)
	})
}

// uiCtx is what a handler gets: the session, the resolved caller and the request.
type uiCtx struct {
	u  *UI
	ss *uiSession
	p  *Principal
	r  *http.Request
	w  http.ResponseWriter
}

type uiHandler func(c *uiCtx) error

func (c *uiCtx) ctx() context.Context { return c.r.Context() }

func (c *uiCtx) id(name string) (int64, error) {
	n, err := parseID(c.r.PathValue(name))
	if err != nil {
		return 0, ErrNotFound
	}
	return n, nil
}

func (c *uiCtx) redirect(path string, okKey string) error {
	if okKey != "" {
		path = withOK(path, okKey)
	}
	http.Redirect(c.w, c.r, path, http.StatusSeeOther)
	return nil
}

// authedFunc wraps a handler: it needs a live session, the shared per-subject limit, the scope the
// API's guard would ask for on the same operation, the CSRF token on unsafe methods, and turns
// Store errors into pages.
func (u *UI) authedFunc(perm string, own bool, fn uiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ss, p, ok := u.sessions.Current(w, r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		c := &uiCtx{u: u, ss: ss, p: &p, r: r, w: w}
		if ok, wait := u.perUser.allow(ss.Subject); !ok {
			w.Header().Set("Retry-After", itoa(int64(wait.Seconds())+1))
			u.renderError(w, r, c, http.StatusTooManyRequests, "err.busy_title", "err.busy")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := r.ParseForm(); err != nil || !session.Equal(r.PostForm.Get("csrf"), ss.CSRF) {
				u.renderError(w, r, c, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
		}
		if !allowedBy(c.p, perm, own) {
			u.renderError(w, r, c, http.StatusForbidden, "err.forbidden_title", "err.forbidden")
			return
		}
		if err := fn(c); err != nil {
			u.fail(c, err)
		}
	}
}

// allowedBy is the API guard's rule, shared so the two cannot drift: the scope must be held, and an
// own-only token (an employee's) only reaches the routes marked for it.
func allowedBy(p *Principal, perm string, own bool) bool {
	if perm != "" && !p.Can(perm) {
		return false
	}
	// Accounting data spans every party and project: a scoped or own-only token never reaches it.
	if slices.Contains(ledgerPerms, perm) && (!p.Unscoped() || p.OwnOnly) {
		return false
	}
	return !p.OwnOnly || own
}

// uiStatus maps a Store error to the HTTP status of the page that reports it.
func uiStatus(err error) int {
	var inv *InvalidError
	var mb *http.MaxBytesError
	switch {
	case errors.Is(err, errBadForm):
		return http.StatusBadRequest
	case errors.As(err, &inv), errors.Is(err, ErrIdempotencyMismatch):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrSeparation), errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, ErrLocked), errors.Is(err, ErrVersionMismatch):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrApprovalsUnavailable), strings.Contains(err.Error(), "database is locked"):
		return http.StatusServiceUnavailable
	case errors.As(err, &mb):
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

func (u *UI) fail(c *uiCtx, err error) {
	st := uiStatus(err)
	var inv *InvalidError
	switch {
	case errors.Is(err, errBadForm):
		u.renderError(c.w, c.r, c, st, "err.bad_form_title", "err.bad_form")
	case errors.As(err, &inv):
		u.renderProblems(c, st, "err.invalid_title", inv.Problems)
	case st == http.StatusNotFound:
		u.renderError(c.w, c.r, c, st, "err.not_found_title", "err.not_found")
	case errors.Is(err, ErrSeparation):
		u.renderError(c.w, c.r, c, st, "err.separation_title", "err.separation")
	case errors.Is(err, ErrForbidden):
		u.renderError(c.w, c.r, c, st, "err.forbidden_title", "err.forbidden")
	case errors.Is(err, ErrVersionMismatch):
		u.renderError(c.w, c.r, c, st, "err.stale_title", "err.stale")
	case errors.Is(err, ErrLocked):
		u.renderMsg(c.w, c.r, c, st, "err.locked_title", err.Error())
	case errors.Is(err, ErrConflict):
		u.renderMsg(c.w, c.r, c, st, "err.conflict_title", strings.TrimPrefix(err.Error(), "conflict: "))
	case errors.Is(err, ErrIdempotencyMismatch):
		u.renderError(c.w, c.r, c, st, "err.invalid_title", "err.idem")
	case st == http.StatusServiceUnavailable:
		c.w.Header().Set("Retry-After", "5")
		u.renderError(c.w, c.r, c, st, "err.unavailable_title", "err.unavailable")
	case st == http.StatusRequestEntityTooLarge:
		u.renderError(c.w, c.r, c, st, "err.too_large_title", "err.too_large")
	default:
		u.Log.Error("ui internal error", "error", err)
		u.renderError(c.w, c.r, c, http.StatusInternalServerError, "err.internal_title", "err.internal")
	}
}

// ---- rendering -------------------------------------------------------------------------------

type navItem struct {
	Href, Label string
	Current     bool
}

// caps is what the signed-in token may do; templates use it to show or hide controls. Hiding is
// presentation: the routes and the Store check the same things again.
type caps struct {
	Read, Write, Approve, Admin, OwnOnly bool
	// Ledger and LedgerWrite are the accounting permissions, which only an unscoped token has (allowedBy):
	// they decide whether the navigation shows the accounting pages and the reconciliation forms.
	Ledger, LedgerWrite bool
}

func capsOf(p *Principal) caps {
	return caps{Read: p.Can(PermRead), Write: p.Can(PermWrite), Approve: p.Can(PermApprove), Admin: p.Can(PermAdmin), OwnOnly: p.OwnOnly,
		Ledger: allowedBy(p, PermLedgerRead, false), LedgerWrite: allowedBy(p, PermLedgerWrite, false)}
}

// page is the data every template receives.
type page struct {
	Lang    string
	Title   string
	Nav     []navItem
	CSRF    string
	Authed  bool
	Flash   string
	Notif   string
	Caps    caps
	Subject string
	Data    any
}

func (u *UI) nav(path string, p *Principal) []navItem {
	cp := capsOf(p)
	type it struct {
		href, key string
		show      bool
	}
	biz := cp.Read && !cp.OwnOnly
	items := []it{
		{"/", "nav.home", true}, {"/approvals", "nav.approvals", cp.Approve && !cp.OwnOnly}, {"/parties", "nav.parties", biz},
		{"/invoices", "nav.invoices", biz}, {"/recurring", "nav.recurring", biz}, {"/contracts", "nav.contracts", biz},
		{"/purchase-orders", "nav.pos", biz}, {"/timesheets", "nav.timesheets", cp.Read}, {"/expenses", "nav.expenses", cp.Read},
		{"/reports/aging", "nav.reports", biz}, {"/settings", "nav.settings", biz},
		{"/ledger/accounts", "nav.ledger", cp.Ledger}, {"/bank/reconciliation", "nav.bank", cp.Ledger},
	}
	out := make([]navItem, 0, len(items))
	for _, i := range items {
		if !i.show {
			continue
		}
		cur := path == i.href || (i.href != "/" && strings.HasPrefix(path, i.href+"/"))
		if i.href == "/reports/aging" && strings.HasPrefix(path, "/reports/") {
			cur = true
		}
		if i.href == "/ledger/accounts" && strings.HasPrefix(path, "/ledger/") {
			cur = true
		}
		if i.href == "/bank/reconciliation" && strings.HasPrefix(path, "/bank/") {
			cur = true
		}
		out = append(out, navItem{Href: i.href, Label: u.msg(i.key), Current: cur})
	}
	return out
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, name, titleKey string, data any) {
	pg := page{Lang: "en", Title: u.msg(titleKey), Data: data}
	if c != nil {
		pg.Authed, pg.CSRF, pg.Subject = true, c.ss.CSRF, c.p.Subject
		pg.Caps = capsOf(c.p)
		if status < 400 {
			pg.Nav = u.nav(r.URL.Path, c.p)
		} else {
			pg.Nav = u.nav("", c.p) // an error page must not show which section the path belongs to
		}
		pg.Notif = u.NotificationsURL
		pg.Flash = flashMsg(u, r.URL.Query().Get("ok"))
	}
	t, err := u.template(name)
	if err != nil {
		panic("finance ui: template " + name + ": " + err.Error())
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pg); err != nil {
		u.Log.Error("ui render", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type errData struct {
	Heading, Message, RequestID string
	Problems                    []ProblemField
	Authed                      bool
}

func newRequestID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, titleKey, msgKey string) {
	u.renderMsg(w, r, c, status, titleKey, u.msg(msgKey))
}

func (u *UI) renderMsg(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, titleKey, msg string) {
	u.render(w, r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Message: msg, RequestID: newRequestID(), Authed: c != nil})
}

func (u *UI) renderProblems(c *uiCtx, status int, titleKey string, ps []ProblemField) {
	u.render(c.w, c.r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Problems: ps, RequestID: newRequestID(), Authed: true})
}

// ---- login and logout ------------------------------------------------------------------------

func (u *UI) clientHost(r *http.Request) string { return clientIP(r, u.TrustProxy) }

type loginData struct {
	Error      string
	CSRF       string
	TokenLogin bool // the paste-a-token form is offered
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	u.render(w, r, nil, status, "login.html", "login.title", loginData{Error: errMsg, CSRF: u.sessions.LoginCSRF(w, r), TokenLogin: !u.NoTokenLogin})
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.sessions.Current(w, r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.loginForm(w, r, http.StatusOK, "")
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	modclient.LimitPublicForm(w, r) // anyone can post here
	if u.NoTokenLogin {             // ui.token_login: false: launch tickets only; nothing is authenticated or counted
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.token_off"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	if !u.sessions.CheckLoginCSRF(r) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	// Valid credentials are never throttled: authenticate first, and only a failure draws on the
	// per-address budget, so a flood of bad tokens cannot lock out someone with a good one.
	token := strings.TrimSpace(r.PostForm.Get("token"))
	p, err := u.authenticateToken(r, token)
	if err != nil {
		if ok, _ := u.failIP.allow(u.clientHost(r)); !ok {
			w.Header().Set("Retry-After", "10")
			u.loginForm(w, r, http.StatusTooManyRequests, u.msg("login.locked"))
			return
		}
		u.loginForm(w, r, http.StatusUnauthorized, u.msg("login.invalid"))
		return
	}
	u.sessions.Start(w, r, *p, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *UI) logout(c *uiCtx) error {
	u.sessions.End(c.w, c.r)
	http.Redirect(c.w, c.r, "/login", http.StatusSeeOther)
	return nil
}

// ---- static assets ---------------------------------------------------------------------------

func (u *UI) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	b, err := uiAssets.ReadFile("ui/static/" + name)
	if err != nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (u *UI) manifest(w http.ResponseWriter, _ *http.Request) {
	b, _ := uiAssets.ReadFile("ui/static/manifest.webmanifest")
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// ---- small helpers ---------------------------------------------------------------------------

func withOK(path, key string) string { return path + "?ok=" + url.QueryEscape(key) }

func parseID(s string) (int64, error) {
	if s == "" || len(s) > 18 {
		return 0, ErrNotFound
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrNotFound
		}
		n = n*10 + int64(c-'0')
	}
	if n < 1 {
		return 0, ErrNotFound
	}
	return n, nil
}

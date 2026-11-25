package adminui

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// --- login / logout ------------------------------------------------------------------------------

// The login CSRF cookie is __Host- prefixed wherever Secure applies (the browser then refuses a
// cookie set by a sibling subdomain or with another Path); plain HTTP development keeps the
// unprefixed name, since browsers reject __Host- cookies without Secure.
func (s *Server) loginCookieName() string {
	if s.Secure {
		return "__Host-can_login_csrf"
	}
	return "can_login_csrf"
}

type loginData struct {
	Username, Error, LoginCSRF string
}

func (s *Server) loginCookie(tok string) *http.Cookie {
	return &http.Cookie{Name: s.loginCookieName(), Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: 3600}
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, tok, username, errKey string) {
	http.SetCookie(w, s.loginCookie(tok))
	d := loginData{Username: username, LoginCSRF: tok}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "login.html", s.page(r, nil, "", s.msg("login.page_title"), d))
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r, http.StatusOK, randToken(), "", "")
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, nil, http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
		return
	}
	c, err := r.Cookie(s.loginCookieName())
	if err != nil || !equalTokens(c.Value, r.PostFormValue("csrf")) {
		s.renderLogin(w, r, http.StatusForbidden, randToken(), r.PostFormValue("username"), "login.csrf")
		return
	}
	user := r.PostFormValue("username")
	res, err := s.Auth.Login(r.Context(), auth.LoginRequest{Username: user, Password: r.PostFormValue("password"), Code: strings.TrimSpace(r.PostFormValue("totp")), IP: httpx.ClientIP(r, s.TrustProxy)})
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		s.renderLogin(w, r, http.StatusTooManyRequests, c.Value, user, "login.locked")
		return
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		s.renderLogin(w, r, http.StatusServiceUnavailable, c.Value, user, "login.busy")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.renderLogin(w, r, http.StatusUnauthorized, c.Value, user, "login.invalid")
		return
	case err != nil:
		s.internal(w, r, nil, err)
		return
	}
	http.SetCookie(w, auth.SessionCookie(res.Token, res.Session.ExpiresAt, s.Secure))
	cc := s.loginCookie("")
	cc.MaxAge = -1
	http.SetCookie(w, cc)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if err := s.Auth.Logout(r.Context(), sc.Token); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	http.SetCookie(w, auth.ClearCookie(s.Secure))
	s.redirect(w, r, "/login", "flash.logged_out")
}

// --- modules and dashboard --------------------------------------------------------------------

type moduleView struct {
	ID, Name, Version, State, StateLabel, StateClass, LastError, Licence, LicenceClass string
	SourceAvailable                                                                    bool
	RAMMB                                                                              int
}

var stateKeys = map[string]struct{ label, class string }{
	store.StateEnabled:  {"state.enabled", "ok"},
	store.StateFailed:   {"state.failed", "err"},
	store.StateDisabled: {"state.disabled", "neutral"},
}

func (s *Server) moduleView(m store.Module) moduleView {
	st, ok := stateKeys[m.State]
	if !ok {
		st = struct{ label, class string }{"", "neutral"}
	}
	label := m.State
	if st.label != "" {
		label = s.msg(st.label)
	}
	name := m.Manifest.Name
	if name == "" {
		name = m.ID
	}
	lic := m.Manifest.Provider.Licence
	if lic == "" {
		lic = m.Manifest.Provider.LicenceClass
	}
	return moduleView{ID: m.ID, Name: name, Version: m.Version, State: m.State, StateLabel: label, StateClass: st.class, LastError: m.LastError,
		Licence: lic, LicenceClass: m.Manifest.Provider.LicenceClass, SourceAvailable: m.Manifest.Provider.LicenceClass == "source-available", RAMMB: m.Manifest.Resources.RAMMB}
}

func (s *Server) moduleViews(ms []store.Module) (out []moduleView, ramTotal int) {
	for _, m := range ms {
		out = append(out, s.moduleView(m))
		if m.State == store.StateEnabled {
			ramTotal += m.Manifest.Resources.RAMMB
		}
	}
	return out, ramTotal
}

type deliveryRow struct {
	ID                               int64
	Type, Module                     string
	Status, StatusLabel, StatusClass string
	Attempts                         int
	Next                             time.Time
}

type deliveryHealth struct {
	Pending, Delivered, Dead int
	DeadRows                 []deliveryRow
}

var deliveryKeys = map[string]struct{ label, class string }{
	"pending":   {"dstatus.pending", "warn"},
	"delivered": {"dstatus.delivered", "ok"},
	"dead":      {"dstatus.dead", "err"},
}

func (s *Server) deliveryRow(d store.Delivery) deliveryRow {
	k, ok := deliveryKeys[d.Status]
	label, class := d.Status, "neutral"
	if ok {
		label, class = s.msg(k.label), k.class
	}
	return deliveryRow{ID: d.ID, Type: d.Event.Type, Module: d.ModuleID, Status: d.Status, StatusLabel: label, StatusClass: class, Attempts: d.Attempts, Next: d.NextAttemptAt}
}

// health summarises the most recent deliveries.
func (s *Server) health(ds []store.Delivery) (h deliveryHealth, rows []deliveryRow) {
	for _, d := range ds {
		row := s.deliveryRow(d)
		rows = append(rows, row)
		switch d.Status {
		case "pending":
			h.Pending++
		case "delivered":
			h.Delivered++
		case "dead":
			h.Dead++
			if len(h.DeadRows) < 10 {
				h.DeadRows = append(h.DeadRows, row)
			}
		}
	}
	return h, rows
}

type dashboardData struct {
	Company      Company
	Modules      []moduleView
	Capabilities []registry.Resolution
	Health       deliveryHealth
	Audit        []audit.Record
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ctx := r.Context()
	ms, err := s.DB.ListModules(ctx)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	caps, err := s.Registry.List(ctx)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	ds, err := s.DB.ListDeliveries(ctx, 500)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	recent, err := s.Audit.Query(ctx, audit.Filter{Limit: 8})
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := dashboardData{Company: s.Company, Capabilities: caps, Audit: recent}
	d.Modules, _ = s.moduleViews(ms)
	d.Health, _ = s.health(ds)
	s.render(w, r, http.StatusOK, "dashboard.html", s.page(r, sc, "dashboard", s.msg("dash.title"), d))
}

type modulesData struct {
	Modules  []moduleView
	RAMTotal int
}

func (s *Server) modules(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ms, err := s.DB.ListModules(r.Context())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	var d modulesData
	d.Modules, d.RAMTotal = s.moduleViews(ms)
	s.render(w, r, http.StatusOK, "modules.html", s.page(r, sc, "modules", s.msg("mod.title"), d))
}

type provide struct{ Capability, Schema string }

type moduleDetail struct {
	moduleView
	Description, Category, Namespace, ProviderName, ProviderType, Upstream, UpstreamVersion, Database string
	RAMLimitMB, DiskMB                                                                                int
	Provides                                                                                          []provide
	Requires, Consumes, Publishes, Subscribes, Permissions, DataClasses                               []string
}

func (s *Server) module(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	m, err := s.DB.GetModule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, sc, http.StatusNotFound, "err.module_not_found", "err.module_not_found_msg")
		return
	}
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	mf := m.Manifest
	d := moduleDetail{moduleView: s.moduleView(m), Description: mf.Description, Category: mf.Category, Namespace: m.Namespace,
		ProviderName: mf.Provider.Name, ProviderType: mf.Provider.Type, Upstream: mf.Provider.Upstream, UpstreamVersion: mf.Provider.UpstreamVersion,
		Database: mf.Requires.Database, RAMLimitMB: mf.Resources.RAMLimitMB, DiskMB: mf.Resources.DiskMB,
		Requires: mf.Requires.Capabilities, Consumes: mf.Consumes, Permissions: mf.Permissions}
	for _, p := range mf.Provides {
		d.Provides = append(d.Provides, provide{p.Capability, p.Schema})
	}
	for _, p := range mf.Publishes {
		d.Publishes = append(d.Publishes, p.Event)
	}
	for _, p := range mf.Subscribes {
		d.Subscribes = append(d.Subscribes, p.Event)
	}
	for _, c := range mf.DataClasses {
		d.DataClasses = append(d.DataClasses, strings.TrimSpace(fmt.Sprintf("%s %s %s", c.Class, c.Sensitivity, c.Retention)))
	}
	s.render(w, r, http.StatusOK, "module.html", s.page(r, sc, "modules", d.Name, d))
}

// --- audit ------------------------------------------------------------------------------------

type auditFilterView struct{ Actor, Action, Target string }

type verifyView struct {
	Passed  bool
	Entries int
	Head    string
	Kind    string
	Seq     int64
	Message string
}

type auditData struct {
	Filter   auditFilterView
	Records  []audit.Record
	OlderURL string
	Verify   *verifyView
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

func (s *Server) auditData(r *http.Request) (auditData, error) {
	q := r.URL.Query()
	f := auditFilterView{Actor: clip(q.Get("actor"), 64), Action: clip(q.Get("action"), 64), Target: clip(q.Get("target"), 128)}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	if before < 0 {
		before = 0
	}
	recs, err := s.Audit.Query(r.Context(), audit.Filter{Actor: f.Actor, ActionPrefix: f.Action, Target: f.Target, Before: before, Limit: pageSize + 1})
	if err != nil {
		return auditData{}, err
	}
	d := auditData{Filter: f}
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		v := url.Values{}
		for k, val := range map[string]string{"actor": f.Actor, "action": f.Action, "target": f.Target} {
			if val != "" {
				v.Set(k, val)
			}
		}
		v.Set("before", strconv.FormatInt(recs[len(recs)-1].Seq, 10))
		d.OlderURL = "/audit?" + v.Encode()
	}
	d.Records = recs
	return d, nil
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, err := s.auditData(r)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.render(w, r, http.StatusOK, "audit.html", s.page(r, sc, "audit", s.msg("audit.title"), d))
}

// busy takes the single-flight lock of the whole-log operations; false means one is running.
func (s *Server) busy(w http.ResponseWriter, r *http.Request, sc *sessionCtx) bool {
	if s.heavy.TryLock() {
		return false
	}
	w.Header().Set("Retry-After", "5")
	s.fail(w, r, sc, http.StatusServiceUnavailable, "err.busy", "err.busy_msg")
	return true
}

func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.busy(w, r, sc) {
		return
	}
	defer s.heavy.Unlock()
	res, err := s.Audit.Verify(r.Context(), audit.VerifyOptions{})
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "audit.verify", Target: "audit", Detail: map[string]any{"ok": res.OK(), "entries": res.Entries}}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d, err := s.auditData(r) // the entries next to the result
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	v := &verifyView{Passed: res.OK(), Entries: res.Entries, Head: res.Head.String()}
	if res.Violation != nil {
		v.Kind, v.Seq, v.Message = res.Violation.Kind, res.Violation.Seq, res.Violation.Message
	}
	d.Verify = v
	s.render(w, r, http.StatusOK, "audit.html", s.page(r, sc, "audit", s.msg("audit.title"), d))
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.busy(w, r, sc) {
		return
	}
	defer s.heavy.Unlock()
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "audit.export", Target: "audit"}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-`+s.Now().UTC().Format("20060102-150405")+`.jsonl"`)
	if err := s.Audit.Export(r.Context(), w); err != nil {
		s.Log.Error("audit export failed", "request_id", httpx.RequestID(r.Context()), "error", err)
	}
}

// --- events -----------------------------------------------------------------------------------

type eventsData struct {
	Health deliveryHealth
	Rows   []deliveryRow
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ds, err := s.DB.ListDeliveries(r.Context(), 500)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	var d eventsData
	var rows []deliveryRow
	d.Health, rows = s.health(ds)
	if len(rows) > 100 {
		rows = rows[:100]
	}
	d.Rows = rows
	s.render(w, r, http.StatusOK, "events.html", s.page(r, sc, "events", s.msg("ev.title"), d))
}

type attemptRow struct {
	Number     int
	At         time.Time
	StatusCode int
	Error      string
	DurationMS int64
}

type eventDetail struct {
	ID                            int64
	EventID, Type, Source, Module string
	Time, Next                    time.Time
	AttemptCount                  int
	StatusLabel, StatusClass      string
	Dead                          bool
	Log                           []attemptRow
}

func (s *Server) event(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.event_not_found", "err.event_not_found_msg")
		return
	}
	dv, err := s.DB.GetDelivery(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, sc, http.StatusNotFound, "err.event_not_found", "err.event_not_found_msg")
		return
	}
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	as, err := s.DB.Attempts(r.Context(), id)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	row := s.deliveryRow(dv)
	d := eventDetail{ID: dv.ID, EventID: dv.Event.ID, Type: dv.Event.Type, Source: dv.Event.Source, Module: dv.ModuleID, Time: dv.Event.Time, Next: dv.NextAttemptAt,
		AttemptCount: dv.Attempts, StatusLabel: row.StatusLabel, StatusClass: row.StatusClass, Dead: dv.Status == "dead"}
	for _, a := range as {
		d.Log = append(d.Log, attemptRow{Number: a.Number, At: a.At, StatusCode: a.StatusCode, Error: a.Error, DurationMS: a.Duration.Milliseconds()})
	}
	s.render(w, r, http.StatusOK, "event.html", s.page(r, sc, "events", s.msg("ev.delivery", dv.ID), d))
}

func (s *Server) requeue(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.event_not_found", "err.event_not_found_msg")
		return
	}
	err = s.DB.InTx(r.Context(), func(tx *sql.Tx) error {
		if err := store.RequeueDeliveryTx(r.Context(), tx, id, s.Now()); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(r.Context(), tx, audit.Entry{Actor: sc.User.Username, Action: "delivery.requeue", Target: strconv.FormatInt(id, 10)})
		return err // a failed audit write rolls the requeue back
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.event_not_found", "err.event_not_found_msg")
		return
	case errors.Is(err, store.ErrNotDead):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.not_dead")
		return
	case err != nil:
		s.internal(w, r, sc, err)
		return
	}
	next := "/events"
	switch n := r.PostFormValue("next"); n {
	case "/":
		next = "/"
	case "/events/" + strconv.FormatInt(id, 10):
		next = n
	}
	s.redirect(w, r, next, "flash.requeued")
}

// --- leads ------------------------------------------------------------------------------------

type statusOption struct {
	Value, Label string
	Selected     bool
}

var leadStatusKeys = map[string]string{
	"new": "lstatus.new", "contacted": "lstatus.contacted", "qualified": "lstatus.qualified", "closed": "lstatus.closed", "spam": "lstatus.spam",
}

func (s *Server) leadStatusLabel(v string) string {
	if k, ok := leadStatusKeys[v]; ok {
		return s.msg(k)
	}
	return v
}

func (s *Server) statusOptions(selected string) []statusOption {
	var out []statusOption
	for _, v := range LeadStatuses {
		out = append(out, statusOption{Value: v, Label: s.leadStatusLabel(v), Selected: v == selected})
	}
	return out
}

type leadRow struct {
	Lead
	StatusLabel string
}

type leadsData struct {
	Unavailable bool
	Status      string
	Statuses    []statusOption
	Items       []leadRow
	NextURL     string
}

var cursorRe = regexp.MustCompile(`^[0-9]{1,18}$`)

// leadsFailure logs a gateway failure and shows the generic 502 page (never the error text).
func (s *Server) leadsFailure(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	s.Log.Error("leads request failed", "request_id", httpx.RequestID(r.Context()), "error", err)
	s.fail(w, r, sc, http.StatusBadGateway, "err.gateway", "err.gateway_msg")
}

func (s *Server) leads(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	title := s.msg("lead.title")
	if s.Leads == nil {
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", title, leadsData{Unavailable: true}))
		return
	}
	status := r.URL.Query().Get("status")
	if !validStatus(status) {
		status = ""
	}
	cursor := r.URL.Query().Get("cursor")
	if !cursorRe.MatchString(cursor) {
		cursor = ""
	}
	page, err := s.Leads.List(r.Context(), LeadQuery{Status: status, Cursor: cursor, Limit: pageSize})
	if errors.Is(err, ErrLeadsUnavailable) {
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", title, leadsData{Unavailable: true}))
		return
	}
	if err != nil {
		s.leadsFailure(w, r, sc, err)
		return
	}
	d := leadsData{Status: status, Statuses: s.statusOptions(status)}
	for _, l := range page.Items {
		d.Items = append(d.Items, leadRow{Lead: l, StatusLabel: s.leadStatusLabel(l.Status)})
	}
	if page.NextCursor != "" && cursorRe.MatchString(page.NextCursor) {
		v := url.Values{"cursor": {page.NextCursor}}
		if status != "" {
			v.Set("status", status)
		}
		d.NextURL = "/leads?" + v.Encode()
	}
	s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", title, d))
}

type kv struct{ Key, Value string }

type leadDetail struct {
	Lead        Lead
	StatusLabel string
	Fields      []kv
	Statuses    []statusOption
}

func fieldText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *Server) lead(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Leads == nil {
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", s.msg("lead.title"), leadsData{Unavailable: true}))
		return
	}
	l, err := s.Leads.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrLeadNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.lead_not_found", "err.lead_not_found_msg")
		return
	case errors.Is(err, ErrLeadsUnavailable):
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", s.msg("lead.title"), leadsData{Unavailable: true}))
		return
	case err != nil:
		s.leadsFailure(w, r, sc, err)
		return
	}
	d := leadDetail{Lead: l, StatusLabel: s.leadStatusLabel(l.Status), Statuses: s.statusOptions(l.Status)}
	keys := make([]string, 0, len(l.Fields))
	for k := range l.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.Fields = append(d.Fields, kv{k, fieldText(l.Fields[k])})
	}
	s.render(w, r, http.StatusOK, "lead.html", s.page(r, sc, "leads", s.msg("lead.detail", l.ID), d))
}

func (s *Server) leadStatus(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	status := r.PostFormValue("status")
	if !validStatus(status) {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.status_invalid")
		return
	}
	if s.Leads == nil {
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", s.msg("lead.title"), leadsData{Unavailable: true}))
		return
	}
	id := r.PathValue("id")
	err := s.Leads.SetStatus(r.Context(), id, status)
	switch {
	case errors.Is(err, ErrLeadNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.lead_not_found", "err.lead_not_found_msg")
		return
	case errors.Is(err, ErrLeadsUnavailable):
		s.render(w, r, http.StatusOK, "leads.html", s.page(r, sc, "leads", s.msg("lead.title"), leadsData{Unavailable: true}))
		return
	case err != nil:
		s.leadsFailure(w, r, sc, err)
		return
	}
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "lead.status", Target: id, Detail: map[string]any{"status": status}}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/leads/"+url.PathEscape(id), "flash.lead_status")
}

// --- settings ---------------------------------------------------------------------------------

type sessionRow struct {
	ID, IP           string
	Created, Expires time.Time
	Current          bool
}

type settingsData struct {
	Error    string
	Sessions []sessionRow
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, errKey string) {
	ss, err := s.DB.ListSessions(r.Context(), sc.User.ID, s.Now())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := settingsData{}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	for _, x := range ss {
		d.Sessions = append(d.Sessions, sessionRow{ID: store.PublicSessionID(x), IP: x.IP, Created: x.CreatedAt, Expires: x.ExpiresAt, Current: x.IDHash == sc.Session.IDHash})
	}
	s.render(w, r, status, "settings.html", s.page(r, sc, "settings", s.msg("settings.title"), d))
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderSettings(w, r, sc, http.StatusOK, "")
}

// accountError maps an auth error of an account operation to a status and message key; ok is false
// for errors that are not the caller's fault.
func accountError(w http.ResponseWriter, err error) (status int, key string, ok bool) {
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		return http.StatusTooManyRequests, "settings.locked", true
	case errors.Is(err, auth.ErrInvalidCredentials):
		return http.StatusUnauthorized, "settings.wrong_current", true
	case errors.Is(err, auth.ErrWeakPassword):
		return http.StatusBadRequest, "settings.weak", true
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		return http.StatusServiceUnavailable, "login.busy", true
	}
	return 0, "", false
}

// endSessions clears the cookie after an operation that destroyed every session.
func (s *Server) endSessions(w http.ResponseWriter, r *http.Request, flash string) {
	http.SetCookie(w, auth.ClearCookie(s.Secure))
	s.redirect(w, r, "/login", flash)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	next := r.PostFormValue("new")
	if next != r.PostFormValue("confirm") {
		s.renderSettings(w, r, sc, http.StatusBadRequest, "settings.mismatch")
		return
	}
	if len(next) < auth.MinPasswordLen {
		s.renderSettings(w, r, sc, http.StatusBadRequest, "settings.weak")
		return
	}
	err := s.Auth.ChangePassword(r.Context(), sc.User, r.PostFormValue("current"), strings.TrimSpace(r.PostFormValue("otp")), next, httpx.ClientIP(r, s.TrustProxy))
	if err != nil {
		if status, key, ok := accountError(w, err); ok {
			s.renderSettings(w, r, sc, status, key)
			return
		}
		s.internal(w, r, sc, err)
		return
	}
	s.endSessions(w, r, "flash.password_changed")
}

type totpData struct {
	Error, Secret, URI string
}

var secretRe = regexp.MustCompile(`^[A-Z2-7]{32}$`)

func (s *Server) renderTOTP(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, secret, errKey string) {
	d := totpData{Secret: secret, URI: auth.TOTPURI(s.msg("app.name"), sc.User.Username, secret)}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "totp.html", s.page(r, sc, "settings", s.msg("settings.totp_new"), d))
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if err := s.Auth.ConfirmPassword(r.Context(), sc.User, r.PostFormValue("current")); err != nil {
		if status, key, ok := accountError(w, err); ok {
			s.renderSettings(w, r, sc, status, key)
			return
		}
		s.internal(w, r, sc, err)
		return
	}
	s.renderTOTP(w, r, sc, http.StatusOK, auth.NewTOTPSecret(), "")
}

func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	secret := r.PostFormValue("secret")
	if !secretRe.MatchString(secret) {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
		return
	}
	err := s.Auth.RotateTOTP(r.Context(), sc.User, r.PostFormValue("current"), strings.TrimSpace(r.PostFormValue("otp")), secret, strings.TrimSpace(r.PostFormValue("code")), httpx.ClientIP(r, s.TrustProxy))
	switch {
	case err == nil:
		s.endSessions(w, r, "flash.totp_rotated")
		return
	case errors.Is(err, auth.ErrBadTOTPCode):
		s.renderTOTP(w, r, sc, http.StatusBadRequest, secret, "settings.bad_code")
		return
	}
	if status, key, ok := accountError(w, err); ok {
		s.renderTOTP(w, r, sc, status, secret, key)
		return
	}
	s.internal(w, r, sc, err)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ok, err := s.DB.RevokeSession(r.Context(), sc.User.ID, r.PathValue("sid"))
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	if !ok {
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "admin.session.revoke", Target: "admin", Detail: map[string]any{"session": r.PathValue("sid")}}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/settings", "flash.session_revoked")
}

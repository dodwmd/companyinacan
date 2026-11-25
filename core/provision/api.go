package provision

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/obs"
)

// Permissions of the provisioning API (modules/provisioning/module.yaml). write includes read and
// admin includes both; the SCIM server has its own scoped tokens (scim.read, scim.write).
const (
	PermRead  = "provision.read"
	PermWrite = "provision.write"
	PermAdmin = "provision.admin"
)

// Principal is an authenticated caller of the provisioning API.
type Principal struct {
	Subject string
	Perms   []string
}

func (p Principal) has(perm string) bool {
	if slices.Contains(p.Perms, PermAdmin) {
		return true
	}
	if perm == PermRead && slices.Contains(p.Perms, PermWrite) {
		return true
	}
	return slices.Contains(p.Perms, perm)
}

// Authenticator resolves a bearer token. A nil principal with a nil error means "not valid".
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// StaticToken authenticates the service token configured for can-core and operators.
type StaticToken struct{ Token string }

// Authenticate implements Authenticator.
func (s StaticToken) Authenticate(_ context.Context, tok string) (*Principal, error) {
	if len(s.Token) >= 24 && subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1 {
		return &Principal{Subject: "svc:api-token", Perms: []string{PermAdmin}}, nil
	}
	return nil, nil
}

// CanAuth authenticates the personal access tokens can-core issues, by introspection. Any doubt
// is "not valid"; an introspection outage is an error (503), never an accepted token.
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(ctx context.Context, tok string) (*Principal, error) {
	if !modclient.TokenShaped(tok) {
		return nil, nil
	}
	id, err := a.In.Lookup(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("%w: token introspection", ErrUnavailable)
	}
	if !id.Active || id.Kind != "user" || !subjectRe.MatchString(id.Subject) {
		return nil, nil
	}
	var perms []string
	for _, p := range id.Permissions {
		if p == PermRead || p == PermWrite || p == PermAdmin {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return nil, nil
	}
	return &Principal{Subject: id.Subject, Perms: perms}, nil
}

// MultiAuth tries each authenticator in turn.
type MultiAuth []Authenticator

// Authenticate implements Authenticator.
func (m MultiAuth) Authenticate(ctx context.Context, tok string) (*Principal, error) {
	var firstErr error
	for _, a := range m {
		p, err := a.Authenticate(ctx, tok)
		if p != nil {
			return p, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// API is the provisioning HTTP API.
type API struct {
	Eng   *Engine
	Auth  Authenticator
	SCIM  *SCIM
	Can   http.Handler // can-core's signed event endpoint, nil standalone
	Log   *slog.Logger
	Names []string // connector names, for listing
	// ConnInfo describes each connector.
	ConnInfo func() []ConnectorInfo

	routes []string
}

// ConnectorInfo is what GET /v1/provision/connectors shows: no URLs, no credentials.
type ConnectorInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Verified   bool   `json:"verified"`
	Privileged bool   `json:"privileged"`
}

const maxAPIBody = 64 << 10

type apiFn func(w http.ResponseWriter, r *http.Request, p *Principal) error

func (a *API) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) guard(perm string, fn apiFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="provision"`)
			problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		p, err := a.Auth.Authenticate(r.Context(), tok)
		if err != nil {
			a.log().Error("provision: authentication unavailable", "error", err)
			problem(w, http.StatusServiceUnavailable, "authentication is unavailable")
			return
		}
		if p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="provision", error="invalid_token"`)
			problem(w, http.StatusUnauthorized, "the token is not valid")
			return
		}
		if !p.has(perm) {
			problem(w, http.StatusForbidden, "this needs the "+perm+" permission")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
		}
		if err := fn(w, r, p); err != nil {
			a.fail(w, r, err)
		}
	}
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		problem(w, http.StatusRequestEntityTooLarge, "the request body is too large")
	case errors.Is(err, ErrNotFound):
		problem(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrInvalid):
		problem(w, http.StatusBadRequest, publicMsg(err))
	case errors.Is(err, ErrConflict):
		problem(w, http.StatusConflict, publicMsg(err))
	case errors.Is(err, ErrForbidden):
		problem(w, http.StatusForbidden, publicMsg(err))
	case errors.Is(err, ErrUnavailable):
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusServiceUnavailable, "a dependency is unavailable")
	default:
		a.log().Error("provision: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		problem(w, http.StatusInternalServerError, "internal error")
	}
}

// publicMsg is the part of a wrapped sentinel error that is safe to show (messages are written
// by this package and carry no personal data or secrets).
func publicMsg(err error) string {
	s := err.Error()
	for _, sent := range []error{ErrInvalid, ErrConflict, ErrForbidden} {
		s = strings.TrimPrefix(s, sent.Error()+": ")
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func readJSON(r *http.Request, v any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return fmt.Errorf("%w: Content-Type must be application/json", ErrInvalid)
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return fmt.Errorf("%w: the body is not valid for this request", ErrInvalid)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON body", ErrInvalid)
	}
	return nil
}

// Handler returns the full service handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes = nil
	route := func(pattern string, h http.HandlerFunc) {
		a.routes = append(a.routes, pattern)
		mux.HandleFunc(pattern, h)
	}
	route("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if a.Can != nil {
		a.routes = append(a.routes, "POST /v1/can/events")
		mux.Handle("POST /v1/can/events", a.Can)
	}
	if a.SCIM != nil {
		mux.Handle(scimBase+"/", a.SCIM.Handler())
		a.routes = append(a.routes, a.SCIM.routes...)
	}
	route("GET /v1/provision/connectors", a.guard(PermRead, a.connectors))
	route("GET /v1/provision/runs", a.guard(PermRead, a.listRuns))
	route("GET /v1/provision/runs/{id}", a.guard(PermRead, a.getRun))
	route("GET /v1/provision/dead-letters", a.guard(PermRead, a.deadLetters))
	route("POST /v1/provision/plans", a.guard(PermRead, a.plan))
	route("POST /v1/provision/runs", a.guard(PermWrite, a.startRun))
	route("POST /v1/provision/runs/{id}/retry", a.guard(PermWrite, a.retryRun))
	route("POST /v1/provision/runs/{id}/cancel", a.guard(PermAdmin, a.cancelRun))
	route("POST /v1/provision/people/{id}/urgent", a.guard(PermAdmin, a.urgent))
	route("PUT /v1/provision/people/{id}/accounts/{connector}", a.guard(PermAdmin, a.linkAccount))
	route("GET /v1/provision/scim-tokens", a.guard(PermAdmin, a.listTokens))
	route("POST /v1/provision/scim-tokens", a.guard(PermAdmin, a.createToken))
	route("DELETE /v1/provision/scim-tokens/{id}", a.guard(PermAdmin, a.revokeToken))
	route("GET /v1/provision/audit", a.guard(PermAdmin, a.audit))
	return obs.Instrument(securityHeaders(mux)) // request metrics and /readyz (epic E22)
}

// Routes lists the "METHOD /path" patterns the service serves (with SCIM and the can-core event
// endpoint enabled); a contract test compares them to the OpenAPI document.
func Routes() []string {
	a := &API{SCIM: &SCIM{}, Can: http.NotFoundHandler()}
	a.Handler()
	return a.routes
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (a *API) connectors(w http.ResponseWriter, _ *http.Request, _ *Principal) error {
	var out []ConnectorInfo
	if a.ConnInfo != nil {
		out = a.ConnInfo()
	}
	if out == nil {
		out = []ConnectorInfo{}
	}
	writeJSON(w, 200, map[string]any{"items": out})
	return nil
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := RunFilter{State: q.Get("state"), PartyID: q.Get("party_id"), Kind: q.Get("kind"), After: q.Get("cursor"), Limit: limit}
	if (f.PartyID != "" && !partyRe.MatchString(f.PartyID)) || (f.Kind != "" && !Kind(f.Kind).Valid()) || (f.After != "" && !runRe.MatchString(f.After)) {
		return fmt.Errorf("%w: invalid filter", ErrInvalid)
	}
	runs, err := a.Eng.Store.ListRuns(r.Context(), f)
	if err != nil {
		return err
	}
	if runs == nil {
		runs = []Run{}
	}
	out := map[string]any{"items": runs}
	if n := len(runs); n > 0 && n == f.limitOrDefault() {
		out["next_cursor"] = runs[n-1].ID
	}
	writeJSON(w, 200, out)
	return nil
}

func (f RunFilter) limitOrDefault() int {
	if f.Limit <= 0 || f.Limit > 200 {
		return 50
	}
	return f.Limit
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	if !runRe.MatchString(r.PathValue("id")) {
		return ErrNotFound
	}
	run, err := a.Eng.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, run)
	return nil
}

func (a *API) deadLetters(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	runs, err := a.Eng.Store.DeadLetters(r.Context(), 100)
	if err != nil {
		return err
	}
	if runs == nil {
		runs = []Run{}
	}
	writeJSON(w, 200, map[string]any{"items": runs})
	return nil
}

type runRequest struct {
	Kind          string `json:"kind"`
	PartyID       string `json:"party_id"`
	DryRun        bool   `json:"dry_run"`
	EffectiveDate string `json:"effective_date"`
}

func (rr runRequest) valid() error {
	if !Kind(rr.Kind).Valid() || !partyRe.MatchString(rr.PartyID) || (rr.EffectiveDate != "" && !dateRe.MatchString(rr.EffectiveDate)) {
		return fmt.Errorf("%w: kind (joiner, mover or leaver), party_id and an optional effective_date (YYYY-MM-DD) are required", ErrInvalid)
	}
	if rr.EffectiveDate != "" {
		if _, err := time.Parse("2006-01-02", rr.EffectiveDate); err != nil {
			return fmt.Errorf("%w: effective_date is not a date", ErrInvalid)
		}
	}
	return nil
}

func (a *API) plan(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	var rr runRequest
	if err := readJSON(r, &rr); err != nil {
		return err
	}
	if err := rr.valid(); err != nil {
		return err
	}
	res, err := a.Eng.Plan(r.Context(), StartRequest{Kind: Kind(rr.Kind), PartyID: rr.PartyID, EffectiveDate: rr.EffectiveDate})
	if err != nil {
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

func (a *API) startRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	key := r.Header.Get("Idempotency-Key")
	if !keyRe.MatchString(key) {
		return fmt.Errorf("%w: an Idempotency-Key header (1 to 128 letters, digits and _ . : -) is required", ErrInvalid)
	}
	var rr runRequest
	if err := readJSON(r, &rr); err != nil {
		return err
	}
	if err := rr.valid(); err != nil {
		return err
	}
	run, created, err := a.Eng.Start(r.Context(), StartRequest{Kind: Kind(rr.Kind), PartyID: rr.PartyID, Trigger: "api", Key: key, Initiator: p.Subject, DryRun: rr.DryRun, EffectiveDate: rr.EffectiveDate})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, run)
	return nil
}

func (a *API) retryRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !runRe.MatchString(r.PathValue("id")) {
		return ErrNotFound
	}
	if err := a.Eng.RetryRun(r.Context(), p.Subject, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !runRe.MatchString(r.PathValue("id")) {
		return ErrNotFound
	}
	if err := a.Eng.CancelRun(r.Context(), p.Subject, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) urgent(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !partyRe.MatchString(r.PathValue("id")) {
		return ErrNotFound
	}
	if err := a.Eng.FlagUrgent(r.Context(), p.Subject, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (a *API) linkAccount(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		ExternalID string `json:"external_id"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	party, conn := r.PathValue("id"), r.PathValue("connector")
	if !partyRe.MatchString(party) || !extIDRe.MatchString(in.ExternalID) {
		return fmt.Errorf("%w: person id and external_id", ErrInvalid)
	}
	if _, ok := a.Eng.Conns.Get(conn); !ok {
		return ErrNotFound
	}
	if _, err := a.Eng.Dir.Get(r.Context(), party); err != nil {
		return err
	}
	if err := a.Eng.Store.LinkAccount(r.Context(), p.Subject, party, conn, in.ExternalID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	ts, err := a.Eng.Store.ListSCIMTokens(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"items": ts})
	return nil
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn string   `json:"expires_in"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ttl, err := time.ParseDuration(in.ExpiresIn)
	if err != nil {
		return fmt.Errorf("%w: expires_in must be a duration such as 720h", ErrInvalid)
	}
	rec, tok, err := a.Eng.Store.CreateSCIMToken(r.Context(), p.Subject, in.Name, in.Scopes, ttl)
	if err != nil {
		return err
	}
	// the only time the token value is ever shown
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "id": rec.ID, "name": rec.Name, "scopes": rec.Scopes, "expires_at": rec.ExpiresAt})
	return nil
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := a.Eng.Store.RevokeSCIMToken(r.Context(), p.Subject, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) audit(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := a.Eng.Store.AuditList(r.Context(), after, limit)
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []AuditRecord{}
	}
	writeJSON(w, 200, map[string]any{"items": recs})
	return nil
}

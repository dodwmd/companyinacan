package forms

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	exportLimit = 10000
	maxNoteLen  = 2000
)

// leadsAPI implements the `leads` capability (schemas/capabilities/leads.v1). It is served
// on its own listener (a ClusterIP Service only) and is never reachable through /_forms.
type leadsAPI struct {
	store *Store
	auth  Authenticator
	log   *slog.Logger
	// notifier is told about erasures: it stops what is being sent and announces them.
	notifier *Dispatcher
}

func (a *leadsAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/leads", a.guard(PermLeadsRead, a.list))
	mux.HandleFunc("GET /v1/leads/export.csv", a.guard(PermLeadsRead, a.export))
	mux.HandleFunc("GET /v1/leads/{id}", a.guard(PermLeadsRead, a.get))
	mux.HandleFunc("PATCH /v1/leads/{id}", a.guard(PermLeadsWrite, a.patch))
	mux.HandleFunc("POST /v1/leads/find", a.guardAs(PermLeadsErase, a.find))
	mux.HandleFunc("DELETE /v1/leads/{id}", a.guardAs(PermLeadsErase, a.erase))
	mux.HandleFunc("GET /v1/erasures", a.guardAs(PermLeadsErase, a.erasures))
	mux.HandleFunc("POST /v1/purge", a.guardAs(PermLeadsErase, a.purge))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		mux.ServeHTTP(w, r)
	})
}

func (a *leadsAPI) guard(perm string, next http.HandlerFunc) http.HandlerFunc {
	return a.guardAs(perm, func(w http.ResponseWriter, r *http.Request, _ *Principal) { next(w, r) })
}

// guardAs is guard for handlers that record who acted.
func (a *leadsAPI) guardAs(perm string, next func(http.ResponseWriter, *http.Request, *Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-forms"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		if !p.Can(perm) {
			if perm == PermLeadsErase {
				a.log.Warn("audit", "action", "lead.erase_denied", "actor", p.Subject)
			}
			writeError(w, http.StatusForbidden, "forbidden", "missing permission "+perm)
			return
		}
		next(w, r, p)
	}
}

func (a *leadsAPI) filter(r *http.Request) (ListFilter, string) {
	q := r.URL.Query()
	f := ListFilter{FormID: q.Get("form"), Status: q.Get("status"), Limit: 50}
	if f.FormID != "" && !idRe.MatchString(f.FormID) {
		return f, "form is not a valid form id"
	}
	if f.Status != "" && !ValidStatus(f.Status) {
		return f, "status is not a known status"
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return f, "limit must be 1..1000"
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return f, "cursor is invalid"
		}
		f.BeforeSeq = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "since must be an RFC 3339 timestamp"
		}
		f.Since = t
	}
	return f, ""
}

func (a *leadsAPI) internal(w http.ResponseWriter, err error) {
	a.log.Error("leads api", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func (a *leadsAPI) list(w http.ResponseWriter, r *http.Request) {
	f, bad := a.filter(r)
	if bad != "" {
		writeError(w, http.StatusBadRequest, "bad_request", bad)
		return
	}
	want := f.Limit
	f.Limit = want + 1 // one extra row tells us whether another page exists
	items, err := a.store.List(r.Context(), f)
	if err != nil {
		a.internal(w, err)
		return
	}
	out := struct {
		Items      []Lead `json:"items"`
		NextCursor string `json:"next_cursor,omitempty"`
	}{Items: items}
	if len(items) > want {
		out.Items = items[:want]
		out.NextCursor = strconv.FormatInt(out.Items[want-1].Seq, 10)
	}
	if out.Items == nil {
		out.Items = []Lead{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *leadsAPI) get(w http.ResponseWriter, r *http.Request) {
	l, err := a.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such lead")
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	ds, err := a.store.Deliveries(r.Context(), l.ID)
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*Lead
		Deliveries []Delivery `json:"deliveries"`
	}{l, nonNil(ds)})
}

func nonNil(d []Delivery) []Delivery {
	if d == nil {
		return []Delivery{}
	}
	return d
}

func (a *leadsAPI) patch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status *string `json:"status"`
		Note   *string `json:"note"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be a JSON object with status and/or note")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "unexpected data after JSON object")
		return
	}
	if in.Status == nil && in.Note == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "provide status and/or note")
		return
	}
	status := ""
	if in.Status != nil {
		status = *in.Status
		if !ValidStatus(status) {
			writeError(w, http.StatusBadRequest, "bad_request", "status is not a known status")
			return
		}
	}
	if in.Note != nil && (utf8.RuneCountInString(*in.Note) > maxNoteLen || !utf8.ValidString(*in.Note) || hasControl(*in.Note)) {
		writeError(w, http.StatusBadRequest, "bad_request", "note is too long or contains control characters")
		return
	}
	l, err := a.store.Update(r.Context(), r.PathValue("id"), status, in.Note)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such lead")
		return
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (a *leadsAPI) export(w http.ResponseWriter, r *http.Request) {
	f, bad := a.filter(r)
	if bad != "" {
		writeError(w, http.StatusBadRequest, "bad_request", bad)
		return
	}
	f.Limit = exportLimit + 1
	f.BeforeSeq = 0
	items, err := a.store.List(r.Context(), f)
	if err != nil {
		a.internal(w, err)
		return
	}
	if len(items) > exportLimit {
		items = items[:exportLimit]
		w.Header().Set("X-Export-Truncated", "true")
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="leads.csv"`)
	if err := WriteCSV(w, items); err != nil {
		a.log.Error("export csv", "error", err)
	}
}

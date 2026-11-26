package people

// The identity lookup: the narrowest credential of the directory, for a module that only has to
// know which person an account is and whether a person exists (equity,
// specs/056-module-capability-wiring). It answers two questions and nothing else: no names, no
// list, no write.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

type lookupRecord struct {
	ID         string `json:"id"`
	SSOSubject string `json:"sso_subject,omitempty"`
	Erased     bool   `json:"erased,omitempty"`
}

// AddLookupToken admits equity's credential. It has no role, so the policy layer denies it
// everything; only serveLookup answers it.
func AddLookupToken(st *StaticTokens, token string) {
	st.Add(token, Principal{Subject: "svc-equity", Lookup: true})
}

const (
	lookupBySubject = `SELECT id, COALESCE(sso_subject,''), erased_at FROM people WHERE sso_subject = ?`
	lookupByID      = `SELECT id, COALESCE(sso_subject,''), erased_at FROM people WHERE id = ?`
)

// lookup finds one record with one of the two queries above.
func (s *Service) lookup(ctx context.Context, query, value string) (*lookupRecord, error) {
	var r lookupRecord
	var erased sql.NullInt64
	err := s.db.QueryRowContext(ctx, query, value).Scan(&r.ID, &r.SSOSubject, &erased)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Erased = erased.Valid && erased.Int64 != 0
	return &r, nil
}

// serveLookup answers a lookup credential: the account's person (an exact subject is needed, so the
// directory cannot be listed) and whether an id is a person. Every other route is forbidden.
func (a *API) serveLookup(w http.ResponseWriter, r *http.Request) {
	switch r.Pattern {
	case "GET /v1/people":
		q := r.URL.Query()
		subject := q.Get("sso_subject")
		q.Del("sso_subject")
		q.Del("limit")
		if !subjectRe.MatchString(subject) || len(q) != 0 {
			break
		}
		items := []lookupRecord{}
		p, err := a.Svc.lookup(r.Context(), lookupBySubject, subject)
		switch {
		case err == nil:
			items = append(items, *p)
		case !errors.Is(err, ErrNotFound):
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	case "GET /v1/people/{id}":
		id := r.PathValue("id")
		if !personIDRe.MatchString(id) {
			a.fail(w, ErrNotFound)
			return
		}
		p, err := a.Svc.lookup(r.Context(), lookupByID, id)
		if err != nil {
			a.fail(w, err)
			return
		}
		p.SSOSubject = "" // asked about a person, not about an account
		writeJSON(w, http.StatusOK, p)
		return
	}
	a.fail(w, ErrForbidden)
}

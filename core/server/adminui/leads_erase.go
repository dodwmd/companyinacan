package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
)

// Erasure of leads (specs/054-lead-erasure): find the submissions that name an address and erase
// them one by one. It needs leads.erase (the forms module's permission; owners hold it) and, to
// erase, a recent proof of identity. The address is only ever posted in a form body, and neither
// can-core's log nor its audit trail records it.

// PermLeadsErase is the forms module's permission to search by address and to erase.
const PermLeadsErase = "leads.erase"

var (
	// ErrLeadsForbidden is a refusal of can-core's credential by the provider.
	ErrLeadsForbidden = errors.New("the leads credential may not erase")
	// ErrLeadScrub is an erasure that happened but left traces in the database files.
	ErrLeadScrub = errors.New("the lead is erased, but scrubbing the database files failed")
)

// LeadMatch is one result of a search by address: ids only.
type LeadMatch struct {
	ID        string    `json:"id"`
	FormID    string    `json:"form_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Match     string    `json:"match"`
}

// LeadEraser is the erasure part of the leads client. HTTPLeads implements it.
type LeadEraser interface {
	Find(ctx context.Context, email string) (items []LeadMatch, truncated bool, err error)
	Erase(ctx context.Context, id string) error
}

// Find implements LeadEraser.
func (c *HTTPLeads) Find(ctx context.Context, email string) ([]LeadMatch, bool, error) {
	var out struct {
		Items     []LeadMatch `json:"items"`
		Truncated bool        `json:"truncated"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/leads/find", nil, map[string]string{"email": email}, &out)
	return out.Items, out.Truncated, err
}

// Erase implements LeadEraser.
func (c *HTTPLeads) Erase(ctx context.Context, id string) error {
	if !leadID.MatchString(id) {
		return ErrLeadNotFound
	}
	return c.do(ctx, http.MethodDelete, "/v1/leads/"+id, nil, nil, nil)
}

func (s *Server) leadEraseRoutes() []route {
	return []route{
		{"GET /lead-erasure", s.authedPerm(PermLeadsErase, s.leadSearchPage)},
		{"POST /lead-erasure", s.authedPerm(PermLeadsErase, s.leadSearch)},
		{"GET /leads/{id}/erase", s.authedPerm(PermLeadsErase, s.leadErasePage)},
		{"POST /leads/{id}/erase", s.authedPerm(PermLeadsErase, s.leadErase)},
	}
}

type leadSearchData struct {
	Unavailable, Searched, Truncated bool
	Error                            string
	Items                            []leadMatchRow
}

type leadMatchRow struct {
	LeadMatch
	StatusLabel string
	Mention     bool
}

func (s *Server) renderSearch(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, d leadSearchData) {
	s.render(w, r, status, "lead_search.html", s.page(r, sc, "leads", s.msg("le.title"), d))
}

func (s *Server) leadSearchPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	_, ok := s.Leads.(LeadEraser)
	s.renderSearch(w, r, sc, http.StatusOK, leadSearchData{Unavailable: !ok})
}

// eraseFailed answers the errors the search and the erasure share.
func (s *Server) eraseFailed(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	switch {
	case errors.Is(err, ErrLeadNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.lead_not_found", "err.lead_not_found_msg")
	case errors.Is(err, ErrLeadsUnavailable):
		s.renderSearch(w, r, sc, http.StatusOK, leadSearchData{Unavailable: true})
	case errors.Is(err, ErrLeadsForbidden):
		s.fail(w, r, sc, http.StatusBadGateway, "err.gateway", "le.err_forbidden")
	case errors.Is(err, ErrLeadScrub):
		s.fail(w, r, sc, http.StatusBadGateway, "err.gateway", "le.err_scrub")
	default:
		s.leadsFailure(w, r, sc, err)
	}
}

func (s *Server) leadSearch(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	eraser, ok := s.Leads.(LeadEraser)
	if !ok {
		s.renderSearch(w, r, sc, http.StatusOK, leadSearchData{Unavailable: true})
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if local, domain, at := strings.Cut(email, "@"); !at || local == "" || domain == "" || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		s.renderSearch(w, r, sc, http.StatusBadRequest, leadSearchData{Error: s.msg("le.err_email")})
		return
	}
	items, truncated, err := eraser.Find(r.Context(), email)
	// An older forms module has no search: the router answers 404 or 405.
	if errors.Is(err, ErrLeadNotFound) {
		err = errors.New("the leads provider has no search by address (upgrade the forms module)")
	}
	if err != nil {
		s.eraseFailed(w, r, sc, err)
		return
	}
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "lead" + ".find", Target: "leads", Detail: map[string]any{"matches": len(items)}}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := leadSearchData{Searched: true, Truncated: truncated}
	for _, m := range items {
		if !leadID.MatchString(m.ID) {
			continue
		}
		d.Items = append(d.Items, leadMatchRow{LeadMatch: m, StatusLabel: s.leadStatusLabel(m.Status), Mention: m.Match != "address"})
	}
	s.renderSearch(w, r, sc, http.StatusOK, d)
}

type leadEraseData struct {
	Lead      Lead
	Error     string
	Recent    bool
	StepUpURL string
}

func (s *Server) renderErase(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, l Lead, errKey string) {
	d := leadEraseData{Lead: l, Recent: s.users().Recent(actorOf(sc)), StepUpURL: "/reauth?next=" + url.QueryEscape("/leads/"+l.ID+"/erase")}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "lead_erase.html", s.page(r, sc, "leads", s.msg("le.erase_title", l.ID), d))
}

func (s *Server) leadErasePage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if _, ok := s.Leads.(LeadEraser); !ok {
		s.renderSearch(w, r, sc, http.StatusOK, leadSearchData{Unavailable: true})
		return
	}
	l, err := s.Leads.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.eraseFailed(w, r, sc, err)
		return
	}
	s.renderErase(w, r, sc, http.StatusOK, l, "")
}

func (s *Server) leadErase(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	eraser, ok := s.Leads.(LeadEraser)
	if !ok {
		s.renderSearch(w, r, sc, http.StatusOK, leadSearchData{Unavailable: true})
		return
	}
	id := r.PathValue("id")
	if !leadID.MatchString(id) {
		s.eraseFailed(w, r, sc, ErrLeadNotFound)
		return
	}
	if !s.users().Recent(actorOf(sc)) {
		http.Redirect(w, r, "/reauth?next="+url.QueryEscape("/leads/"+id+"/erase"), http.StatusSeeOther)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirm")) != id {
		l, err := s.Leads.Get(r.Context(), id)
		if err != nil {
			s.eraseFailed(w, r, sc, err)
			return
		}
		s.renderErase(w, r, sc, http.StatusBadRequest, l, "le.err_confirm")
		return
	}
	err := eraser.Erase(r.Context(), id)
	if err != nil && !errors.Is(err, ErrLeadScrub) {
		s.eraseFailed(w, r, sc, err)
		return
	}
	// The lead is gone, so it is on record even when the scrub has to be repeated.
	if _, aerr := s.Audit.Append(context.WithoutCancel(r.Context()), audit.Entry{Actor: sc.User.Username, Action: "lead" + ".erase", Target: id, Detail: map[string]any{"scrubbed": err == nil}}); aerr != nil {
		s.internal(w, r, sc, aerr)
		return
	}
	if err != nil {
		s.eraseFailed(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/leads", "flash.lead_erased")
}

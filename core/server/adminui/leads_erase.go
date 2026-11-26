package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/httpx"
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

// eraseTimeout covers the scrub of the database files, which the provider gives 20 seconds, and
// stays inside the 30 seconds can-core has to answer the browser.
const eraseTimeout = 25 * time.Second

// Erase implements LeadEraser. It waits longer than the other calls.
func (c *HTTPLeads) Erase(ctx context.Context, id string) error {
	if !leadID.MatchString(id) {
		return ErrLeadNotFound
	}
	client := *c.client()
	client.Timeout = eraseTimeout
	patient := *c
	patient.Client = &client
	return patient.do(ctx, http.MethodDelete, "/v1/leads/"+id, nil, nil, nil)
}

// ledgerCommand records an erasure for restores. Nothing does it by itself yet.
func ledgerCommand(id string) string {
	return "sudo can backup ledger-add --subject " + id + " --class lead-submission"
}

func (s *Server) leadEraseRoutes() []route {
	return []route{
		{"GET /lead-erasure", s.authedPerm(PermLeadsErase, s.leadSearchPage)},
		{"POST /lead-erasure", s.authedPerm(PermLeadsErase, s.leadSearch)},
		{"GET /lead-erasure/{id}", s.authedPerm(PermLeadsErase, s.leadErasedPage)},
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

type leadErasedData struct {
	ID, Command string
	Unscrubbed  bool
}

// leadErasedPage says that the lead is gone and what still has to be done.
func (s *Server) leadErasedPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id := r.PathValue("id")
	if s.Leads == nil || !leadID.MatchString(id) {
		s.eraseFailed(w, r, sc, ErrLeadNotFound)
		return
	}
	_, err := s.Leads.Get(r.Context(), id)
	switch {
	case err == nil: // it is still there: this page must not say otherwise
		s.redirect(w, r, "/leads/"+url.PathEscape(id), "")
	case errors.Is(err, ErrLeadNotFound):
		d := leadErasedData{ID: id, Command: ledgerCommand(id), Unscrubbed: r.URL.Query().Get("scrub") == "pending"}
		s.render(w, r, http.StatusOK, "lead_erased.html", s.page(r, sc, "leads", s.msg("le.done_title", id), d))
	default:
		s.eraseFailed(w, r, sc, err)
	}
}

type leadEraseData struct {
	Command   string
	Lead      Lead
	Error     string
	Recent    bool
	StepUpURL string
}

func (s *Server) renderErase(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, l Lead, errKey string) {
	d := leadEraseData{Command: ledgerCommand(l.ID), Lead: l, Recent: s.users().Recent(actorOf(sc)), StepUpURL: "/reauth?next=" + url.QueryEscape("/leads/"+l.ID+"/erase")}
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
	// On record before anything is destroyed: the answer of the provider may never arrive.
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "lead" + ".erase_requested", Target: id}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	// A browser that gives up does not stop an erasure half way.
	ctx := context.WithoutCancel(r.Context())
	err := eraser.Erase(ctx, id)
	unscrubbed := errors.Is(err, ErrLeadScrub)
	if err != nil && !unscrubbed {
		// No answer, or an odd one: the lead may be gone all the same.
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, gerr := s.Leads.Get(check, id); !errors.Is(gerr, ErrLeadNotFound) || errors.Is(err, ErrLeadNotFound) {
			s.eraseFailed(w, r, sc, err)
			return
		}
		s.Log.Warn("lead erased although the request failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		unscrubbed = true
	}
	if _, aerr := s.Audit.Append(ctx, audit.Entry{Actor: sc.User.Username, Action: "lead" + ".erase", Target: id, Detail: map[string]any{"scrubbed": !unscrubbed}}); aerr != nil {
		s.internal(w, r, sc, aerr)
		return
	}
	next := "/lead-erasure/" + url.PathEscape(id)
	if unscrubbed {
		next += "?scrub=pending"
	}
	s.redirect(w, r, next, "")
}

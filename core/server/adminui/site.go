package adminui

// The website editor (specs/067-site-editor, docs/site-editor.md): edit, preview, publish, history.
// The logic is core/server/siteedit. Every route needs platform.operate; publishing needs a recent
// proof of identity.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/siteedit"
)

func (s *Server) siteRoutes() []route {
	op := func(h func(http.ResponseWriter, *http.Request, *sessionCtx)) http.HandlerFunc {
		return s.authedPerm(rbac.PermOperate, func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
			if s.Site == nil {
				s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
				return
			}
			h(w, r, sc)
		})
	}
	return []route{
		{"GET /site", op(s.siteEditor)},
		{"POST /site", op(s.siteSave)},
		{"GET /site/drafts/{id}", op(s.siteDraft)},
		{"GET /site/drafts/{id}/edit", op(s.siteDraftEdit)},
		{"GET /site/drafts/{id}/frame", op(s.siteFrame)},
		{"POST /site/drafts/{id}/publish", op(s.sitePublish)},
		{"GET /site/history", op(s.siteHistory)},
		{"POST /site/history/{rev}/restore", op(s.siteRestore)},
	}
}

func (s *Server) siteNav(r *http.Request, sc *sessionCtx, active string) []navItem {
	if s.Site == nil {
		return nil
	}
	if ok, err := s.allowed(r, sc, rbac.PermOperate); err != nil || !ok {
		return nil
	}
	return []navItem{{Href: "/site", Label: s.msg("nav.site"), Current: active == "site"}}
}

var draftID = regexp.MustCompile(`^[0-9a-f]{24}$`)

// siteFail maps a siteedit error to an error page.
func (s *Server) siteFail(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	page := func(status int, heading, msg string) {
		d := errorData{Heading: s.msg(heading), Message: msg, RequestID: httpx.RequestID(r.Context()), Home: "/site", HomeLabel: s.msg("si.back")}
		s.render(w, r, status, "error.html", s.page(r, sc, "site", d.Heading, d))
	}
	var inv *siteedit.InvalidError
	switch {
	case errors.Is(err, siteedit.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case errors.Is(err, siteedit.ErrStale):
		page(http.StatusConflict, "err.conflict", s.msg("si.err_stale"))
	case errors.Is(err, siteedit.ErrExpired):
		page(http.StatusConflict, "err.conflict", s.msg("err.expired"))
	case errors.Is(err, siteedit.ErrDocument):
		page(http.StatusBadRequest, "err.bad_request", s.msg("si.err_document"))
	case errors.Is(err, siteedit.ErrTooBig):
		page(http.StatusBadRequest, "err.bad_request", s.msg("si.err_too_big"))
	case errors.As(err, &inv):
		page(http.StatusBadRequest, "err.bad_request", inv.Error())
	case errors.Is(err, siteedit.ErrNotLive):
		s.Log.Error("site publish failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		page(http.StatusBadGateway, "si.err_not_live_title", s.msg("si.err_not_live"))
	case errors.Is(err, siteedit.ErrNoSite):
		page(http.StatusServiceUnavailable, "err.unavailable", s.msg("si.err_no_site"))
	case errors.Is(err, modmgr.ErrNoCompany):
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.no_company")
	default:
		s.internal(w, r, sc, err)
	}
}

type siteData struct {
	Base, Live, LiveClass string
	Sections              []siteSection
	// Summary are the errors, in the order of the form; Other those of no field.
	Summary, Other []siteFieldError
	Stale          bool
}

var liveLabels = map[string][2]string{
	siteedit.LiveCurrent:  {"si.live_current", "ok"},
	siteedit.LiveOutdated: {"si.live_outdated", "warn"},
	siteedit.LiveUnknown:  {"si.live_unknown", "neutral"},
	siteedit.LiveNone:     {"si.live_none", "warn"},
}

func (s *Server) renderEditor(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, st siteedit.State, base string, v siteValues, errs map[string]string, stale bool) {
	d := siteData{Base: base, Stale: stale}
	l := liveLabels[st.Live]
	d.Live, d.LiveClass = s.msg(l[0]), l[1]
	var other []siteFieldError
	d.Sections, other = s.siteForm(v, errs, st.Company.Site.Brand.Logo)
	sort.Slice(other, func(i, j int) bool { return other[i].Field < other[j].Field })
	d.Other = other
	for _, sec := range d.Sections {
		if sec.Error != "" {
			d.Summary = append(d.Summary, siteFieldError{Field: "h-" + sec.ID, Message: sec.Title + ": " + sec.Error})
		}
		for _, g := range sec.Groups {
			for _, f := range g.Fields {
				if f.Error != "" {
					label := f.Label
					if g.Legend != "" {
						label = g.Legend + ", " + label
					}
					d.Summary = append(d.Summary, siteFieldError{Field: f.ID, Message: sec.Title + ", " + label + ": " + f.Error})
				}
			}
		}
	}
	s.render(w, r, status, "site.html", s.page(r, sc, "site", s.msg("si.title"), d))
}

func (s *Server) siteEditor(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	st, err := s.Site.State(r.Context())
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	s.renderEditor(w, r, sc, http.StatusOK, st, st.Hash, valuesOf(st.Company.Site), nil, false)
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) siteSave(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	st, err := s.Site.State(r.Context())
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	base := r.PostFormValue("base")
	if !hashRe.MatchString(base) {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
		return
	}
	v := valuesFrom(r.PostForm)
	sub := v.parse(st.Company.Site.Template)
	d, err := s.Site.CreateDraft(r.Context(), sc.User.Username, base, sub.site, siteedit.KindPublish)
	var inv *siteedit.InvalidError
	switch {
	case errors.As(err, &inv):
		s.renderEditor(w, r, sc, http.StatusBadRequest, st, base, v, formErrors(sub, inv.Fields), base != st.Hash)
	case errors.Is(err, siteedit.ErrStale):
		// What was typed stays on the page; it can only be sent after reloading.
		s.renderEditor(w, r, sc, http.StatusConflict, st, base, v, nil, true)
	case err != nil:
		s.siteFail(w, r, sc, err)
	default:
		http.Redirect(w, r, "/site/drafts/"+d.ID, http.StatusSeeOther)
	}
}

type siteDraftData struct {
	ID, Actor            string
	Created              time.Time
	Restore, Published   bool
	Stale, Expired       bool
	Pages, Warnings      []string
	Invalid              []siteedit.FieldError
	Diff                 []diffRow
	DiffTooLarge         bool
	CanPublish, Recent   bool
	StepUpURL, PublishTo string
}

func (s *Server) draft(w http.ResponseWriter, r *http.Request, sc *sessionCtx) (string, bool) {
	id := r.PathValue("id")
	if !draftID.MatchString(id) {
		s.siteFail(w, r, sc, siteedit.ErrNotFound)
		return "", false
	}
	return id, true
}

func (s *Server) siteDraft(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, ok := s.draft(w, r, sc)
	if !ok {
		return
	}
	rv, err := s.Site.Review(r.Context(), id)
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	d := siteDraftData{
		ID: id, Actor: rv.Draft.Actor, Created: rv.Draft.CreatedAt, Restore: rv.Draft.Kind == siteedit.KindRestore, Published: rv.Draft.State != "open",
		Stale: rv.Stale, Expired: rv.Expired, Pages: rv.Pages, Warnings: rv.Warnings, Invalid: rv.Invalid, DiffTooLarge: rv.DiffTooLarge,
		Recent: s.users().Recent(actorOf(sc)), StepUpURL: "/reauth?next=" + url.QueryEscape("/site/drafts/"+id),
		PublishTo: "/site/drafts/" + id + "/publish",
	}
	d.CanPublish = !d.Published && !d.Stale && !d.Expired && len(d.Invalid) == 0
	for _, l := range rv.Diff {
		switch l.Op {
		case '+':
			d.Diff = append(d.Diff, diffRow{"add", "+", l.Text})
		case '-':
			d.Diff = append(d.Diff, diffRow{"del", "-", l.Text})
		case '~':
			d.Diff = append(d.Diff, diffRow{"skip", "~", ""})
		default:
			d.Diff = append(d.Diff, diffRow{"ctx", " ", l.Text})
		}
	}
	// This page alone may frame something: the preview, from this origin.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; manifest-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	s.render(w, r, http.StatusOK, "site_draft.html", s.page(r, sc, "site", s.msg("si.preview_title"), d))
}

func (s *Server) siteDraftEdit(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, ok := s.draft(w, r, sc)
	if !ok {
		return
	}
	d, err := s.Site.GetDraft(r.Context(), id)
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	st, err := s.Site.State(r.Context())
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	s.renderEditor(w, r, sc, http.StatusOK, st, d.BaseHash, valuesOf(d.Site), nil, d.BaseHash != st.Hash)
}

// siteFrame serves one page of a draft for the preview frame. The document is the website, not the
// admin UI: its policy sandboxes it (an opaque origin, no script, no forms, no navigation of the
// page around it) and allows only its own inline stylesheet and inline images.
func (s *Server) siteFrame(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, ok := s.draft(w, r, sc)
	if !ok {
		return
	}
	doc, style, err := s.Site.Preview(r.Context(), id, r.URL.Query().Get("page"))
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src "+style+"; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(doc) //nolint:gosec // rendered by html/template; sandboxed by the policy above
}

func (s *Server) sitePublish(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, ok := s.draft(w, r, sc)
	if !ok {
		return
	}
	if !s.users().Recent(actorOf(sc)) {
		http.Redirect(w, r, "/reauth?next="+url.QueryEscape("/site/drafts/"+id), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if _, err := s.Site.Publish(ctx, sc.User.Username, id); err != nil {
		// A publisher that is not ready says what to do about it.
		if errors.Is(err, siteedit.ErrNotReady) {
			d := errorData{Heading: s.msg("err.unavailable"), Message: err.Error(), RequestID: httpx.RequestID(r.Context()), Home: "/site", HomeLabel: s.msg("si.back")}
			s.render(w, r, http.StatusServiceUnavailable, "error.html", s.page(r, sc, "site", d.Heading, d))
			return
		}
		s.siteFail(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/site", "flash.site_published")
}

type siteRevisionRow struct {
	ID                 int64
	CreatedAt          time.Time
	Actor, Kind, Class string
	Current            bool
}

var revisionKinds = map[string]string{siteedit.KindPublish: "si.kind_publish", siteedit.KindRestore: "si.kind_restore", siteedit.KindExternal: "si.kind_external"}

func (s *Server) siteHistory(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	revs, err := s.Site.History(r.Context())
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	var rows []siteRevisionRow
	for _, rv := range revs {
		rows = append(rows, siteRevisionRow{ID: rv.ID, CreatedAt: rv.CreatedAt, Actor: rv.Actor, Kind: s.msg(revisionKinds[rv.Kind]), Current: rv.Current})
	}
	s.render(w, r, http.StatusOK, "site_history.html", s.page(r, sc, "site", s.msg("si.history_title"), rows))
}

func (s *Server) siteRestore(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
	if err != nil {
		s.siteFail(w, r, sc, siteedit.ErrNotFound)
		return
	}
	d, err := s.Site.Restore(r.Context(), sc.User.Username, rev)
	if err != nil {
		s.siteFail(w, r, sc, err)
		return
	}
	http.Redirect(w, r, "/site/drafts/"+d.ID, http.StatusSeeOther)
}

package adminui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Pages of the shared platform services: approvals, notifications, access (roles).
// Contract: specs/010-admin-ui/contracts/ui-routes.md, specs/015-approvals-notifications.

func (s *Server) platformRoutes() []route {
	return []route{
		{"GET /approvals", s.authed(s.approvalsPage)},
		{"GET /approvals/{id}", s.authed(s.approvalPage)},
		{"POST /approvals/{id}/decide", s.authed(s.approvalDecide)},
		{"POST /approvals/{id}/comment", s.authed(s.approvalComment)},
		{"POST /approvals/{id}/cancel", s.authed(s.approvalCancel)},
		{"POST /approvals/delegate", s.authed(s.delegate)},
		{"POST /approvals/delegations/{did}/revoke", s.authed(s.undelegate)},
		{"GET /notifications", s.authed(s.notificationsPage)},
		{"POST /notifications/read", s.authed(s.notificationsReadAll)},
		{"POST /notifications/settings", s.authed(s.notificationsSettings)},
		{"POST /notifications/prefs", s.authed(s.notificationsPref)},
		{"POST /notifications/{id}/read", s.authed(s.notificationRead)},
		{"GET /access", s.authedPerm(rbac.PermManage, s.accessPage)},
		{"POST /access/{username}/grant", s.authedPerm(rbac.PermManage, s.accessGrant)},
		{"POST /access/{username}/revoke", s.authedPerm(rbac.PermManage, s.accessRevoke)},
	}
}

func subjectOf(sc *sessionCtx) string { return "user:" + sc.User.Username }

// allowed reports whether the user holds perm; with no RBAC service everything is allowed.
func (s *Server) allowed(r *http.Request, sc *sessionCtx, perm string) (bool, error) {
	if s.RBAC == nil {
		return true, nil
	}
	return s.RBAC.Check(r.Context(), subjectOf(sc), perm, "")
}

// authedPerm is authed plus a permission check (403 page when missing, fail closed on errors).
func (s *Server) authedPerm(perm string, h func(http.ResponseWriter, *http.Request, *sessionCtx)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
		ok, err := s.allowed(r, sc, perm)
		if err != nil {
			s.internal(w, r, sc, err)
			return
		}
		if !ok {
			s.fail(w, r, sc, http.StatusForbidden, "err.no_permission", "err.no_permission_msg")
			return
		}
		h(w, r, sc)
	})
}

// homeGate sends users who may not view the platform to their approvals inbox.
func (s *Server) homeGate(h func(http.ResponseWriter, *http.Request, *sessionCtx)) func(http.ResponseWriter, *http.Request, *sessionCtx) {
	return func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
		ok, err := s.allowed(r, sc, rbac.PermView)
		if err != nil {
			s.internal(w, r, sc, err)
			return
		}
		if !ok && s.Approvals != nil {
			http.Redirect(w, r, "/approvals", http.StatusSeeOther)
			return
		}
		if !ok {
			s.fail(w, r, sc, http.StatusForbidden, "err.no_permission", "err.no_permission_msg")
			return
		}
		h(w, r, sc)
	}
}

// platformNav adds the approvals, notifications and access links when the services exist.
func (s *Server) platformNav(r *http.Request, sc *sessionCtx, active string) []navItem {
	var out []navItem
	if s.Approvals != nil {
		out = append(out, navItem{Href: "/approvals", Label: s.msg("nav.approvals"), Current: active == "approvals"})
	}
	if s.Notify != nil {
		it := navItem{Href: "/notifications", Label: s.msg("nav.notifications"), Current: active == "notifications"}
		if n, err := s.Notify.UnreadCount(r.Context(), subjectOf(sc)); err == nil && n > 0 {
			it.Count = strconv.Itoa(n)
			if n >= notify.MaxUnreadCount {
				it.Count = strconv.Itoa(notify.MaxUnreadCount-1) + "+"
			}
		}
		out = append(out, it)
	}
	if s.RBAC != nil {
		if ok, err := s.allowed(r, sc, rbac.PermManage); err == nil && ok {
			out = append(out, navItem{Href: "/access", Label: s.msg("nav.access"), Current: active == "access"})
		}
	}
	return out
}

// --- approvals --------------------------------------------------------------------------------

var statusClass = map[string]string{"pending": "warn", "approved": "ok", "rejected": "err", "cancelled": "neutral", "expired": "neutral"}

type approvalRow struct {
	ID, Title, Type, Requester, Amount, StatusLabel, StatusClass string
	Created                                                      time.Time
}

func (s *Server) rows(rs []approvals.Request) []approvalRow {
	out := make([]approvalRow, 0, len(rs))
	for _, r := range rs {
		t := r.Title
		if t == "" {
			t = s.msg("ap.untitled")
		}
		out = append(out, approvalRow{ID: r.ID, Title: t, Type: r.Type, Requester: r.Requester, Amount: formatAmount(r.Amount), StatusLabel: s.msg("st." + r.Status), StatusClass: statusClass[r.Status], Created: r.CreatedAt})
	}
	return out
}

// formatAmount shows minor units with two decimals.
func formatAmount(v int64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%02d", v/100, v%100)
}

type delegationRow struct {
	ID        int64
	Delegate  string
	Delegator string
	From, To  time.Time
	Mine      bool
}

type tableData struct {
	Rows               []approvalRow
	ID, Caption, Empty string
}

type approvalsData struct {
	WaitingT, RequestedT, AllT tableData
	Waiting, Requested, All    []approvalRow
	IsAdmin                    bool
	Status                     string
	Statuses                   []statusOption
	Delegations                []delegationRow
}

func (s *Server) approvalsPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Approvals == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	ctx, me := r.Context(), subjectOf(sc)
	d := approvalsData{}
	waiting, err := s.Approvals.Waiting(ctx, me, pageSize)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	mine, err := s.Approvals.Requested(ctx, me, pageSize)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d.Waiting, d.Requested = s.rows(waiting), s.rows(mine)
	if ok, err := s.allowed(r, sc, rbac.PermApprovalAdmin); err == nil && ok {
		d.IsAdmin = true
		d.Status = r.URL.Query().Get("status")
		all, err := s.Approvals.List(ctx, d.Status, pageSize)
		if err != nil {
			d.Status = ""
			all, err = s.Approvals.List(ctx, "", pageSize)
		}
		if err != nil {
			s.internal(w, r, sc, err)
			return
		}
		d.All = s.rows(all)
		d.Statuses = append(d.Statuses, statusOption{Value: "", Label: s.msg("ap.any_status"), Selected: d.Status == ""})
		for _, st := range []string{"pending", "approved", "rejected", "cancelled", "expired"} {
			d.Statuses = append(d.Statuses, statusOption{Value: st, Label: s.msg("st." + st), Selected: d.Status == st})
		}
	}
	d.WaitingT = tableData{d.Waiting, "waiting", s.msg("ap.waiting"), s.msg("ap.waiting_empty")}
	d.RequestedT = tableData{d.Requested, "mine", s.msg("ap.requested"), s.msg("ap.requested_empty")}
	d.AllT = tableData{d.All, "all", s.msg("ap.all"), s.msg("ap.waiting_empty")}
	ds, err := s.Approvals.Delegations(ctx, me)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	for _, x := range ds {
		d.Delegations = append(d.Delegations, delegationRow{ID: x.ID, Delegate: x.Delegate, Delegator: x.Delegator, From: x.From, To: x.To, Mine: x.Delegator == me})
	}
	s.render(w, r, http.StatusOK, "approvals.html", s.page(r, sc, "approvals", s.msg("ap.title"), d))
}

type stepRowView struct {
	Name, StatusLabel, StatusClass, Approvers string
	Approvals, Quorum                         int
}

type decisionRow struct {
	KindLabel, Actor, OnBehalf, Comment string
	At                                  time.Time
}

type detailKV struct{ Key, Value string }

type approvalData struct {
	Req                                   approvals.Request
	StatusLabel, StatusClass, Amount      string
	Steps                                 []stepRowView
	History                               []decisionRow
	Details                               []detailKV
	CanDecide, CanCancel, CanComment, Own bool
}

func (s *Server) notFoundApproval(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.fail(w, r, sc, http.StatusNotFound, "err.approval_not_found", "err.approval_not_found_m")
}

// visible loads a request the user may see; anyone else gets the same 404 as for a missing id.
func (s *Server) visible(w http.ResponseWriter, r *http.Request, sc *sessionCtx) (approvals.Detail, bool) {
	if s.Approvals == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return approvals.Detail{}, false
	}
	d, err := s.Approvals.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		s.notFoundApproval(w, r, sc)
		return d, false
	case err != nil:
		s.internal(w, r, sc, err)
		return d, false
	}
	if !s.Approvals.CanView(r.Context(), d, subjectOf(sc)) {
		s.notFoundApproval(w, r, sc)
		return d, false
	}
	return d, true
}

var payloadKey = regexp.MustCompile(`^[A-Za-z0-9_. -]{1,60}$`)

// payloadDetails lists the scalar members of the module's payload for the approver. Nested values
// are not shown; everything is escaped by the template.
func payloadDetails(raw json.RawMessage) []detailKV {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []detailKV
	for _, k := range keys {
		if !payloadKey.MatchString(k) || len(out) >= 30 {
			continue
		}
		var v string
		switch x := m[k].(type) {
		case string:
			v = x
		case float64, bool:
			v = fmt.Sprint(x)
		default:
			continue
		}
		if len(v) > 300 {
			v = v[:300] + "..."
		}
		out = append(out, detailKV{k, v})
	}
	return out
}

func (s *Server) approvalPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, ok := s.visible(w, r, sc)
	if !ok {
		return
	}
	me := subjectOf(sc)
	// Audit before returning data the module supplied (it may be personal data).
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: sc.User.Username, Action: "approval.read", Target: d.Request.ID}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	ad := approvalData{Req: d.Request, StatusLabel: s.msg("st." + d.Request.Status), StatusClass: statusClass[d.Request.Status], Amount: formatAmount(d.Request.Amount), Details: payloadDetails(d.Request.Payload), Own: d.Request.Requester == me}
	if ad.Req.Title == "" {
		ad.Req.Title = s.msg("ap.untitled")
	}
	ad.Req.Payload = nil
	for _, st := range d.Steps {
		who := append(append([]string{}, st.Users...), st.Roles...)
		ad.Steps = append(ad.Steps, stepRowView{Name: st.Name, StatusLabel: s.msg("st." + st.Status), StatusClass: map[string]string{"approved": "ok", "rejected": "err", "active": "warn"}[st.Status], Approvers: strings.Join(who, ", "), Approvals: st.Approvals, Quorum: st.Quorum})
	}
	for _, x := range d.Decisions {
		row := decisionRow{KindLabel: s.msg("dk." + x.Kind), Actor: x.Actor, Comment: x.Comment, At: x.At}
		if x.OnBehalfOf != "" {
			row.OnBehalf = s.msg("ap.on_behalf", x.OnBehalfOf)
		}
		ad.History = append(ad.History, row)
	}
	ad.CanDecide = s.Approvals.CanAct(r.Context(), d, me)
	ad.CanComment = true
	if d.Request.Status == "pending" {
		ad.CanCancel = ad.Own
		if !ad.CanCancel {
			ad.CanCancel, _ = s.allowed(r, sc, rbac.PermApprovalAdmin)
		}
	}
	s.render(w, r, http.StatusOK, "approval.html", s.page(r, sc, "approvals", s.msg("ap.detail_title"), ad))
}

// approvalErr renders the page for an engine error.
func (s *Server) approvalErr(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		s.notFoundApproval(w, r, sc)
	case errors.Is(err, approvals.ErrInvalid):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
	case errors.Is(err, approvals.ErrSelfApproval):
		s.fail(w, r, sc, http.StatusForbidden, "err.forbidden", "err.self_approval")
	case errors.Is(err, approvals.ErrNotEligible):
		s.fail(w, r, sc, http.StatusForbidden, "err.forbidden", "err.not_approver")
	case errors.Is(err, approvals.ErrNotPending):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.not_pending")
	case errors.Is(err, approvals.ErrAlreadyVoted):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.already_voted")
	default:
		s.internal(w, r, sc, err)
	}
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, ok := s.visible(w, r, sc)
	if !ok {
		return
	}
	dec := r.PostFormValue("decision")
	if dec != "approve" && dec != "reject" {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	}
	if _, err := s.Approvals.Decide(r.Context(), d.Request.ID, subjectOf(sc), dec, strings.TrimSpace(r.PostFormValue("comment"))); err != nil {
		s.approvalErr(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/approvals/"+d.Request.ID, "flash.decided")
}

func (s *Server) approvalComment(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, ok := s.visible(w, r, sc)
	if !ok {
		return
	}
	if err := s.Approvals.Comment(r.Context(), d.Request.ID, subjectOf(sc), r.PostFormValue("comment")); err != nil {
		s.approvalErr(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/approvals/"+d.Request.ID, "flash.commented")
}

func (s *Server) approvalCancel(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, ok := s.visible(w, r, sc)
	if !ok {
		return
	}
	if err := s.Approvals.Cancel(r.Context(), d.Request.ID, subjectOf(sc), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		s.approvalErr(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/approvals/"+d.Request.ID, "flash.cancelled")
}

func (s *Server) delegate(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Approvals == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	until, err := time.Parse("2006-01-02", r.PostFormValue("until"))
	name := strings.TrimSpace(r.PostFormValue("delegate"))
	if err != nil || !usernameOK(name) {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	}
	now := s.Now()
	if _, err := s.Approvals.Delegate(r.Context(), subjectOf(sc), "user:"+name, now, until.Add(24*time.Hour)); err != nil {
		s.approvalErr(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/approvals", "flash.delegated")
}

func (s *Server) undelegate(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Approvals == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("did"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.Approvals.RevokeDelegation(r.Context(), subjectOf(sc), id); err != nil {
		s.approvalErr(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/approvals", "flash.undelegated")
}

var userRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

func usernameOK(s string) bool { return userRe.MatchString(s) }

// --- notifications ----------------------------------------------------------------------------

type notifRow struct {
	ID           int64
	Title, Body  string
	Link         string
	Created      time.Time
	Read, Urgent bool
}

type prefCell struct {
	Type, Label, Channel string
	On                   bool
}

type notifData struct {
	Items                           []notifRow
	NextURL                         string
	Email, QuietStart, QuietEnd, TZ string
	Digests                         []statusOption
	EmailT, InAppT                  prefTable
}

type prefTable struct {
	ID, Caption, CSRF string
	Cells             []prefCell
}

func minutesToClock(m int) string {
	if m < 0 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func clockToMinutes(s string) (int, bool) {
	if s == "" {
		return -1, true
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

var digestKeys = map[string]string{"off": "nt.digest_off", "hourly": "nt.digest_hourly", "daily": "nt.digest_daily"}

var prefLabelKeys = map[string]string{
	"approval.requested": "nt.type_approval_requested", "approval.escalated": "nt.type_approval_escalated",
	"approval.decided": "nt.type_approval_decided", "lead" + ".created": "nt.type_lead_created",
}

var prefTypes = []string{"approval.requested", "approval.escalated", "approval.decided", "lead" + ".created", "*"}

func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Notify == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	ctx, me := r.Context(), subjectOf(sc)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	items, err := s.Notify.List(ctx, me, notify.ListOptions{BeforeID: before, Limit: pageSize})
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := notifData{}
	for _, it := range items {
		d.Items = append(d.Items, notifRow{ID: it.ID, Title: it.Title, Body: it.Body, Link: it.Link, Created: it.CreatedAt, Read: it.Read, Urgent: it.Urgent})
	}
	if len(items) == pageSize {
		d.NextURL = "/notifications?before=" + strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	set, err := s.Notify.Settings(ctx, me)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d.Email, d.QuietStart, d.QuietEnd, d.TZ = set.Email, minutesToClock(set.QuietStart), minutesToClock(set.QuietEnd), set.TZ
	for _, v := range []string{"off", "hourly", "daily"} {
		d.Digests = append(d.Digests, statusOption{Value: v, Label: s.msg(digestKeys[v]), Selected: set.Digest == v})
	}
	prefs, err := s.Notify.Prefs(ctx, me)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	on := map[string]bool{}
	for _, p := range prefs {
		on[p.Channel+"|"+p.Type] = p.Enabled
	}
	cell := func(ch, typ string) prefCell {
		label := s.msg("nt.type_all")
		if typ != "*" {
			label = s.msg(prefLabelKeys[typ])
		}
		v, ok := on[ch+"|"+typ]
		return prefCell{Type: typ, Label: label, Channel: ch, On: !ok || v}
	}
	d.EmailT = prefTable{ID: "pe", Caption: s.msg("nt.channel_email"), CSRF: sc.Session.CSRF}
	d.InAppT = prefTable{ID: "pi", Caption: s.msg("nt.channel_inapp"), CSRF: sc.Session.CSRF}
	for _, t := range prefTypes {
		d.EmailT.Cells = append(d.EmailT.Cells, cell(notify.ChannelEmail, t))
		d.InAppT.Cells = append(d.InAppT.Cells, cell(notify.ChannelInApp, t))
	}
	s.render(w, r, http.StatusOK, "notifications.html", s.page(r, sc, "notifications", s.msg("nt.title"), d))
}

func (s *Server) notificationsReadAll(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Notify == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	if err := s.Notify.MarkAllRead(r.Context(), subjectOf(sc)); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.redirect(w, r, "/notifications", "flash.notif_read")
}

func (s *Server) notificationRead(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Notify == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	switch err := s.Notify.MarkRead(r.Context(), subjectOf(sc), id); {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/notifications", "flash.notif_read")
	}
}

func (s *Server) notificationsSettings(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Notify == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	qs, ok1 := clockToMinutes(r.PostFormValue("quiet_start"))
	qe, ok2 := clockToMinutes(r.PostFormValue("quiet_end"))
	if !ok1 || !ok2 {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	}
	err := s.Notify.SetSettings(r.Context(), notify.Settings{Subject: subjectOf(sc), Email: strings.TrimSpace(r.PostFormValue("email")), QuietStart: qs, QuietEnd: qe, TZ: r.PostFormValue("tz"), Digest: r.PostFormValue("digest")})
	switch {
	case notify.ErrInvalid(err):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/notifications", "flash.notif_saved")
	}
}

func (s *Server) notificationsPref(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Notify == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	// An unchecked box is not sent, so absence of `enabled` means off.
	err := s.Notify.SetPref(r.Context(), subjectOf(sc), r.PostFormValue("type"), r.PostFormValue("channel"), r.PostFormValue("enabled") != "")
	switch {
	case notify.ErrInvalid(err):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/notifications", "flash.notif_saved")
	}
}

// --- access -----------------------------------------------------------------------------------

type accessAssignment struct{ Role, Scope string }

type accessUser struct {
	Username    string
	Assignments []accessAssignment
}

type roleRow struct{ Name, Permissions, Sources string }

type accessData struct {
	Users []accessUser
	Roles []roleRow
	Names []string
}

func (s *Server) accessPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.RBAC == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	users, err := s.RBAC.Users(r.Context(), 500)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	roles, err := s.RBAC.Roles(r.Context())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := accessData{}
	for _, u := range users {
		au := accessUser{Username: u.Username}
		for _, a := range u.Assignments {
			au.Assignments = append(au.Assignments, accessAssignment{a.Role, a.Scope})
		}
		d.Users = append(d.Users, au)
	}
	for _, ro := range roles {
		d.Names = append(d.Names, ro.Name)
		d.Roles = append(d.Roles, roleRow{ro.Name, strings.Join(ro.Permissions, ", "), strings.Join(ro.Sources, ", ")})
	}
	s.render(w, r, http.StatusOK, "access.html", s.page(r, sc, "access", s.msg("ac.title"), d))
}

func (s *Server) accessChange(w http.ResponseWriter, r *http.Request, sc *sessionCtx, flash string, change func(actor, subject, role, scope string) error) {
	if s.RBAC == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	name := r.PathValue("username")
	if !usernameOK(name) {
		s.fail(w, r, sc, http.StatusNotFound, "err.user_not_found", "err.user_not_found_msg")
		return
	}
	if _, err := s.DB.AdminByUsername(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, sc, http.StatusNotFound, "err.user_not_found", "err.user_not_found_msg")
			return
		}
		s.internal(w, r, sc, err)
		return
	}
	switch err := change(sc.User.Username, "user:"+name, strings.TrimSpace(r.PostFormValue("role")), strings.TrimSpace(r.PostFormValue("scope"))); {
	case errors.Is(err, rbac.ErrUnknownRole), errors.Is(err, rbac.ErrInvalid):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
	case errors.Is(err, rbac.ErrLastOwner):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.last_owner")
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/access", flash)
	}
}

func (s *Server) accessGrant(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.accessChange(w, r, sc, "flash.role_granted", func(a, subj, role, scope string) error {
		return s.RBAC.Grant(r.Context(), a, subj, role, scope)
	})
}

func (s *Server) accessRevoke(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.accessChange(w, r, sc, "flash.role_revoked", func(a, subj, role, scope string) error {
		return s.RBAC.Revoke(r.Context(), a, subj, role, scope)
	})
}

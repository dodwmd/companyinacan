package session

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso). The person arrives from the Apps
// page with a one-time launch ticket in the query string. GET only shows a form (a link scanner or
// a browser prefetch must not spend the ticket); the POST redeems it over the module API and starts
// a session of the module's own. The browser only ever receives that session's cookie, and the
// module never receives a long-lived credential: it keeps an opaque handle that can-core answers
// "is this person still allowed, and with what" for.

// TicketView is the interstitial: who the ticket signs in and, when this browser already holds a
// session of somebody else, who that is.
type TicketView struct{ CSRF, Ticket, Name, Account, Subject, Other string }

// MessageKeys are the keys LaunchPages.SignIn is called with; every module's catalogue has them.
var MessageKeys = []string{"login.ticket_invalid", "login.sso_unavailable", "login.csrf", "login.locked"}

// LaunchPages is how a module draws the outcomes of the two ticket requests.
type LaunchPages struct {
	// SignIn renders the sign-in page with a status and one of MessageKeys.
	SignIn func(status int, msgKey string)
	// Confirm renders the interstitial.
	Confirm func(status int, v TicketView)
	// Failed counts a refused ticket against the client address. When the address is over its
	// limit it returns true (after any delay it imposes) and the Retry-After value.
	Failed func() (retryAfter string, limited bool)
}

func (m *Manager[P]) api() modclient.SessionAPI {
	if m.cfg.SSO == nil {
		return nil
	}
	return m.cfg.SSO()
}

func (m *Manager[P]) account(subject string) bool {
	return strings.HasPrefix(subject, "user:") && (m.cfg.ValidSubject == nil || m.cfg.ValidSubject(subject))
}

// revalidate asks can-core again about a launch-ticket session. Any doubt (can-core unreachable,
// handle unknown, subject changed, nothing left to do here) is false.
func (m *Manager[P]) revalidate(r *http.Request, s *Session[P]) (P, string, bool) {
	var none P
	api := m.api()
	if api == nil {
		return none, "", false
	}
	v, err := api.RevalidateSession(r.Context(), s.handle)
	if err != nil || !v.Active || v.Subject != s.Subject {
		return none, "", false
	}
	p, ok := m.cfg.Grant(v.Subject, v.Permissions)
	if !ok {
		return none, "", false
	}
	return p, v.Name, true
}

// peek asks who the ticket would sign in, for this browser only: the nonce cookie can-core set at
// launch must come with it, so a link opened anywhere else names nobody. A page has been written
// when ok is false.
func (m *Manager[P]) peek(r *http.Request, ticket string, pg LaunchPages, refuse func()) (modclient.LaunchSession, bool) {
	p, err := m.api().PeekTicket(r.Context(), ticket, modclient.LaunchBinding(r, m.cfg.Module, m.cfg.Secure(r)))
	switch {
	case errors.Is(err, modclient.ErrUnavailable):
		pg.SignIn(http.StatusServiceUnavailable, "login.sso_unavailable")
	case err != nil || !p.Active || !m.account(p.Subject):
		refuse()
	default:
		return p, true
	}
	return modclient.LaunchSession{}, false
}

func (m *Manager[P]) confirm(w http.ResponseWriter, r *http.Request, status int, ticket string, p modclient.LaunchSession, pg LaunchPages) {
	v := TicketView{CSRF: m.LoginCSRF(w, r), Ticket: ticket, Name: p.Name, Subject: p.Subject, Account: strings.TrimPrefix(p.Subject, "user:")}
	if other, shown := m.Live(r); other != "" && other != p.Subject {
		v.Other = shown
	}
	pg.Confirm(status, v)
}

// TicketPage is GET of the launch link. It never consumes the ticket. The caller answers 404 itself
// when there is no single sign-on.
func (m *Manager[P]) TicketPage(w http.ResponseWriter, r *http.Request, pg LaunchPages) {
	t := r.URL.Query().Get("t")
	if !modclient.TicketShaped(t) {
		pg.SignIn(http.StatusBadRequest, "login.ticket_invalid")
		return
	}
	p, ok := m.peek(r, t, pg, func() { pg.SignIn(http.StatusUnauthorized, "login.ticket_invalid") })
	if !ok {
		return
	}
	m.confirm(w, r, http.StatusOK, t, p, pg)
}

// TicketPost redeems the ticket. The caller has bounded and parsed the form of this public route
// and answers 404 itself when there is no single sign-on. Refusals are counted per client address
// (as failed sign-ins are) and all look the same; a valid ticket is never throttled. admit, when
// set, is a last check of the person before a session starts. landing turns can-core's return path
// into a local URL.
func (m *Manager[P]) TicketPost(w http.ResponseWriter, r *http.Request, pg LaunchPages, admit func(P) bool, landing func(returnPath string) string) {
	if !m.CheckLoginCSRF(r) {
		pg.SignIn(http.StatusForbidden, "login.csrf")
		return
	}
	refuse := func() {
		if after, limited := pg.Failed(); limited {
			w.Header().Set("Retry-After", after)
			pg.SignIn(http.StatusTooManyRequests, "login.locked")
			return
		}
		pg.SignIn(http.StatusUnauthorized, "login.ticket_invalid")
	}
	t := r.PostFormValue("ticket")
	// A session of somebody else is never replaced without the person saying so.
	if other, _ := m.Live(r); other != "" {
		p, ok := m.peek(r, t, pg, refuse)
		if !ok {
			return
		}
		if p.Subject != other && r.PostFormValue("replace") != p.Subject {
			m.confirm(w, r, http.StatusConflict, t, p, pg)
			return
		}
	}
	ls, err := m.api().RedeemTicket(r.Context(), t, modclient.LaunchBinding(r, m.cfg.Module, m.cfg.Secure(r)))
	if err != nil {
		if errors.Is(err, modclient.ErrUnavailable) {
			pg.SignIn(http.StatusServiceUnavailable, "login.sso_unavailable")
			return
		}
		refuse()
		return
	}
	if !ls.Active || !m.account(ls.Subject) || ls.Session == "" || !ls.ExpiresAt.After(m.cfg.Now()) {
		refuse()
		return
	}
	p, ok := m.cfg.Grant(ls.Subject, ls.Permissions)
	if !ok || (admit != nil && !admit(p)) {
		refuse()
		return
	}
	m.replace(w, r, &Session[P]{Subject: ls.Subject, p: p, handle: ls.Session, name: ls.Name, deadline: ls.ExpiresAt})
	// Land on a clean URL: the ticket is not in it, and the place to land is checked here again.
	http.Redirect(w, r, landing(ls.ReturnPath), http.StatusSeeOther)
}

// ---- step-up ---------------------------------------------------------------------------------

// StepUpMaxAge is how recent the person's authentication at the admin UI must be for a
// launch-ticket session to do something irreversible.
const StepUpMaxAge = 5 * time.Minute

// The ways a step-up refuses.
var (
	// ErrStepUpReenter: the sign-in token was not typed again, or not the same one.
	ErrStepUpReenter = errors.New("the sign-in token must be typed again")
	// ErrStepUpUnconfirmed: the credential could not be confirmed as still allowed.
	ErrStepUpUnconfirmed = errors.New("the sign-in could not be confirmed")
	// ErrStepUpStale: the person authenticated to the admin UI too long ago, or nobody knows when.
	ErrStepUpStale = errors.New("the sign-in is not recent enough")
)

// StepUp is the check before an irreversible action: a session left open on an unlocked screen is
// not enough. It asks now, never a cache, and returns what the credential may do now.
//
// A token session must present its token again (typed), and Config.Fresh must still accept it; an
// error of Fresh is returned as it is, so a module can tell an outage from a refusal.
//
// A launch-ticket session has no token: can-core is asked what the person holds, allowed (when set)
// is applied to that, and can-core must say that the person authenticated to the admin UI (password
// and TOTP, or the identity provider's auth_time) at most StepUpMaxAge ago. A recent launch proves
// nothing, since an admin session left open can launch for hours. An unknown time refuses.
func (m *Manager[P]) StepUp(r *http.Request, s *Session[P], typed string, allowed func(P) bool) (P, error) {
	var none P
	if !s.SSO() {
		if !Equal(typed, s.token) {
			return none, ErrStepUpReenter
		}
		if m.cfg.Fresh == nil {
			return none, ErrStepUpUnconfirmed
		}
		p, err := m.cfg.Fresh(r, s.token)
		if err != nil {
			return none, err
		}
		if m.cfg.Subject(p) != s.Subject || (allowed != nil && !allowed(p)) {
			return none, ErrStepUpUnconfirmed
		}
		return p, nil
	}
	api := m.api()
	if api == nil {
		return none, ErrStepUpUnconfirmed
	}
	v, err := api.RevalidateSession(r.Context(), s.handle)
	if err != nil || !v.Active || v.Subject != s.Subject {
		return none, ErrStepUpUnconfirmed
	}
	p, ok := m.cfg.Grant(v.Subject, v.Permissions)
	if !ok || (allowed != nil && !allowed(p)) {
		return none, ErrStepUpUnconfirmed
	}
	// A minute of clock difference between can-core and this module is tolerated, no more.
	if age := m.cfg.Now().Sub(v.AuthenticatedAt); v.AuthenticatedAt.IsZero() || age > StepUpMaxAge || age < -time.Minute {
		return none, ErrStepUpStale
	}
	return p, nil
}

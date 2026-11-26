// Package users is the administration of people's access (epic E11e, specs/044-users-roles): local
// accounts and their one-time set-up links, disabling and deleting, role assignments and overrides,
// sessions, personal access tokens and single sign-on identities.
//
// It is the one authorization path for these changes: the admin UI and the admin API call it and
// decide nothing themselves. Every change needs rbac.manage; sensitive ones also need a recent
// sign-in or step-up; only an owner changes an owner; nobody gains more than they hold; the last
// owner who can sign in stays; and the audit entry commits with the change.
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// StepUpWindow is how recent a sign-in or step-up must be for a sensitive change.
const StepUpWindow = 10 * time.Minute

// Errors. The HTTP layers map them to statuses; wrapped messages are safe to show.
var (
	ErrForbidden   = errors.New("not allowed")
	ErrStepUp      = errors.New("recent authentication required")
	ErrNotFound    = errors.New("no such user")
	ErrInvalid     = errors.New("invalid request")
	ErrExists      = errors.New("that user name is taken")
	ErrLastOwner   = errors.New("that is the last owner who can sign in")
	ErrSelf        = errors.New("you cannot do that to your own account")
	ErrUnavailable = errors.New("not available")
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// Actor is the signed-in person making a request.
type Actor struct {
	User    store.AdminUser
	Session store.Session
}

func (a Actor) subject() string { return "user:" + a.User.Username }

// Service administers users. DB, Audit and Auth are required. A nil RBAC enforces no permission
// (the single-admin behaviour of earlier versions) and offers no roles; the other dependencies
// switch their part off when nil.
type Service struct {
	DB       *store.DB
	Audit    *audit.Log
	Auth     *auth.Service
	RBAC     *rbac.Service
	PATs     *pat.Service
	SSO      *sso.Service
	Identity *identity.Service
	// Mail sends set-up links by e-mail; nil, or not Ready, shows them to the administrator once.
	Mail Mailer
	// BaseURL is the public address of the admin UI (https://admin.<domain>), from configuration and
	// never from a request. Without it no link is mailed.
	BaseURL string
	Now     func() time.Time
	Log     *slog.Logger
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return s.DB.Now()
}

func (s *Service) ids() *identity.Service {
	if s.Identity != nil {
		return s.Identity
	}
	return &identity.Service{DB: s.DB, Audit: s.Audit, Now: s.Now, Log: s.Log}
}

// Recent reports whether the actor proved who they are recently enough for a sensitive change.
func (s *Service) Recent(a Actor) bool {
	at := a.Session.ProvedAt()
	return !at.IsZero() && s.now().Sub(at) <= StepUpWindow
}

// manage requires rbac.manage.
func (s *Service) manage(ctx context.Context, a Actor) error {
	if s.RBAC == nil {
		return nil
	}
	ok, err := s.RBAC.Check(ctx, a.subject(), rbac.PermManage, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: your roles do not include %s", ErrForbidden, rbac.PermManage)
	}
	return nil
}

// sensitive requires rbac.manage and a recent proof of identity.
func (s *Service) sensitive(ctx context.Context, a Actor) error {
	if err := s.manage(ctx, a); err != nil {
		return err
	}
	if !s.Recent(a) {
		return ErrStepUp
	}
	return nil
}

func (s *Service) account(ctx context.Context, username string) (store.Account, error) {
	if !usernameRe.MatchString(username) && !rbac.ValidSubject("user:"+username) {
		return store.Account{}, ErrNotFound
	}
	a, err := s.DB.AccountByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return a, ErrNotFound
	}
	return a, err
}

// ownerGuard refuses, inside tx, a change to an owner by anyone but an owner.
func ownerGuard(ctx context.Context, tx *sql.Tx, a Actor, subject string) error {
	owner, err := store.HoldsOwner(ctx, tx, subject)
	if err != nil || !owner {
		return err
	}
	mine, err := store.HoldsOwner(ctx, tx, a.subject())
	if err == nil && !mine {
		return fmt.Errorf("%w: only an owner changes an owner", ErrForbidden)
	}
	return err
}

// keepsAnOwner refuses, inside tx, to take away the sign-in of the only owner who has one.
func keepsAnOwner(ctx context.Context, tx *sql.Tx, subject string) error {
	owner, err := store.HoldsOwner(ctx, tx, subject)
	if err != nil || !owner {
		return err
	}
	usable, err := store.IsUsable(ctx, tx, strings.TrimPrefix(subject, "user:"))
	if err != nil || !usable {
		return err
	}
	other, err := store.OtherUsableOwner(ctx, tx, subject)
	if err == nil && !other {
		return ErrLastOwner
	}
	return err
}

// mapErr turns the errors of the services underneath into this package's.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rbac.ErrLastOwner), errors.Is(err, identity.ErrLastOwner), errors.Is(err, store.ErrLastOwner):
		return ErrLastOwner
	case errors.Is(err, rbac.ErrForbidden), errors.Is(err, pat.ErrForbidden), errors.Is(err, identity.ErrForbidden), errors.Is(err, sso.ErrLinkForbidden):
		return fmt.Errorf("%w: %s", ErrForbidden, err.Error())
	case errors.Is(err, rbac.ErrUnknownRole), errors.Is(err, rbac.ErrInvalid), errors.Is(err, identity.ErrInvalid), errors.Is(err, sso.ErrLinkInvalid):
		return fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	case errors.Is(err, identity.ErrUnknown), errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	}
	return err
}

// --- reading ------------------------------------------------------------------------------------

// Status of an account.
const (
	StatusActive   = "active"
	StatusInvited  = "invited" // waiting for its one-time link to be used
	StatusDisabled = "disabled"
)

func status(a store.Account) string {
	switch {
	case !a.DisabledAt.IsZero():
		return StatusDisabled
	case a.Pending && !a.Linked:
		return StatusInvited
	}
	return StatusActive
}

// Summary is one row of the user list.
type Summary struct {
	Username string   `json:"username"`
	Email    string   `json:"email,omitempty"`
	Source   string   `json:"source"`
	Status   string   `json:"status"`
	Roles    []string `json:"roles"`
}

// List returns up to 500 accounts with their roles.
func (s *Service) List(ctx context.Context, a Actor) ([]Summary, error) {
	if err := s.manage(ctx, a); err != nil {
		return nil, err
	}
	accounts, err := s.DB.Accounts(ctx, 500)
	if err != nil {
		return nil, err
	}
	roles := map[string][]string{}
	if s.RBAC != nil {
		views, err := s.RBAC.Users(ctx, 500)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			for _, x := range v.Assignments {
				name := x.Role
				if x.Scope != "" {
					name += " (" + x.Scope + ")"
				}
				roles[v.Username] = append(roles[v.Username], name)
			}
		}
	}
	out := make([]Summary, 0, len(accounts))
	for _, x := range accounts {
		out = append(out, Summary{Username: x.Username, Email: x.Email, Source: x.AuthSource, Status: status(x), Roles: append([]string{}, roles[x.Username]...)})
	}
	return out, nil
}

// RoleInfo says what a role grants.
type RoleInfo struct {
	Name        string   `json:"name"`
	Module      string   `json:"module,omitempty"` // "" for a built-in role
	Permissions []string `json:"permissions"`
	Overrides   []string `json:"overrides,omitempty"`
}

// Assigned is a role an account holds.
type Assigned struct {
	RoleInfo
	Scope     string `json:"scope,omitempty"`
	GrantedBy string `json:"granted_by"`
}

// SessionInfo is one live admin session, identified by its public id.
type SessionInfo struct {
	ID      string    `json:"id"`
	IP      string    `json:"ip"`
	Source  string    `json:"source"`
	Created time.Time `json:"created_at"`
	Expires time.Time `json:"expires_at"`
	Current bool      `json:"current,omitempty"`
}

// LinkPending describes an unused set-up link (never the link itself).
type LinkPending struct {
	Purpose   string    `json:"purpose"`
	CreatedBy string    `json:"created_by"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Detail is everything about one account.
type Detail struct {
	Summary
	Linked      bool               `json:"linked"`
	Assigned    []Assigned         `json:"assignments"`
	Sessions    []SessionInfo      `json:"sessions"`
	Tokens      []pat.View         `json:"tokens"`
	Identity    *sso.IdentityView  `json:"sso_identity,omitempty"`
	PendingLink *LinkPending       `json:"pending_link,omitempty"`
	Grantable   []RoleInfo         `json:"grantable_roles"`
	Linkable    []sso.IdentityView `json:"linkable_identities,omitempty"`
	// Self: this is the caller's own account. Manageable: the caller may change it (an owner is
	// changed only by owners). Credentials: the caller may send a set-up link. ActorOwner: the caller
	// is an owner.
	Self        bool `json:"self"`
	Manageable  bool `json:"manageable"`
	Credentials bool `json:"credentials_manageable"`
	ActorOwner  bool `json:"actor_is_owner"`
}

func roleInfo(r rbac.Role) RoleInfo {
	module, _, found := strings.Cut(r.Name, ":")
	if !found {
		module = ""
	}
	return RoleInfo{Name: r.Name, Module: module, Permissions: r.Permissions, Overrides: r.Overrides}
}

// Roles lists every role with what it grants.
func (s *Service) Roles(ctx context.Context, a Actor) ([]RoleInfo, error) {
	if err := s.manage(ctx, a); err != nil {
		return nil, err
	}
	if s.RBAC == nil {
		return nil, ErrUnavailable
	}
	rs, err := s.RBAC.Roles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RoleInfo, 0, len(rs))
	for _, r := range rs {
		out = append(out, roleInfo(r))
	}
	return out, nil
}

func (s *Service) isOwner(ctx context.Context, subject string) (bool, error) {
	if s.RBAC == nil {
		return true, nil
	}
	return s.RBAC.HasRole(ctx, subject, rbac.Owner)
}

// Detail returns one account.
func (s *Service) Detail(ctx context.Context, a Actor, username string) (Detail, error) {
	if err := s.manage(ctx, a); err != nil {
		return Detail{}, err
	}
	acc, err := s.account(ctx, username)
	if err != nil {
		return Detail{}, err
	}
	subject := "user:" + acc.Username
	d := Detail{
		Summary: Summary{Username: acc.Username, Email: acc.Email, Source: acc.AuthSource, Status: status(acc), Roles: []string{}},
		Linked:  acc.Linked, Self: acc.ID == a.User.ID,
		Assigned: []Assigned{}, Sessions: []SessionInfo{}, Tokens: []pat.View{}, Grantable: []RoleInfo{},
	}
	if d.ActorOwner, err = s.isOwner(ctx, a.subject()); err != nil {
		return d, err
	}
	targetOwner := false
	if s.RBAC != nil {
		if targetOwner, err = s.RBAC.HasRole(ctx, subject, rbac.Owner); err != nil {
			return d, err
		}
	}
	d.Manageable = d.ActorOwner || !targetOwner
	if s.RBAC != nil {
		rs, err := s.RBAC.Roles(ctx)
		if err != nil {
			return d, err
		}
		table := map[string]RoleInfo{}
		for _, r := range rs {
			table[r.Name] = roleInfo(r)
		}
		as, err := s.RBAC.SubjectAssignments(ctx, subject)
		if err != nil {
			return d, err
		}
		for _, x := range as {
			info, ok := table[x.Role]
			if !ok {
				info = RoleInfo{Name: x.Role, Permissions: []string{}}
			}
			d.Assigned = append(d.Assigned, Assigned{RoleInfo: info, Scope: x.Scope, GrantedBy: x.GrantedBy})
			d.Roles = append(d.Roles, x.Role)
		}
		if d.Manageable && !d.Self {
			for _, r := range rs {
				ok, err := s.RBAC.CanGrant(ctx, a.User.Username, r.Name)
				if err != nil {
					return d, err
				}
				if ok {
					d.Grantable = append(d.Grantable, roleInfo(r))
				}
			}
		}
	}
	if d.Manageable && !d.Self && acc.AuthSource == store.SourceLocal {
		if d.Credentials, err = s.covers(ctx, a.User.Username, subject); err != nil {
			return d, err
		}
	}
	sessions, err := s.DB.ListSessions(ctx, acc.ID, s.now())
	if err != nil {
		return d, err
	}
	for _, x := range sessions {
		d.Sessions = append(d.Sessions, SessionInfo{ID: store.PublicSessionID(x), IP: x.IP, Source: x.Source, Created: x.CreatedAt, Expires: x.ExpiresAt, Current: x.IDHash == a.Session.IDHash})
	}
	if s.PATs != nil {
		views, err := s.PATs.ListFor(ctx, a.subject(), true)
		if err != nil {
			return d, err
		}
		for _, v := range views {
			if v.Subject == subject && !v.Revoked && !v.Expired {
				d.Tokens = append(d.Tokens, v)
			}
		}
	}
	link, err := s.DB.SetupLinkOf(ctx, acc.ID, s.now())
	if err == nil {
		d.PendingLink = &LinkPending{Purpose: link.Purpose, CreatedBy: link.CreatedBy, ExpiresAt: link.ExpiresAt}
	} else if !errors.Is(err, store.ErrNotFound) {
		return d, err
	}
	if s.SSO != nil {
		ids, err := s.SSO.Identities(ctx)
		if err != nil {
			return d, err
		}
		for _, id := range ids {
			switch {
			case id.Username == acc.Username:
				found := id
				d.Identity = &found
			case !id.Linked && id.Username != a.User.Username && acc.AuthSource == store.SourceLocal && d.ActorOwner:
				d.Linkable = append(d.Linkable, id)
			}
		}
		if d.Identity != nil {
			d.Linkable = nil
		}
	}
	return d, nil
}

// covers reports whether actor holds all that the target holds.
func (s *Service) covers(ctx context.Context, actor, target string) (bool, error) {
	if s.RBAC == nil {
		return true, nil
	}
	return s.RBAC.Covers(ctx, actor, target)
}

// --- disabling, enabling, deleting --------------------------------------------------------------

// Disable ends everything the account holds at once and stops it signing in.
func (s *Service) Disable(ctx context.Context, a Actor, username string) (identity.Result, error) {
	if err := s.sensitive(ctx, a); err != nil {
		return identity.Result{}, err
	}
	if username == a.User.Username {
		return identity.Result{}, ErrSelf
	}
	res, err := s.ids().RevokeNow(ctx, a.subject(), "user:"+username)
	return res, mapErr(err)
}

// Enable re-enables a disabled account (owners only). Tokens and sessions do not come back.
func (s *Service) Enable(ctx context.Context, a Actor, username string) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	return mapErr(s.ids().Enable(ctx, a.subject(), "user:"+username))
}

// Delete removes the account, its roles and everything it can sign in or be reached with. The name
// is never used again.
func (s *Service) Delete(ctx context.Context, a Actor, username string) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	if username == a.User.Username {
		return ErrSelf
	}
	err := s.DB.InTx(ctx, func(tx *sql.Tx) error {
		acc, err := store.AccountTx(ctx, tx, username)
		if err != nil {
			return err
		}
		subject := "user:" + acc.Username
		if err := ownerGuard(ctx, tx, a, subject); err != nil {
			return err
		}
		owner, err := store.HoldsOwner(ctx, tx, subject)
		if err != nil {
			return err
		}
		if owner {
			if other, err := store.OtherUsableOwner(ctx, tx, subject); err != nil {
				return err
			} else if !other {
				return ErrLastOwner
			}
		}
		if err := s.DB.DeleteAccount(ctx, tx, acc); err != nil {
			return err
		}
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.delete", Target: subject, Detail: map[string]any{"source": acc.AuthSource}})
		return err
	})
	if s.RBAC != nil {
		s.RBAC.Invalidate()
	}
	return mapErr(err)
}

// --- roles --------------------------------------------------------------------------------------

func (s *Service) roleChange(ctx context.Context, a Actor, username string, change func(actor, subject string) error) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	if s.RBAC == nil {
		return ErrUnavailable
	}
	acc, err := s.account(ctx, username)
	if err != nil {
		return err
	}
	return change(a.User.Username, "user:"+acc.Username)
}

// Grant assigns a role. rbac refuses what the actor does not hold and owner changes by non-owners.
func (s *Service) Grant(ctx context.Context, a Actor, username, role, scope string) error {
	return mapErr(s.roleChange(ctx, a, username, func(actor, subject string) error {
		return s.RBAC.Grant(ctx, actor, subject, role, scope)
	}))
}

// Revoke removes a role; the person's tokens and sessions end with it.
func (s *Service) Revoke(ctx context.Context, a Actor, username, role, scope string) error {
	err := s.roleChange(ctx, a, username, func(actor, subject string) error {
		err := s.RBAC.Revoke(ctx, actor, subject, role, scope)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: the user does not hold that role", ErrNotFound)
		}
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return err
	}
	return mapErr(err)
}

// Override changes one permission of a role relative to its manifest (owners only).
func (s *Service) Override(ctx context.Context, a Actor, role, permission, effect string) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	if s.RBAC == nil {
		return ErrUnavailable
	}
	return mapErr(s.RBAC.Override(ctx, a.User.Username, role, permission, effect))
}

// Permissions lists every permission an override can name: the platform's and the enabled modules'.
func (s *Service) Permissions(ctx context.Context, a Actor) ([]string, error) {
	if err := s.manage(ctx, a); err != nil {
		return nil, err
	}
	out := []string{rbac.PermView, rbac.PermAudit, rbac.PermManage, rbac.PermApprovalAdmin, rbac.PermNotifyAdmin, rbac.PermOperate, rbac.PermSetup, rbac.PermPurge}
	mods, err := s.DB.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		if m.State != store.StateEnabled {
			continue
		}
		for _, p := range m.Manifest.Permissions {
			if !config.IsReservedPermission(p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// --- sessions and tokens ------------------------------------------------------------------------

// target is the account of a change to sessions or tokens: it exists and the actor may change it.
func (s *Service) target(ctx context.Context, a Actor, username string) (store.Account, error) {
	if err := s.manage(ctx, a); err != nil {
		return store.Account{}, err
	}
	acc, err := s.account(ctx, username)
	if err != nil {
		return acc, err
	}
	if s.RBAC != nil && acc.ID != a.User.ID {
		owner, err := s.RBAC.HasRole(ctx, "user:"+acc.Username, rbac.Owner)
		if err != nil {
			return acc, err
		}
		if owner {
			mine, err := s.RBAC.HasRole(ctx, a.subject(), rbac.Owner)
			if err != nil {
				return acc, err
			}
			if !mine {
				return acc, fmt.Errorf("%w: only an owner changes an owner", ErrForbidden)
			}
		}
	}
	return acc, nil
}

// RevokeSession ends one of the account's sessions and what it launched into modules.
func (s *Service) RevokeSession(ctx context.Context, a Actor, username, publicID string) error {
	acc, err := s.target(ctx, a, username)
	if err != nil {
		return err
	}
	ok, err := s.DB.RevokeSession(ctx, acc.ID, publicID, func(tx *sql.Tx) error {
		_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.session.revoke", Target: "user:" + acc.Username, Detail: map[string]any{"session": publicID}})
		return err
	})
	if err == nil && !ok {
		return fmt.Errorf("%w: no such session", ErrNotFound)
	}
	return err
}

// RevokeSessions ends all of the account's sessions, in the admin UI and in modules.
func (s *Service) RevokeSessions(ctx context.Context, a Actor, username string) error {
	acc, err := s.target(ctx, a, username)
	if err != nil {
		return err
	}
	subject := "user:" + acc.Username
	return s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := store.EndSessions(ctx, tx, subject, s.now()); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.sessions.revoke", Target: subject})
		return err
	})
}

// RevokeToken ends one of the account's personal access tokens.
func (s *Service) RevokeToken(ctx context.Context, a Actor, username, id string) error {
	acc, err := s.target(ctx, a, username)
	if err != nil {
		return err
	}
	if s.PATs == nil {
		return ErrUnavailable
	}
	t, err := s.DB.GetAPIToken(ctx, id)
	if err != nil || t.Subject != "user:"+acc.Username {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: no such token", ErrNotFound)
		}
		return err
	}
	return mapErr(s.PATs.Revoke(ctx, a.subject(), id, true))
}

// --- single sign-on identities ------------------------------------------------------------------

// LinkIdentity binds an identity to a local account (owners only).
func (s *Service) LinkIdentity(ctx context.Context, a Actor, identityID int64, username string) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	if s.SSO == nil {
		return ErrUnavailable
	}
	return mapErr(s.SSO.Link(ctx, a.User.Username, identityID, username))
}

// UnlinkIdentity removes an identity (owners only).
func (s *Service) UnlinkIdentity(ctx context.Context, a Actor, identityID int64) error {
	if err := s.sensitive(ctx, a); err != nil {
		return err
	}
	if s.SSO == nil {
		return ErrUnavailable
	}
	return mapErr(s.SSO.Unlink(ctx, a.User.Username, identityID))
}

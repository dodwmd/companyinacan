// Package rbac is can-core's role-based access control (SPEC §3.7, epic E11a).
//
// Modules declare permissions and default role grants in module.yaml (`permissions`, `roles`);
// the operator assigns roles to subjects and may override a role's grants. Subjects are strings:
// "user:<admin username>" for people who sign in to the admin UI and "party:<id>" for everyone
// else. Roles come from the manifests of enabled modules plus the built-in roles below; the
// built-in `owner` role is the break-glass role and holds every permission.
//
// Check is what modules call (through the module API) and what the admin UI enforces. It fails
// closed: any error, unknown subject, unknown permission or unscoped question about a scoped
// grant answers "no". Answers are cached per subject for DefaultTTL; every change made through
// this package invalidates the cache at once, a change made by re-registering a module becomes
// visible within the TTL.
package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Owner is the break-glass role: every permission, cannot be edited, never left without a holder.
const Owner = "owner"

// Wildcard is the permission of the owner role.
const Wildcard = "*"

// Override effects.
const (
	EffectGrant  = "grant"
	EffectRevoke = "revoke"
	EffectReset  = "reset" // removes the override, restoring the manifest default
)

// Platform permissions enforced by can-core itself.
const (
	PermView          = "platform.view"       // module list, events, leads, dashboard
	PermAudit         = "platform.audit"      // read, verify and export the audit log
	PermManage        = "rbac.manage"         // assign roles, override grants
	PermApprovalAdmin = "approvals.admin"     // see and cancel any approval request
	PermNotifyAdmin   = "notifications.admin" // delivery log of everyone's notifications
	PermOperate       = "platform.operate"    // requeue deliveries, change lead status
)

// builtin roles besides owner. Least privilege: a new admin user holds none of them.
var builtin = map[string][]string{
	Owner:      {Wildcard},
	"operator": {PermView, PermOperate, PermAudit, PermApprovalAdmin, PermNotifyAdmin},
	"auditor":  {PermView, PermAudit},
}

// DefaultTTL bounds how long a cached answer lives.
const DefaultTTL = 30 * time.Second

const maxCacheEntries = 2048

// Errors.
var (
	ErrUnknownRole = errors.New("unknown role")
	ErrLastOwner   = errors.New("the last owner cannot be removed")
	ErrInvalid     = errors.New("invalid argument")
	// ErrForbidden is returned when the actor may not make the change (owner changes need an owner,
	// nobody grants themselves a role).
	ErrForbidden = errors.New("not allowed to make this change")
)

// CLIActor is the audit actor of `can-core admin roles`, the break-glass path inside the pod: it
// may do what owners do, because it exists for installations that have no working owner.
const CLIActor = "cli"

var (
	subjectRe = regexp.MustCompile(`^(user|party):[A-Za-z0-9_.@-]{1,64}$`)
	roleRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	scopeRe   = regexp.MustCompile(`^[A-Za-z0-9_./:-]{1,128}(/\*)?$`)
	permRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9-]+)+$`)
)

// ValidSubject reports whether s is a well-formed subject.
func ValidSubject(s string) bool { return subjectRe.MatchString(s) }

// Appender is the audit write interface (audit.Log satisfies it).
type Appender interface {
	AppendTx(ctx context.Context, tx *sql.Tx, e audit.Entry) (audit.Record, error)
}

// Options configure a Service.
type Options struct {
	Now func() time.Time
	TTL time.Duration
}

// Assignment is one role held by a subject.
type Assignment struct {
	Subject   string    `json:"subject"`
	Role      string    `json:"role"`
	Scope     string    `json:"scope,omitempty"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
}

// Role is a role with its effective permissions.
type Role struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	Sources     []string `json:"sources"` // module ids and "builtin"
	Overrides   []string `json:"overrides,omitempty"`
}

// Service is the RBAC service.
type Service struct {
	db    *store.DB
	audit Appender
	now   func() time.Time
	ttl   time.Duration

	mu     sync.Mutex
	dataV  int64                      // PRAGMA data_version when the caches were filled
	roles  map[string]map[string]bool // role -> permissions, nil when stale
	rolesT time.Time
	rolesS map[string][]string // role -> sources
	subj   map[string]subjEntry
}

type subjEntry struct {
	at      time.Time
	assigns []Assignment
}

// New returns a Service.
func New(db *store.DB, a Appender, opt Options) *Service {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.TTL == 0 {
		opt.TTL = DefaultTTL
	}
	return &Service{db: db, audit: a, now: opt.Now, ttl: opt.TTL, subj: map[string]subjEntry{}}
}

// Invalidate drops every cached answer.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.roles, s.subj = nil, map[string]subjEntry{}
	s.mu.Unlock()
}

// loadRoles builds the role table from builtin roles, enabled modules' manifests and overrides.
func (s *Service) loadRoles(ctx context.Context) (map[string]map[string]bool, map[string][]string, error) {
	roles := map[string]map[string]bool{}
	sources := map[string][]string{}
	add := func(role, perm, src string) {
		if roles[role] == nil {
			roles[role] = map[string]bool{}
		}
		roles[role][perm] = true
		seen := false
		for _, x := range sources[role] {
			seen = seen || x == src
		}
		if !seen {
			sources[role] = append(sources[role], src)
		}
	}
	for r, ps := range builtin {
		for _, p := range ps {
			add(r, p, "builtin")
		}
	}
	mods, err := s.db.ListModules(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range mods {
		if m.State != store.StateEnabled {
			continue
		}
		declared := map[string]bool{}
		for _, p := range m.Manifest.Permissions {
			declared[p] = true
		}
		for r, ps := range m.Manifest.Roles {
			if config.IsBuiltinRole(r) {
				continue // module roles never merge into built-in roles (owner, operator, auditor)
			}
			if roles[r] == nil {
				roles[r] = map[string]bool{}
				sources[r] = append(sources[r], m.ID)
			}
			for _, p := range ps {
				// Defence in depth behind validate and the registry: a role grants only
				// permissions its own manifest declares, never platform ones.
				if declared[p] && !config.IsReservedPermission(p) {
					add(r, p, m.ID)
				}
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role, permission, effect FROM rbac_overrides ORDER BY role, permission LIMIT 5000`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r, p, eff string
		if err := rows.Scan(&r, &p, &eff); err != nil {
			return nil, nil, err
		}
		if r == Owner {
			continue
		}
		if eff == EffectGrant {
			add(r, p, "operator")
		} else if roles[r] != nil {
			delete(roles[r], p)
		}
	}
	return roles, sources, rows.Err()
}

// sync drops every cache when another connection (the CLI, `can apply`) changed the database since
// the caches were filled: PRAGMA data_version changes exactly then, so revocations made outside
// this process are seen at once, not after the TTL.
func (s *Service) sync(ctx context.Context) error {
	var v int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v); err != nil {
		return err
	}
	s.mu.Lock()
	if v != s.dataV {
		s.dataV = v
		s.roles, s.subj = nil, map[string]subjEntry{}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) roleTable(ctx context.Context) (map[string]map[string]bool, map[string][]string, error) {
	s.mu.Lock()
	if s.roles != nil && s.now().Sub(s.rolesT) < s.ttl {
		r, src := s.roles, s.rolesS
		s.mu.Unlock()
		return r, src, nil
	}
	s.mu.Unlock()
	r, src, err := s.loadRoles(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.roles, s.rolesS, s.rolesT = r, src, s.now()
	s.mu.Unlock()
	return r, src, nil
}

func (s *Service) assignments(ctx context.Context, subject string) ([]Assignment, error) {
	if err := s.sync(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if e, ok := s.subj[subject]; ok && s.now().Sub(e.at) < s.ttl {
		s.mu.Unlock()
		return e.assigns, nil
	}
	s.mu.Unlock()
	as, err := s.queryAssignments(ctx, `WHERE subject=?`, subject)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.subj) >= maxCacheEntries {
		s.subj = map[string]subjEntry{}
	}
	s.subj[subject] = subjEntry{at: s.now(), assigns: as}
	s.mu.Unlock()
	return as, nil
}

func (s *Service) queryAssignments(ctx context.Context, where string, args ...any) ([]Assignment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, role, scope, granted_by, granted_at FROM rbac_assignments `+where+` ORDER BY subject, role, scope LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Assignment
	for rows.Next() {
		var a Assignment
		var at int64
		if err := rows.Scan(&a.Subject, &a.Role, &a.Scope, &a.GrantedBy, &at); err != nil {
			return nil, err
		}
		a.GrantedAt = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func scopeMatches(scope, resource string) bool {
	switch {
	case scope == "":
		return true
	case resource == "":
		return false
	case strings.HasSuffix(scope, "/*"):
		return strings.HasPrefix(resource, strings.TrimSuffix(scope, "*")) && len(resource) > len(scope)-1
	default:
		return resource == scope
	}
}

// Check answers whether subject holds permission on resource (empty resource = unscoped question).
// Errors deny.
func (s *Service) Check(ctx context.Context, subject, permission, resource string) (bool, error) {
	if subject == "" || permission == "" {
		return false, nil
	}
	as, err := s.assignments(ctx, subject)
	if err != nil {
		return false, err
	}
	if len(as) == 0 {
		return false, nil
	}
	roles, _, err := s.roleTable(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		if !scopeMatches(a.Scope, resource) {
			continue
		}
		if ps := roles[a.Role]; ps[Wildcard] || ps[permission] {
			return true, nil
		}
	}
	return false, nil
}

// HasRole reports whether subject holds the role without a scope. Scoped assignments answer
// resource questions in Check only; approver resolution and notification fan-out use this.
func (s *Service) HasRole(ctx context.Context, subject, role string) (bool, error) {
	as, err := s.assignments(ctx, subject)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		if a.Role == role && a.Scope == "" {
			return true, nil
		}
	}
	return false, nil
}

// SubjectsWithRole lists up to limit subjects holding the role without a scope, in subject order.
func (s *Service) SubjectsWithRole(ctx context.Context, role string, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT subject FROM rbac_assignments WHERE role=? AND scope='' ORDER BY subject LIMIT ?`, role, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SubjectAssignments lists the roles of one subject.
func (s *Service) SubjectAssignments(ctx context.Context, subject string) ([]Assignment, error) {
	return s.assignments(ctx, subject)
}

// Roles lists every role with its effective permissions.
func (s *Service) Roles(ctx context.Context) ([]Role, error) {
	roles, sources, err := s.roleTable(ctx)
	if err != nil {
		return nil, err
	}
	ov, err := s.overrides(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(roles))
	for name, ps := range roles {
		r := Role{Name: name, Permissions: []string{}, Sources: append([]string{}, sources[name]...), Overrides: ov[name]}
		for p := range ps {
			r.Permissions = append(r.Permissions, p)
		}
		sort.Strings(r.Permissions)
		sort.Strings(r.Sources)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Service) overrides(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, effect || ' ' || permission FROM rbac_overrides ORDER BY role, permission LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var r, o string
		if err := rows.Scan(&r, &o); err != nil {
			return nil, err
		}
		out[r] = append(out[r], o)
	}
	return out, rows.Err()
}

func (s *Service) roleExists(ctx context.Context, role string) error {
	roles, _, err := s.roleTable(ctx)
	if err != nil {
		return err
	}
	if _, ok := roles[role]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	return nil
}

// Grant assigns role to subject (idempotent). scope is "" for everything or a resource prefix
// ending in /*.
func (s *Service) Grant(ctx context.Context, actor, subject, role, scope string) error {
	if !subjectRe.MatchString(subject) || !roleRe.MatchString(role) || (scope != "" && !scopeRe.MatchString(scope)) {
		return ErrInvalid
	}
	if role == Owner && scope != "" {
		return fmt.Errorf("%w: the owner role cannot be scoped", ErrInvalid)
	}
	if err := s.roleExists(ctx, role); err != nil {
		return err
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if actor != CLIActor && subject == "user:"+actor {
			return fmt.Errorf("%w: nobody assigns roles to themselves", ErrForbidden)
		}
		if role == Owner {
			if err := s.requireOwner(ctx, tx, actor); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rbac_assignments(subject, role, scope, granted_by, granted_at) VALUES (?,?,?,?,?)`,
			subject, role, scope, actor, s.db.Now().Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "rbac.grant", Target: subject, Detail: map[string]any{"role": role, "scope": scope}})
		return err
	})
	s.Invalidate()
	return err
}

// requireOwner fails unless the actor is an unscoped owner (read inside the transaction, so
// concurrent changes cannot slip between the check and the write). The CLI actor passes: it is the
// recovery path for installations without a working owner.
func (s *Service) requireOwner(ctx context.Context, tx *sql.Tx, actor string) error {
	if actor == CLIActor {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE subject=? AND role=? AND scope=''`, "user:"+actor, Owner).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: only an owner changes owners", ErrForbidden)
	}
	return nil
}

// Revoke removes an assignment. The last holder of owner cannot be removed.
func (s *Service) Revoke(ctx context.Context, actor, subject, role, scope string) error {
	if !subjectRe.MatchString(subject) || !roleRe.MatchString(role) {
		return ErrInvalid
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if role == Owner {
			if err := s.requireOwner(ctx, tx, actor); err != nil {
				return err
			}
			var others int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT subject) FROM rbac_assignments WHERE role=? AND scope='' AND subject<>?`, Owner, subject).Scan(&others); err != nil {
				return err
			}
			var mine int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE role=? AND scope=? AND subject=?`, Owner, scope, subject).Scan(&mine); err != nil {
				return err
			}
			if mine > 0 && others == 0 {
				return ErrLastOwner
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM rbac_assignments WHERE subject=? AND role=? AND scope=?`, subject, role, scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.ErrNotFound
		}
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "rbac.revoke", Target: subject, Detail: map[string]any{"role": role, "scope": scope}})
		return err
	})
	s.Invalidate()
	return err
}

// Override changes one grant of a role relative to the manifest defaults.
func (s *Service) Override(ctx context.Context, actor, role, permission, effect string) error {
	if role == Owner {
		return fmt.Errorf("%w: the owner role cannot be edited", ErrInvalid)
	}
	if !roleRe.MatchString(role) || !permRe.MatchString(permission) || (effect != EffectGrant && effect != EffectRevoke && effect != EffectReset) {
		return ErrInvalid
	}
	if err := s.roleExists(ctx, role); err != nil {
		return err
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireOwner(ctx, tx, actor); err != nil {
			return err
		}
		var err error
		if effect == EffectReset {
			_, err = tx.ExecContext(ctx, `DELETE FROM rbac_overrides WHERE role=? AND permission=?`, role, permission)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO rbac_overrides(role, permission, effect, set_by, set_at) VALUES (?,?,?,?,?)
				ON CONFLICT(role, permission) DO UPDATE SET effect=excluded.effect, set_by=excluded.set_by, set_at=excluded.set_at`,
				role, permission, effect, actor, s.db.Now().Unix())
		}
		if err != nil {
			return err
		}
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "rbac.override", Target: role, Detail: map[string]any{"permission": permission, "effect": effect}})
		return err
	})
	s.Invalidate()
	return err
}

// UserView is an admin user with their assignments.
type UserView struct {
	Username    string       `json:"username"`
	Subject     string       `json:"subject"`
	Assignments []Assignment `json:"assignments"`
}

// Users lists up to limit admin users with their role assignments (two queries, no per-user loop).
func (s *Service) Users(ctx context.Context, limit int) ([]UserView, error) {
	if limit < 1 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT username FROM admin_users ORDER BY username LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []UserView
	idx := map[string]int{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			_ = rows.Close()
			return nil, err
		}
		idx["user:"+u] = len(out)
		out = append(out, UserView{Username: u, Subject: "user:" + u, Assignments: []Assignment{}})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	as, err := s.queryAssignments(ctx, `WHERE subject LIKE 'user:%'`)
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		if i, ok := idx[a.Subject]; ok {
			out[i].Assignments = append(out[i].Assignments, a)
		}
	}
	return out, nil
}

package sso

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Mapping is one group-to-roles row of the status page.
type Mapping struct {
	Group string   `json:"group"`
	Roles []string `json:"roles"`
}

// Status is what the settings page and API show. It holds no secret: the client secret appears only
// as the name of its Secret.
type Status struct {
	Configured           bool      `json:"configured"`
	Enabled              bool      `json:"enabled"`
	Error                string    `json:"error,omitempty"`
	Issuer               string    `json:"issuer,omitempty"`
	ClientID             string    `json:"client_id,omitempty"`
	ClientSecretRef      string    `json:"client_secret_ref,omitempty"`
	RedirectURI          string    `json:"redirect_uri,omitempty"`
	Scopes               []string  `json:"scopes,omitempty"`
	Mappings             []Mapping `json:"group_roles"`
	AllowedEmailDomains  []string  `json:"allowed_email_domains,omitempty"`
	RequireEmailVerified bool      `json:"require_email_verified"`
	RequireAMR           []string  `json:"require_amr,omitempty"`
	RequireMappedRole    bool      `json:"require_mapped_role"`
	LocalLogin           string    `json:"local_login,omitempty"`
	LocalLoginEffective  string    `json:"local_login_effective,omitempty"`
	RevalidateInterval   string    `json:"revalidate_interval,omitempty"`
	RPLogout             bool      `json:"rp_logout"`
	InsecureDev          bool      `json:"allow_insecure_dev"`
	Identities           int       `json:"identities"`
}

// Status reports the configuration as the service currently sees it.
func (s *Service) Status(ctx context.Context) (Status, error) {
	c := s.load(ctx)
	var st Status
	st.Mappings = []Mapping{}
	if c.off {
		return st, nil
	}
	st.Configured, st.Enabled = true, c.err == nil
	if c.err != nil {
		st.Error = c.err.Error()
	}
	cfg := c.cfg
	st.Issuer, st.ClientID, st.ClientSecretRef = cfg.Issuer, cfg.ClientID, cfg.ClientSecretRef
	st.RedirectURI = strings.TrimRight(cfg.RedirectBaseURL, "/") + "/auth/oidc/callback"
	st.Scopes, st.AllowedEmailDomains, st.RequireAMR = cfg.Scopes, cfg.AllowedEmailDomains, cfg.RequireAMR
	st.RequireEmailVerified, st.RequireMappedRole, st.RPLogout, st.InsecureDev = cfg.EmailVerifiedRequired(), cfg.RequireMappedRole, cfg.RPLogout, cfg.AllowInsecureDev
	st.LocalLogin = cfg.LocalLogin
	if st.LocalLogin == "" {
		st.LocalLogin = config.LocalLoginAll
	}
	st.LocalLoginEffective = st.LocalLogin
	if st.LocalLogin == config.LocalLoginDisabled && c.err != nil {
		st.LocalLoginEffective = config.LocalLoginOwnerOnly // SSO is failing: owners keep the break-glass login
	} else if st.LocalLogin == config.LocalLoginDisabled {
		if n, err := s.DB.SSOOwnerCount(ctx); err != nil || n == 0 {
			st.LocalLoginEffective = config.LocalLoginOwnerOnly // safety net: no SSO owner exists yet
		}
	}
	st.RevalidateInterval = cfg.RevalidateEvery().String()
	groups := make([]string, 0, len(cfg.GroupRoles))
	for g := range cfg.GroupRoles {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		st.Mappings = append(st.Mappings, Mapping{Group: g, Roles: cfg.GroupRoles[g]})
	}
	ids, err := s.DB.ListSSOIdentities(ctx, 500)
	if err != nil {
		return st, err
	}
	st.Identities = len(ids)
	return st, nil
}

// TestResult is the outcome of the discovery test.
type TestResult struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	Issuer   string `json:"issuer,omitempty"`
	Keys     int    `json:"keys"`
	Endpoint string `json:"authorization_host,omitempty"`
}

// TestDiscovery fetches the provider's discovery document and key set exactly as sign-in would
// (https, public addresses, no redirects, size caps) and reports the result. It never uses or shows
// the client secret and is audited.
func (s *Service) TestDiscovery(ctx context.Context, actor string) (TestResult, error) {
	c := s.load(ctx)
	if c.off {
		return TestResult{}, ErrNotConfigured
	}
	cfg := c.cfg
	var res TestResult
	if issues := config.ValidateSSO(&cfg, ""); len(issues) > 0 && c.prov == nil {
		res = TestResult{Message: "configuration invalid: " + issues[0].Message}
	} else {
		p := NewProvider(cfg, "-", s.Client, s.Now)
		d, err := p.FetchDiscovery(ctx)
		if err == nil {
			var keys []Key
			keys, err = p.fetchKeysFrom(ctx, d)
			res.Keys = len(keys)
		}
		if err != nil {
			res.Message = Sanitise(err.Error())
		} else {
			res.OK, res.Message, res.Issuer = true, "discovery and signing keys fetched", d.Issuer
			res.Endpoint = hostName(d.AuthorizationEndpoint)
		}
	}
	if _, err := s.Audit.Append(ctx, audit.Entry{Actor: actor, Action: "sso.test", Target: "sso", Detail: map[string]any{"ok": res.OK}}); err != nil {
		return res, err
	}
	return res, nil
}

func hostName(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// Sanitise shortens an error to something safe to show.
func Sanitise(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (p *Provider) fetchKeysFrom(ctx context.Context, d *Discovery) ([]Key, error) {
	var raw []byte
	if err := call(ctx, p.client, "GET", d.JWKSURI, nil, [2]string{}, "", maxJWKSBytes, &raw); err != nil {
		return nil, err
	}
	return ParseJWKS(raw)
}

// IdentityView is an identity as the admin pages show it.
type IdentityView struct {
	ID         int64    `json:"id"`
	Username   string   `json:"username"`
	Email      string   `json:"email,omitempty"`
	Name       string   `json:"name,omitempty"`
	Groups     []string `json:"groups"`
	Linked     bool     `json:"linked"`
	LinkedBy   string   `json:"linked_by,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastLogin  string   `json:"last_login"`
	Roles      []string `json:"roles"`
	ExternalID string   `json:"external_subject"`
}

// Identities lists the known external identities with their effective roles.
func (s *Service) Identities(ctx context.Context) ([]IdentityView, error) {
	ids, err := s.DB.ListSSOIdentities(ctx, 500)
	if err != nil {
		return nil, err
	}
	out := make([]IdentityView, 0, len(ids))
	for _, i := range ids {
		v := IdentityView{ID: i.ID, Username: i.Username, Email: i.Email, Name: i.Name, Linked: i.UserSource == store.SourceLocal, LinkedBy: i.LinkedBy,
			CreatedAt: i.CreatedAt.UTC().Format("2006-01-02 15:04"), LastLogin: i.LastLogin.UTC().Format("2006-01-02 15:04"), Groups: []string{}, Roles: []string{}, ExternalID: "oidc:" + i.IssuerHash + ":" + i.Subject}
		_ = json.Unmarshal([]byte(i.GroupsJSON), &v.Groups)
		if s.RBAC != nil {
			if as, err := s.RBAC.SubjectAssignments(ctx, "user:"+i.Username); err == nil {
				for _, a := range as {
					v.Roles = append(v.Roles, a.Role)
				}
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// Errors of linking.
var (
	ErrLinkForbidden = errors.New("only an owner links or unlinks identities")
	ErrLinkInvalid   = errors.New("cannot link: unknown identity or user, the user is not a local account, or already has an identity")
)

func (s *Service) requireOwner(ctx context.Context, actor string) error {
	if s.RBAC == nil {
		return nil
	}
	ok, err := s.RBAC.HasRole(ctx, "user:"+actor, rbac.Owner)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLinkForbidden
	}
	return nil
}

// Link binds an external identity to an existing local administrator. This is the only way an
// identity reaches an account that existed before it: never by email. Owner only; audited in the
// same transaction. The user the identity was provisioned for is deleted.
func (s *Service) Link(ctx context.Context, actor string, identityID int64, username string) error {
	s.init()
	if err := s.requireOwner(ctx, actor); err != nil {
		return err
	}
	target, err := s.DB.AdminByUsername(ctx, username)
	if err != nil {
		return ErrLinkInvalid
	}
	err = s.DB.LinkSSOIdentity(ctx, identityID, target.ID, actor, func(tx *sql.Tx, from store.SSOIdentity, t store.AdminUser) error {
		_, aerr := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "sso.link", Target: "user:" + t.Username, Detail: map[string]any{
			"external_subject": "oidc:" + from.IssuerHash + ":" + from.Subject, "replaced_user": from.Username,
		}})
		return aerr
	})
	if errors.Is(err, store.ErrSSOConflict) || errors.Is(err, store.ErrNotFound) {
		return ErrLinkInvalid
	}
	if s.RBAC != nil {
		s.RBAC.Invalidate()
	}
	return err
}

// Unlink removes an identity (a provisioned user is deleted with it; a linked local account stays).
func (s *Service) Unlink(ctx context.Context, actor string, identityID int64) error {
	s.init()
	if err := s.requireOwner(ctx, actor); err != nil {
		return err
	}
	err := s.DB.UnlinkSSOIdentity(ctx, identityID, func(tx *sql.Tx, id store.SSOIdentity) error {
		if id.UserSource == store.SourceSSO && s.RBAC != nil {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT subject) FROM rbac_assignments WHERE role='owner' AND scope='' AND subject<>?`, "user:"+id.Username).Scan(&n); err != nil {
				return err
			}
			var mine int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE role='owner' AND scope='' AND subject=?`, "user:"+id.Username).Scan(&mine); err != nil {
				return err
			}
			if mine > 0 && n == 0 {
				return rbac.ErrLastOwner
			}
		}
		_, aerr := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "sso.unlink", Target: "user:" + id.Username, Detail: map[string]any{"external_subject": "oidc:" + id.IssuerHash + ":" + id.Subject, "deleted_user": id.UserSource == store.SourceSSO}})
		return aerr
	})
	if errors.Is(err, store.ErrNotFound) {
		return ErrLinkInvalid
	}
	if s.RBAC != nil {
		s.RBAC.Invalidate()
	}
	return err
}

// UserInfo is the answer for an end-user session token on the module API.
type UserInfo struct {
	Active  bool     `json:"active"`
	Kind    string   `json:"kind,omitempty"`
	Sub     string   `json:"sub,omitempty"`
	Subject string   `json:"subject,omitempty"` // the RBAC subject, user:<name>
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Roles   []string `json:"roles,omitempty"`
	AMR     []string `json:"amr,omitempty"`
}

// IntrospectSession resolves an end-user session token to the identity behind it. Only sessions
// created by single sign-on are described; anything else, an expired session or a revoked one is
// simply inactive.
func (s *Service) IntrospectSession(ctx context.Context, token string) (UserInfo, error) {
	sess, user, err := s.Auth.Authenticate(ctx, token)
	if err != nil {
		return UserInfo{}, nil //nolint:nilerr // inactive, not an error
	}
	if sess.Source != store.SourceSSO {
		return UserInfo{}, nil
	}
	ident, err := s.DB.SSOIdentityByUser(ctx, user.ID)
	if err != nil {
		return UserInfo{}, nil //nolint:nilerr
	}
	ui := UserInfo{Active: true, Kind: "user", Sub: "oidc:" + ident.IssuerHash + ":" + ident.Subject, Subject: "user:" + user.Username, Email: ident.Email, Name: ident.Name, Groups: []string{}, Roles: []string{}, AMR: []string{}}
	_ = json.Unmarshal([]byte(ident.GroupsJSON), &ui.Groups)
	_ = json.Unmarshal([]byte(ident.AMRJSON), &ui.AMR)
	if s.RBAC != nil {
		as, err := s.RBAC.SubjectAssignments(ctx, ui.Subject)
		if err != nil {
			return UserInfo{}, err
		}
		seen := map[string]bool{}
		for _, a := range as {
			if a.Scope == "" && !seen[a.Role] {
				seen[a.Role] = true
				ui.Roles = append(ui.Roles, a.Role)
			}
		}
		sort.Strings(ui.Roles)
	}
	return ui, nil
}

// --- client secret readers ---------------------------------------------------------------------

// SecretReader resolves client_secret_ref ("name" or "name/key") to the secret value.
type SecretReader interface {
	Secret(ctx context.Context, ref string) (string, error)
}

const maxSecretBytes = 4 << 10

func splitRef(ref string) (name, key string) {
	name, key, _ = strings.Cut(ref, "/")
	if key == "" {
		key = "client-secret"
	}
	return
}

func cleanSecret(v string) (string, error) {
	v = strings.TrimRight(v, "\r\n")
	if v == "" || len(v) > maxSecretBytes {
		return "", errors.New("secret is empty or too large")
	}
	return v, nil
}

// DirSecrets reads a Secret mounted as a directory: <Dir>/<name>/<key>.
type DirSecrets struct{ Dir string }

// Secret implements SecretReader.
func (d DirSecrets) Secret(_ context.Context, ref string) (string, error) {
	if !config.ValidSecretRef(ref) {
		return "", errors.New("invalid secret reference")
	}
	name, key := splitRef(ref)
	f, err := os.Open(filepath.Join(d.Dir, name, key)) //nolint:gosec // name and key are validated
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", err
	}
	return cleanSecret(string(b))
}

// StaticSecrets is a SecretReader for tests.
type StaticSecrets map[string]string

// Secret implements SecretReader.
func (m StaticSecrets) Secret(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", errors.New("no such secret")
}

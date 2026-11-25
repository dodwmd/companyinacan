package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SSO is company.yaml `sso:` (epic E11b, specs/021-oidc-sso): OpenID Connect single sign-on for
// the admin UI and API. The client secret is never in company.yaml, only the name of a Secret.
type SSO struct {
	Enabled bool `yaml:"enabled"`
	// Issuer is the OIDC issuer URL; discovery is fetched from <issuer>/.well-known/openid-configuration.
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecretRef is the Secret holding the client secret: "name" (key client-secret) or "name/key".
	ClientSecretRef string   `yaml:"client_secret_ref"`
	Scopes          []string `yaml:"scopes"`
	// RedirectBaseURL is the origin of the admin UI; default https://admin.<domain>.
	RedirectBaseURL string    `yaml:"redirect_base_url"`
	Claims          SSOClaims `yaml:"claims"`
	// AllowedEmailDomains limits who may sign in by the domain of the email claim (empty = any).
	AllowedEmailDomains []string `yaml:"allowed_email_domains"`
	// RequireEmailVerified defaults to true: a token whose email_verified is not true is refused.
	RequireEmailVerified *bool `yaml:"require_email_verified"`
	// RequireAMR lists acceptable `amr` values (any one must be present), e.g. [mfa]. Empty = not checked.
	RequireAMR []string `yaml:"require_amr"`
	// GroupRoles maps an IdP group to roles: built-in operator/auditor/owner or module-namespaced
	// "module:role". Users in no mapped group get no role.
	GroupRoles map[string][]string `yaml:"group_roles"`
	// ConfirmOwnerMapping must be true when any group maps to owner.
	ConfirmOwnerMapping bool `yaml:"confirm_owner_mapping"`
	// RequireMappedRole refuses sign-in for users whose groups map to no role.
	RequireMappedRole bool `yaml:"require_mapped_role"`
	// LocalLogin: all (default), owner_only, or disabled (needs ConfirmDisableLocalLogin).
	LocalLogin               string `yaml:"local_login"`
	ConfirmDisableLocalLogin bool   `yaml:"confirm_disable_local_login"`
	// RevalidateInterval is how long an SSO session lives before sign-in is repeated at the IdP
	// (default 1h, 5m to 24h). It bounds how long a revoked group lives on in an open session.
	RevalidateInterval string `yaml:"revalidate_interval"`
	// RPLogout also ends the IdP session at logout (needs end_session_endpoint in discovery).
	RPLogout bool `yaml:"rp_logout"`
	// AllowInsecureDev permits http issuers and private addresses. Development only.
	AllowInsecureDev bool `yaml:"allow_insecure_dev"`
}

// SSOClaims names the ID token claims used.
type SSOClaims struct {
	Subject string `yaml:"subject"`
	Email   string `yaml:"email"`
	Groups  string `yaml:"groups"`
	Name    string `yaml:"name"`
}

// SSO defaults and limits.
const (
	DefaultRevalidate = time.Hour
	MinRevalidate     = 5 * time.Minute
	MaxRevalidate     = 24 * time.Hour
	MaxGroupMappings  = 200
)

// Local login modes.
const (
	LocalLoginAll       = "all"
	LocalLoginOwnerOnly = "owner_only"
	LocalLoginDisabled  = "disabled"
)

var (
	ssoRoleRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}(:[a-z][a-z0-9-]{0,39})?$`)
	claimRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,63}$`)
	scopeTokRe  = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,64}$`)
	domainRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	secretRefRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?(/[A-Za-z0-9._-]{1,253})?$`)
)

// ValidSecretRef accepts the name of a Secret (a DNS subdomain) with an optional key, and refuses the
// Secrets can-core owns (`can-*`, `kubernetes.io*`), consecutive dots, and `.`/`..` keys.
func ValidSecretRef(v string) bool {
	if !secretRefRe.MatchString(v) || len(v) > 253+1+253 {
		return false
	}
	name, key, _ := strings.Cut(v, "/")
	if len(name) > 253 || strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	if strings.HasPrefix(name, "can-") || name == "can" || strings.HasPrefix(name, "kubernetes.io") {
		return false
	}
	return key != "." && key != ".." && !strings.Contains(key, "..")
}

// SSOIssue is one validation finding with the path of the offending field.
type SSOIssue struct{ Path, Rule, Message string }

// Normalised returns a copy with defaults applied; call after validation.
func (s SSO) Normalised(domain string) SSO {
	if len(s.Scopes) == 0 {
		s.Scopes = []string{"openid", "email", "profile"}
	}
	have := false
	for _, x := range s.Scopes {
		have = have || x == "openid"
	}
	if !have {
		s.Scopes = append([]string{"openid"}, s.Scopes...)
	}
	if s.RedirectBaseURL == "" && domain != "" {
		s.RedirectBaseURL = "https://admin." + domain
	}
	s.RedirectBaseURL = strings.TrimRight(s.RedirectBaseURL, "/")
	if s.Claims.Subject == "" {
		s.Claims.Subject = "sub"
	}
	if s.Claims.Email == "" {
		s.Claims.Email = "email"
	}
	if s.Claims.Groups == "" {
		s.Claims.Groups = "groups"
	}
	if s.Claims.Name == "" {
		s.Claims.Name = "name"
	}
	if s.LocalLogin == "" {
		s.LocalLogin = LocalLoginAll
	}
	if s.RequireEmailVerified == nil {
		t := true
		s.RequireEmailVerified = &t
	}
	return s
}

// RevalidateEvery parses RevalidateInterval (default when empty or invalid).
func (s SSO) RevalidateEvery() time.Duration {
	if s.RevalidateInterval == "" {
		return DefaultRevalidate
	}
	d, err := time.ParseDuration(s.RevalidateInterval)
	if err != nil {
		return DefaultRevalidate
	}
	return d
}

// EmailVerifiedRequired reports the effective require_email_verified (default true).
func (s SSO) EmailVerifiedRequired() bool {
	return s.RequireEmailVerified == nil || *s.RequireEmailVerified
}

// ValidateSSO checks the semantic rules the JSON Schema cannot express. domain is company.domain.
func ValidateSSO(s *SSO, domain string) []SSOIssue {
	if s == nil || !s.Enabled {
		return nil
	}
	var out []SSOIssue
	add := func(path, rule, msg string, a ...any) {
		out = append(out, SSOIssue{Path: "/sso" + path, Rule: "semantic.sso-" + rule, Message: fmt.Sprintf(msg, a...)})
	}
	if err := CheckHTTPSURL(s.Issuer, s.AllowInsecureDev, false); err != nil {
		add("/issuer", "issuer", "issuer %v", err)
	} else if u, err := url.Parse(s.Issuer); err == nil && (u.RawQuery != "" || u.Fragment != "") {
		add("/issuer", "issuer", "issuer must not have a query or fragment")
	}
	if s.ClientID == "" || len(s.ClientID) > 256 || strings.ContainsAny(s.ClientID, " \t\r\n") {
		add("/client_id", "client-id", "client_id is required (1 to 256 characters, no spaces)")
	}
	if !ValidSecretRef(s.ClientSecretRef) {
		add("/client_secret_ref", "secret-ref", "client_secret_ref must be the name of a Secret (name or name/key; names starting can- or kubernetes.io are refused); the secret itself never goes in company.yaml")
	}
	if len(s.Scopes) > 20 {
		add("/scopes", "scopes", "at most 20 scopes")
	}
	for _, sc := range s.Scopes {
		if !scopeTokRe.MatchString(sc) {
			add("/scopes", "scopes", "invalid scope %q", sc)
		}
	}
	if base := s.RedirectBaseURL; base == "" {
		if domain == "" {
			add("/redirect_base_url", "redirect", "set redirect_base_url or the company domain")
		}
	} else if err := CheckHTTPSURL(base, s.AllowInsecureDev, true); err != nil {
		add("/redirect_base_url", "redirect", "redirect_base_url %v", err)
	}
	for _, c := range []struct{ f, v string }{{"subject", s.Claims.Subject}, {"email", s.Claims.Email}, {"groups", s.Claims.Groups}, {"name", s.Claims.Name}} {
		if c.v != "" && !claimRe.MatchString(c.v) {
			add("/claims/"+c.f, "claim", "invalid claim name %q", c.v)
		}
	}
	for _, d := range s.AllowedEmailDomains {
		if !domainRe.MatchString(d) {
			add("/allowed_email_domains", "email-domain", "%q is not a lowercase domain name", d)
		}
	}
	if len(s.GroupRoles) > MaxGroupMappings {
		add("/group_roles", "mapping", "at most %d group mappings", MaxGroupMappings)
	}
	groups := make([]string, 0, len(s.GroupRoles))
	for g := range s.GroupRoles {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	owner := false
	for _, g := range groups {
		if g == "" || len(g) > 256 || strings.ContainsAny(g, "\x00\r\n") {
			add("/group_roles", "mapping", "invalid group name")
			continue
		}
		if len(s.GroupRoles[g]) > 20 {
			add("/group_roles/"+g, "mapping", "at most 20 roles per group")
		}
		for _, r := range s.GroupRoles[g] {
			switch {
			case !ssoRoleRe.MatchString(r):
				add("/group_roles/"+g, "role", "invalid role %q", r)
			case !strings.Contains(r, ":") && !IsBuiltinRole(r):
				add("/group_roles/"+g, "role", "role %q is neither built in (owner, operator, auditor) nor module-namespaced (module:role)", r)
			case r == "owner":
				owner = true
			}
		}
	}
	if owner && !s.ConfirmOwnerMapping {
		add("/confirm_owner_mapping", "owner-confirm", "a group maps to owner: set confirm_owner_mapping: true to confirm that everyone in that IdP group gets full control")
	}
	switch s.LocalLogin {
	case "", LocalLoginAll, LocalLoginOwnerOnly:
	case LocalLoginDisabled:
		if !s.ConfirmDisableLocalLogin {
			add("/confirm_disable_local_login", "local-login", "local_login: disabled needs confirm_disable_local_login: true (the break-glass password login stops working; `can-core admin` in the pod remains)")
		}
		if !owner {
			add("/local_login", "local-login", "local_login: disabled needs at least one group mapped to owner, otherwise nobody could administer")
		}
	default:
		add("/local_login", "local-login", "local_login must be all, owner_only or disabled")
	}
	if s.RevalidateInterval != "" {
		d, err := time.ParseDuration(s.RevalidateInterval)
		if err != nil || d < MinRevalidate || d > MaxRevalidate {
			add("/revalidate_interval", "revalidate", "revalidate_interval must be a duration between %s and %s", MinRevalidate, MaxRevalidate)
		}
	}
	return out
}

// CheckHTTPSURL requires an absolute https URL without userinfo (http allowed with insecure), and,
// unless insecure, refuses "localhost"-like names and IP literals in private, loopback or link-local
// ranges. originOnly additionally refuses a path.
func CheckHTTPSURL(raw string, insecure, originOnly bool) error {
	if raw == "" {
		return fmt.Errorf("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("is not an absolute URL")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain credentials")
	}
	if u.Scheme != "https" && (!insecure || u.Scheme != "http") {
		return fmt.Errorf("must be https")
	}
	if originOnly && (strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "") {
		return fmt.Errorf("must be an origin (scheme and host) without path")
	}
	if !insecure {
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".local") {
			return fmt.Errorf("host %q is not allowed (private name); set allow_insecure_dev only for development", h)
		}
		if ip := net.ParseIP(h); ip != nil && !PublicIP(ip) {
			return fmt.Errorf("address %s is not a public address; set allow_insecure_dev only for development", h)
		}
	}
	return nil
}

var denyNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// PublicIP reports whether ip is a routable public address (not private, loopback, link-local,
// carrier-grade NAT, documentation, multicast, unspecified, or an IPv4-mapped form of those).
func PublicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range denyNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

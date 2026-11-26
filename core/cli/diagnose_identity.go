package cli

// A support bundle should not say whose it is. The scrubbers of core/diagnose remove secrets and
// email addresses; this one removes, by value, what identifies the installation: the company, its
// domain, the server, the SSH user and key paths, the identity provider tenant and the mail relay.

import (
	"encoding/json"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/provider"
)

type identity struct {
	values map[string]string // value -> placeholder
	words  map[string]bool   // replaced only where not part of a longer word
}

func (id *identity) add(v, placeholder string) {
	v = strings.TrimSpace(v)
	if len(v) < 3 || v == "root" || strings.Contains(v, "[redacted") {
		return
	}
	forms := []string{v}
	if j, err := json.Marshal(v); err == nil { // as it appears inside the bundle's JSON files
		forms = append(forms, strings.Trim(string(j), `"`))
	}
	for _, f := range forms {
		if _, ok := id.values[f]; !ok {
			id.values[f] = placeholder
		}
	}
}

// word is add for values that are ordinary words (a user or company name).
func (id *identity) word(v, placeholder string) {
	id.add(v, placeholder)
	id.words[strings.TrimSpace(v)] = true
}

var reSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// identityFrom collects the identifying values of the company in dir. ok is false when there is a
// company.yaml that cannot be read as YAML: then nothing can be said about what is in it.
func identityFrom(dir, companyYAML string) (*identity, bool) {
	id := &identity{values: map[string]string{}, words: map[string]bool{}}
	if home, err := os.UserHomeDir(); err == nil {
		id.add(home, "<home>")
	}
	if st, err := provider.NewStore(dir).Load(); err == nil && st.Server != nil {
		id.server(st.Server.IP)
		id.word(st.Server.User, "<ssh-user>")
		id.word(st.Server.Name, "<server-name>")
	}
	if companyYAML == "" {
		return id, true
	}
	var co config.Company
	if err := yaml.Unmarshal([]byte(companyYAML), &co); err != nil {
		return id, false
	}
	id.word(co.Name, "<company>")
	id.word(strings.Trim(reSlugRun.ReplaceAllString(strings.ToLower(co.Name), "-"), "-"), "<company>")
	id.add(co.Domain, "<domain>")
	for key, v := range co.Provider.Options {
		s, _ := v.(string)
		switch key {
		case "host":
			id.server(s)
		case "user":
			id.word(s, "<ssh-user>")
		case "operator_ip":
			id.add(s, "<operator-address>")
		case "server_name", "node_name":
			id.word(s, "<server-name>")
		case "key_file", "ssh_private_key_file", "ssh_public_key_file", "token_secret_ref":
			s = strings.TrimPrefix(s, "file:")
			if !strings.HasPrefix(s, "env:") {
				id.add(s, "<path>")
			}
		case "ssh_key_fingerprints":
			list, _ := v.([]any)
			for _, f := range list {
				fp, _ := f.(string)
				id.add(fp, "<key-fingerprint>")
			}
		}
	}
	if s := co.SSO; s != nil {
		id.url(s.Issuer, "<sso-issuer>")
		id.url(s.RedirectBaseURL, "<url>")
		id.add(s.ClientID, "<sso-client-id>")
		for _, d := range s.AllowedEmailDomains {
			id.add(d, "<email-domain>")
		}
		for group := range s.GroupRoles {
			id.word(group, "<sso-group>")
		}
	}
	if co.Email != nil && co.Email.Relay != nil {
		id.add(co.Email.Relay.Host, "<relay-host>")
		id.add(co.Email.Relay.From, "<email>")
	}
	if zone, _ := co.DNS["zone"].(string); zone != "" {
		id.add(zone, "<domain>")
	}
	return id, true
}

func (id *identity) server(addr string) {
	id.add(addr, "<server>")
	if _, err := netip.ParseAddr(addr); err == nil {
		id.add(strings.NewReplacer(".", "-", ":", "-").Replace(addr), "<server-dashed>")
	}
}

// url removes a URL, its host and the parts of its path that can name a tenant or realm.
func (id *identity) url(raw, placeholder string) {
	id.add(raw, placeholder)
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return
	}
	host, path, _ := strings.Cut(rest, "/")
	id.add(host, placeholder+"-host")
	for _, p := range strings.Split(path, "/") {
		switch p {
		case "realms", "auth", "oauth2", "default", "v2.0", "application":
		default:
			id.word(p, "<sso-tenant>")
		}
	}
}

var reIPv4 = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// redact replaces the identifying values, longest first, whatever their case, and then every public
// IPv4 address (private and cluster addresses stay: they are what a network problem is about).
func (id *identity) redact(text string) string {
	values := make([]string, 0, len(id.values))
	for v := range id.values {
		values = append(values, v)
	}
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	for _, v := range values {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(v))
		var out strings.Builder
		last := 0
		for _, m := range re.FindAllStringIndex(text, -1) {
			if id.words[v] && (m[0] > 0 && wordByte(text[m[0]-1]) || m[1] < len(text) && wordByte(text[m[1]])) {
				continue
			}
			out.WriteString(text[last:m[0]])
			out.WriteString(id.values[v])
			last = m[1]
		}
		out.WriteString(text[last:])
		text = out.String()
	}
	return reIPv4.ReplaceAllStringFunc(text, func(m string) string {
		a, err := netip.ParseAddr(m)
		if err != nil || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
			return m
		}
		return "<address>"
	})
}

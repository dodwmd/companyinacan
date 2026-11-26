package cli

// A support bundle should not say whose it is. The scrubbers of core/diagnose remove secrets and
// email addresses; this one removes, by value, what identifies the installation: the company, its
// domain, the server, the SSH user and key paths, the identity provider tenant and the mail relay.

import (
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"golang.org/x/text/unicode/norm"

	"github.com/dodwmd/companyinacan/core/provider"
)

type identity struct {
	values map[string]string // value -> placeholder
	words  map[string]bool   // replaced only where not part of a longer word
	names  []namePattern     // spellings of the company name
}

type namePattern struct {
	re      *regexp.Regexp
	bounded bool // only where not part of a longer word
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

// identityFrom collects the identifying values of the company in dir. ok is false when there is a
// company.yaml that cannot be read as YAML: then it is not put in the bundle, and what could still
// be picked out of its lines is removed from everything else. A file that is YAML but has a value
// of the wrong type is read all the same: such a file is when a bundle is wanted.
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
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(companyYAML), &doc); err != nil {
		for _, m := range reYAMLLine.FindAllStringSubmatch(companyYAML, -1) {
			id.setting(m[1], strings.Trim(strings.TrimSpace(m[2]), `"'`))
		}
		return id, false
	}
	id.walk(&doc, "", 0)
	return id, true
}

// reYAMLLine picks `key: value` out of a file that does not parse, also from a one-line mapping.
var reYAMLLine = regexp.MustCompile(`(?m)(?:^|[{,])[ \t-]*([a-z_]+)[ \t]*:[ \t]*("[^"\n]*"|'[^'\n]*'|[^,{}\[\]#\n]+)`)

// walk visits every value of company.yaml with the key it stands under, whatever the shape of the
// document, so that a value of the wrong type hides nothing.
func (id *identity) walk(n *yaml.Node, key string, depth int) {
	if depth > 20 {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			id.walk(c, key, depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if key == "group_roles" {
				id.word(k, "<sso-group>")
				continue
			}
			// Only the company's own name: module and provider names further down are not it.
			if k == "name" && depth > 1 {
				k = "other_name"
			}
			id.walk(n.Content[i+1], k, depth+1)
		}
	case yaml.ScalarNode:
		id.setting(key, n.Value)
	}
}

// setting records the value of one company.yaml key. Values of keys it does not know are removed
// when they look like an address, a host or a path where one of the identifying sections is not
// the mapping it should be.
func (id *identity) setting(key, v string) {
	switch key {
	case "name":
		id.name(v)
	case "domain", "zone":
		id.add(v, "<domain>")
	case "host":
		// the SSH server, or the mail relay
		if _, err := netip.ParseAddr(v); err == nil {
			id.server(v)
		} else {
			id.add(v, "<host>")
		}
	case "user":
		id.word(v, "<ssh-user>")
	case "operator_ip":
		id.add(v, "<operator-address>")
	case "server_name", "node_name":
		id.word(v, "<server-name>")
	case "key_file", "ssh_private_key_file", "ssh_public_key_file", "token_secret_ref":
		v = strings.TrimPrefix(v, "file:")
		if !strings.HasPrefix(v, "env:") {
			id.add(v, "<path>")
		}
	case "ssh_key_fingerprints":
		id.add(v, "<key-fingerprint>")
	case "issuer":
		id.url(v, "<sso-issuer>")
	case "redirect_base_url":
		id.url(v, "<url>")
	case "client_id":
		id.add(v, "<sso-client-id>")
	case "allowed_email_domains":
		id.add(v, "<email-domain>")
	case "from":
		id.add(v, "<email>")
	case "provider", "options", "sso", "email", "relay", "dns":
		if strings.ContainsAny(v, ".@/:") {
			id.add(v, "<config-value>")
		}
	}
}

var (
	reNameSplit = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	legalForms  = map[string]bool{"pty": true, "ltd": true, "limited": true, "inc": true, "llc": true, "gmbh": true, "corp": true, "plc": true, "co": true, "company": true}
	// germanic spells umlauts the way host and user names do.
	germanic = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "ø", "o", "æ", "ae", "å", "a")
)

func unaccented(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, norm.NFD.String(s))
}

// name records the company name in the ways it turns up in logs, host names and identifiers: any
// case, with spaces, dashes, underscores or dots between its words or none, with and without the
// legal form, and without accents.
func (id *identity) name(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 3 || strings.Contains(v, "[redacted") {
		return
	}
	lower := strings.ToLower(norm.NFC.String(v))
	for _, spelling := range []string{lower, unaccented(lower), unaccented(germanic.Replace(lower))} {
		words := slices.DeleteFunc(reNameSplit.Split(spelling, -1), func(w string) bool { return w == "" })
		short := slices.Clone(words)
		for len(short) > 1 && legalForms[short[len(short)-1]] {
			short = short[:len(short)-1]
		}
		for _, w := range [][]string{words, short} {
			joined := strings.Join(w, "")
			n := utf8.RuneCountInString(joined)
			if n < 3 || (len(w) < len(words) && (n < 4 || joined == "the")) {
				continue
			}
			quoted := make([]string, len(w))
			for i := range w {
				quoted[i] = regexp.QuoteMeta(w[i])
			}
			expr := `(?i)` + strings.Join(quoted, `(?:%20|[\s_.+-])*`)
			if !slices.ContainsFunc(id.names, func(p namePattern) bool { return p.re.String() == expr }) {
				// A long name of several words is distinctive enough to be removed inside a longer word.
				id.names = append(id.names, namePattern{regexp.MustCompile(expr), len(w) < 2 || n < 8})
			}
		}
	}
}

func (id *identity) server(addr string) {
	id.add(addr, "<server>")
	if a, err := netip.ParseAddr(addr); err == nil {
		for _, f := range []string{addr, a.String(), a.StringExpanded()} {
			id.add(f, "<server>")
			id.add(strings.NewReplacer(".", "-", ":", "-").Replace(f), "<server-dashed>")
		}
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

// redact replaces the identifying values, longest first, whatever their case, then the company
// name, and then every public IPv4 and IPv6 address, also as written in sslip.io and nip.io names
// (private and cluster addresses stay: they are what a network problem is about).
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
	// Longest first, so that the name with its legal form goes before the name without it.
	slices.SortStableFunc(id.names, func(a, b namePattern) int { return len(b.re.String()) - len(a.re.String()) })
	for _, p := range id.names {
		text = replaceMatches(text, p.re, p.bounded, "<company>")
	}
	text = redactSslip(text)
	text = redactIPv6(text)
	return reIPv4.ReplaceAllStringFunc(text, func(m string) string {
		a, err := netip.ParseAddr(m)
		if err != nil || !public(a) {
			return m
		}
		return "<address>"
	})
}

func public(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	return !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsUnspecified() && !a.IsMulticast()
}

// replaceMatches replaces the matches of re; bounded leaves those inside a longer word.
func replaceMatches(text string, re *regexp.Regexp, bounded bool, with string) string {
	var out strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(text, -1) {
		if bounded {
			before, _ := utf8.DecodeLastRuneInString(text[:m[0]])
			after, _ := utf8.DecodeRuneInString(text[m[1]:])
			if m[0] > 0 && alnum(before) || m[1] < len(text) && alnum(after) {
				continue
			}
		}
		out.WriteString(text[last:m[0]])
		out.WriteString(with)
		last = m[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

func alnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

var (
	// an address written into a host name: 203-0-113-7, 2001-db8--1 or cb007107 before .sslip.io
	reSslip = regexp.MustCompile(`(?i)((?:[0-9]{1,3}-){3}[0-9]{1,3}|[0-9a-f]{0,4}(?:-[0-9a-f]{0,4}){2,7}|[0-9a-f]{8})(\.(?:sslip|nip)\.io)`)
	// a run that may hold an IPv6 address
	reIPv6Run = regexp.MustCompile(`[0-9A-Fa-f]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f.]*(?:%[0-9A-Za-z_-]+)?`)
)

func redactSslip(text string) string {
	return reSslip.ReplaceAllStringFunc(text, func(m string) string {
		parts := reSslip.FindStringSubmatch(m)
		label, suffix := parts[1], parts[2]
		if len(label) == 8 && !strings.Contains(label, "-") {
			if b, err := hex.DecodeString(label); err == nil && public(netip.AddrFrom4([4]byte(b))) {
				return "<address-dashed>" + suffix
			}
			return m
		}
		// The match may begin with the end of an ordinary label ("app-" before the address).
		for start := 0; ; {
			rest := label[start:]
			for _, sep := range []string{".", ":"} {
				if a, err := netip.ParseAddr(strings.ReplaceAll(rest, "-", sep)); err == nil {
					if !public(a) {
						return m
					}
					return label[:start] + "<address-dashed>" + suffix
				}
			}
			i := strings.IndexByte(rest, '-')
			if i < 0 {
				return m
			}
			start += i + 1
		}
	})
}

func redactIPv6(text string) string {
	var out strings.Builder
	last := 0
	for _, m := range reIPv6Run.FindAllStringIndex(text, -1) {
		from, to := m[0], m[1]
		// The run may begin with the end of a label: "ip:" starts it at the colon, "node:" at "de:".
		if from > 0 && wordByte(text[from-1]) {
			from += strings.IndexByte(text[from:to], ':')
			if strings.HasPrefix(text[from:to], "::") {
				continue // "std::string"
			}
			from++
		}
		if f, t, ok := findIPv6(text, from, to); ok {
			out.WriteString(text[last:f])
			out.WriteString("<address>")
			last = t
		}
	}
	out.WriteString(text[last:])
	return out.String()
}

// findIPv6 looks for a public IPv6 address in text[from:to], longest first: the run can carry a
// label made of hex digits ("face:"), a port written without brackets, or punctuation.
func findIPv6(text string, from, to int) (int, int, bool) {
	starts := []int{from}
	for f := from; len(starts) < 3; {
		i := strings.IndexByte(text[f:to], ':')
		if i < 0 || strings.HasPrefix(text[f+i:to], "::") {
			break
		}
		f += i + 1
		starts = append(starts, f)
	}
	ends := []int{to, to - 1, to - 2}
	if i := strings.LastIndexByte(text[from:to], ':'); i > 0 {
		ends = append(ends, from+i)
	}
	for _, f := range starts {
		for _, t := range ends {
			if f >= t || t < len(text) && wordByte(text[t]) {
				continue
			}
			if a, err := netip.ParseAddr(text[f:t]); err == nil && a.Is6() {
				return f, t, public(a)
			}
		}
	}
	return 0, 0, false
}

package verify

// Probes that need their own flag because they do more than read the server: one failed sign-in
// (--probe-client-ip), and connections from the server to the backup repository
// (--probe-backup-repo), the mail relay (--probe-smtp) and the identity provider (--probe-sso).
// None sends mail, signs in, or changes data other than the one audit entry of the first.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/provider"
)

var podNets = []netip.Prefix{netip.MustParsePrefix(provider.DefaultPodCIDR), netip.MustParsePrefix(provider.DefaultServiceCIDR)}

// CheckClientIP makes one sign-in attempt for a user that does not exist, then reads back from the
// audit log which source address can-core recorded for it. The address itself is never reported.
func CheckClientIP(ctx context.Context, n Net, r provider.Runner, addr, domain string, routes []Route) Check {
	const id = "edge.client-address"
	admin, ok := adminRoute(routes, domain)
	if !ok {
		return skip(id, "the admin UI is not routed")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return skip(id, "no random probe name")
	}
	user := "can-verify-" + hex.EncodeToString(b[:])
	c := n.client(addr)
	defer c.CloseIdleConnections()
	body := `{"username":"` + user + `","password":"not-a-password","totp":"000000"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scheme(admin)+"://"+admin.Host+"/api/v1/login", strings.NewReader(body))
	if err != nil {
		return skip(id, "bad admin address")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return skip(id, "the sign-in request got no answer")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return skip(id, fmt.Sprintf("the sign-in request answered HTTP %d, not 401: wait a minute and try again", resp.StatusCode))
	}
	out, err := r.Run(ctx, "k3s kubectl -n can-system exec deploy/can-core -- /can-core audit export 2>/dev/null | grep -F '"+user+"' | tail -n 1\n")
	if err != nil {
		return skip(id, "the audit log could not be read")
	}
	var rec struct{ Detail string }
	var detail struct{ IP string }
	if json.Unmarshal(jsonPart(out), &rec) != nil || json.Unmarshal([]byte(rec.Detail), &detail) != nil {
		return skip(id, "the audit log has no entry for the probe")
	}
	got, err := netip.ParseAddr(detail.IP)
	if err != nil {
		return fail(id, "can-core recorded no usable source address")
	}
	got = got.Unmap()
	server, serr := netip.ParseAddr(addr)
	privateServer := serr == nil && (server.IsPrivate() || server.IsLoopback())
	switch {
	case slices.ContainsFunc(podNets, func(p netip.Prefix) bool { return p.Contains(got) }) || got.IsLoopback():
		return fail(id, "can-core saw a cluster-internal address: every client looks the same, so lockouts and rate limits are shared")
	case !privateServer && got.IsPrivate():
		return fail(id, "can-core saw a private address for a request made to a public server (if you reach it over a VPN, this is expected)")
	}
	return pass(id, "can-core recorded a client address outside the cluster (compare it with yours in the audit log)")
}

// jsonPart drops what ssh or sudo printed before a JSON object (the runner merges the streams).
func jsonPart(out string) []byte {
	if i := strings.Index(out, "{"); i >= 0 {
		return []byte(strings.TrimSpace(out[i:]))
	}
	return nil
}

// CheckBackupRepo lists the snapshots of the real repository from the server, without taking a
// lock or writing a cache. Only the count and age are kept: restic's output names the bucket.
func CheckBackupRepo(ctx context.Context, r provider.Runner, now time.Time) Check {
	const id = "backup.repository"
	out, err := r.Run(ctx, backupRepoScript)
	if strings.Contains(out, "CAN_NOT_INSTALLED") {
		return skip(id, "no backup is installed on the server")
	}
	var snaps []backup.Snapshot
	i := strings.Index(out, "[")
	if err != nil || i < 0 || json.Unmarshal([]byte(strings.TrimSpace(out[i:])), &snaps) != nil {
		return fail(id, "restic could not list the repository from the server (journalctl -u can-backup)")
	}
	good := backup.UsableSnapshots(snaps)
	var newest time.Time
	for _, s := range good {
		if s.Time.After(newest) {
			newest = s.Time
		}
	}
	if len(good) == 0 {
		return fail(id, "the repository holds no usable snapshot")
	}
	age := now.Sub(newest)
	d := fmt.Sprintf("%d usable snapshots, newest %d h old (this does not prove a restore: see the runbook)", len(good), int(age.Hours()))
	return verdict(id, age <= maxBackupAge, d, d)
}

const backupRepoScript = `#!/usr/bin/env bash
# can verify: list backup snapshots (read-only)
set -u
if [ ! -f /usr/local/sbin/can-backup ] || [ ! -r /etc/can/backup.d/restic.env ]; then echo CAN_NOT_INSTALLED; exit 0; fi
set -a
. /etc/can/backup.d/restic.env
set +a
eval "$(grep -m1 '^export RESTIC_REPOSITORY=' /usr/local/sbin/can-backup)"
export RESTIC_PASSWORD_FILE=/etc/can/backup.d/restic.password
timeout 90 restic snapshots --json --no-lock --no-cache --tag can 2>/dev/null
`

var (
	reRelayHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	reIssuer    = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$`)
)

// CheckSMTP opens a TLS session with the relay from the server and closes it. It does not
// authenticate and sends no mail.
func CheckSMTP(ctx context.Context, r provider.Runner, host string, port int, implicitTLS bool) Check {
	const id = "email.relay-tls"
	if host == "" {
		return skip(id, "no email.relay.host in company.yaml")
	}
	if !reRelayHost.MatchString(host) || port < 1 || port > 65535 {
		return fail(id, "email.relay host or port is not valid")
	}
	starttls := "-starttls smtp "
	if implicitTLS {
		starttls = ""
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
# can verify: TLS handshake with the mail relay, no mail
if ! command -v openssl >/dev/null 2>&1; then echo CAN_NO_OPENSSL; exit 0; fi
if echo QUIT | timeout 20 openssl s_client -connect '%[1]s:%[2]d' -servername '%[1]s' -verify_hostname '%[1]s' -verify_return_error %[3]s-quiet >/dev/null 2>&1; then echo CAN_TLS_OK; else echo CAN_TLS_FAILED; fi
`, host, port, starttls)
	out, err := r.Run(ctx, script)
	switch {
	case strings.Contains(out, "CAN_NO_OPENSSL"):
		return skip(id, "openssl is not installed on the server")
	case err != nil || !strings.Contains(out, "CAN_TLS_OK"):
		return fail(id, fmt.Sprintf("no verified TLS session with the relay on port %d from the server (many providers block outbound mail ports)", port))
	}
	return pass(id, fmt.Sprintf("verified TLS session with the relay on port %d from the server; credentials and delivery not tested", port))
}

// CheckSSO reads the issuer's discovery document from the server and holds it to what can-core
// needs (docs/sso.md). No sign-in happens.
func CheckSSO(ctx context.Context, r provider.Runner, issuer string) Check {
	const id = "sso.discovery"
	if issuer == "" {
		return skip(id, "sso is not enabled")
	}
	if !reIssuer.MatchString(issuer) {
		return fail(id, "sso.issuer is not a plain https URL")
	}
	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	out, err := r.Run(ctx, "# can verify: OpenID Connect discovery\ncurl -fsS --max-time 20 --max-filesize 262144 '"+u+"'\n")
	if err != nil {
		return fail(id, "the discovery document could not be fetched from the server")
	}
	var d struct {
		Issuer string   `json:"issuer"`
		Auth   string   `json:"authorization_endpoint"`
		Token  string   `json:"token_endpoint"`
		JWKS   string   `json:"jwks_uri"`
		Algs   []string `json:"id_token_signing_alg_values_supported"`
	}
	if json.Unmarshal(jsonPart(out), &d) != nil {
		return fail(id, "the discovery document is not JSON")
	}
	var bad []string
	if d.Issuer != issuer {
		bad = append(bad, "its issuer differs from sso.issuer (check a trailing slash)")
	}
	for name, v := range map[string]string{"authorization_endpoint": d.Auth, "token_endpoint": d.Token, "jwks_uri": d.JWKS} {
		if p, err := url.Parse(v); err != nil || p.Scheme != "https" || p.Host == "" {
			bad = append(bad, name+" is not https")
		}
	}
	if !slices.Contains(d.Algs, "RS256") && !slices.Contains(d.Algs, "ES256") {
		bad = append(bad, "neither RS256 nor ES256 is offered")
	}
	slices.Sort(bad)
	return verdict(id, len(bad) == 0, "discovery is consistent; sign-in itself is a manual check", strings.Join(bad, "; "))
}

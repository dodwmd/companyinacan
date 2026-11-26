package verify

// Probes from the operator's machine (`--probe-public`, `--probe-rate-limit`): they send traffic to
// the company's own server, and only there: every connection is made to the recorded server address,
// whatever DNS says, so that DNS, certificates and routing are judged separately.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Net is how the probes reach the network; tests replace it.
type Net struct {
	Dial  func(ctx context.Context, network, address string) (net.Conn, error)
	Roots *x509.CertPool // nil = the system's
	Now   func() time.Time
}

// DefaultNet is the real network.
func DefaultNet() Net {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return Net{Dial: d.DialContext, Now: time.Now}
}

func (n Net) client(addr string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, hostport string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(hostport)
				if err != nil {
					return nil, err
				}
				return n.Dial(ctx, network, net.JoinHostPort(addr, port))
			},
			TLSClientConfig: &tls.Config{RootCAs: n.Roots, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// DNSName is one record the company needs and where it points ("ok", "missing", "elsewhere").
type DNSName struct{ Name, State string }

// CheckDNS judges the lookups. Names of the company's own domain count only once something under
// the domain is routed: before that the server is served on its instant address.
func CheckDNS(names []DNSName, domain string, proxied bool, routes []Route) Check {
	const id = "public.dns"
	switch {
	case len(names) == 0:
		return skip(id, "no records to check (no domain, or the server has no IP address)")
	case proxied:
		return skip(id, "dns.proxied is set: the names answer with the proxy's addresses")
	case len(routes) > 0 && !slices.ContainsFunc(routes, func(r Route) bool { return underDomain(r.Host, domain) }):
		return skip(id, "nothing under your domain is routed yet (`can up --domain-live` after adding the records)")
	}
	var bad []string
	for _, n := range names {
		if n.State != "ok" {
			bad = append(bad, n.Name+" "+n.State)
		}
	}
	return verdict(id, len(bad) == 0, fmt.Sprintf("%d records point at the server", len(names)), strings.Join(bad, ", "))
}

func underDomain(host, domain string) bool {
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// CheckPorts checks that the Kubernetes API and kubelet ports do not answer.
func CheckPorts(ctx context.Context, n Net, addr string) Check {
	const id = "public.closed-ports"
	open := func(port string) bool {
		c, err := n.Dial(ctx, "tcp", net.JoinHostPort(addr, port))
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	if !open("443") && !open("80") {
		return skip(id, "neither 443 nor 80 answers from here, so closed ports would prove nothing")
	}
	var bad []string
	for _, p := range []string{"6443", "10250"} {
		if open(p) {
			bad = append(bad, p)
		}
	}
	return verdict(id, len(bad) == 0, "6443 and 10250 do not answer from here",
		"reachable from here: "+strings.Join(bad, ", ")+" (check the host and cloud firewall)")
}

const (
	minCertLife = 21 * 24 * time.Hour
	maxHosts    = 20
)

// CheckTLS checks the certificate served for each TLS host.
func CheckTLS(ctx context.Context, n Net, addr string, routes []Route) Check {
	const id = "public.certificates"
	var good, bad []string
	for _, r := range routes {
		if !r.TLS || len(good)+len(bad) >= maxHosts {
			continue
		}
		if why := n.certificate(ctx, addr, r.Host); why != "" {
			bad = append(bad, r.Host+": "+why)
		} else {
			good = append(good, r.Host)
		}
	}
	switch {
	case len(bad) > 0:
		return fail(id, strings.Join(bad, "; "))
	case len(good) == 0:
		return skip(id, "no host is routed with TLS (plain-HTTP fallback)")
	}
	return pass(id, fmt.Sprintf("%d hosts serve a trusted certificate valid for over 21 days", len(good)))
}

// certificate returns "" or what is wrong with the certificate for host.
func (n Net) certificate(ctx context.Context, addr, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := n.Dial(ctx, "tcp", net.JoinHostPort(addr, "443"))
	if err != nil {
		return "cannot connect to 443"
	}
	defer func() { _ = raw.Close() }()
	// Verification is done below, to say why it failed.
	c := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // verified below
	if err := c.HandshakeContext(ctx); err != nil {
		return "TLS handshake failed"
	}
	peers := c.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		return "no certificate"
	}
	leaf := peers[0]
	if leaf.Subject.CommonName == "TRAEFIK DEFAULT CERT" {
		return "Traefik's default certificate: none was issued (kubectl -n kube-system logs deploy/traefik)"
	}
	inter := x509.NewCertPool()
	for _, p := range peers[1:] {
		inter.AddCert(p)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: n.Roots, Intermediates: inter, CurrentTime: n.Now()}); err != nil {
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			return "not signed by a trusted authority (a staging certificate?)"
		}
		return "not valid for this name or expired"
	}
	if left := leaf.NotAfter.Sub(n.Now()); left < minCertLife {
		return fmt.Sprintf("expires in %d days: renewal is not working", int(left.Hours()/24))
	}
	return ""
}

// siteRoute is the first public-site host (not the admin UI, not a module).
func siteRoute(routes []Route, domain string) (Route, bool) {
	var found []Route
	for _, r := range routes {
		if r.Namespace == "can-system" && !strings.HasPrefix(r.Host, "admin.") {
			found = append(found, r)
		}
	}
	for _, r := range found {
		if r.Host == domain {
			return r, true
		}
	}
	if len(found) == 0 {
		return Route{}, false
	}
	return found[0], true
}

// adminRoute is the admin UI's host, preferring the company's own domain.
func adminRoute(routes []Route, domain string) (Route, bool) {
	var out Route
	ok := false
	for _, r := range routes {
		if r.Namespace == "can-system" && strings.HasPrefix(r.Host, "admin.") && (!ok || underDomain(r.Host, domain)) {
			out, ok = r, true
		}
	}
	return out, ok
}

func scheme(r Route) string {
	if r.TLS {
		return "https"
	}
	return "http"
}

// CheckHTTP checks that the site and the admin UI answer, and that a TLS site redirects plain HTTP.
func CheckHTTP(ctx context.Context, n Net, addr, domain string, routes []Route) []Check {
	c := n.client(addr)
	defer c.CloseIdleConnections()
	status := func(url string) (*http.Response, string) {
		resp, err := get(ctx, c, url)
		if err != nil {
			return nil, "no answer"
		}
		return resp, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	var cs []Check
	site, ok := siteRoute(routes, domain)
	if !ok {
		cs = append(cs, skip("public.site", "no site host is routed (plain-HTTP fallback on the address)"), skip("public.https-redirect", "no site host is routed"))
	} else {
		resp, d := status(scheme(site) + "://" + site.Host + "/")
		cs = append(cs, verdict("public.site", resp != nil && resp.StatusCode == http.StatusOK, "the site answers 200 over "+scheme(site), "the site: "+d))
		if !site.TLS {
			cs = append(cs, skip("public.https-redirect", "the site is served over plain HTTP"))
		} else {
			resp, d := status("http://" + site.Host + "/")
			to := ""
			if resp != nil {
				to = resp.Header.Get("Location")
			}
			cs = append(cs, verdict("public.https-redirect", resp != nil && resp.StatusCode/100 == 3 && strings.HasPrefix(to, "https://"),
				"plain HTTP redirects to HTTPS", "plain HTTP did not redirect to HTTPS: "+d))
		}
	}
	admin, ok := adminRoute(routes, domain)
	if !ok {
		return append(cs, skip("public.admin", "the admin UI is not routed (plain-HTTP fallback: use kubectl port-forward)"))
	}
	resp, d := status(scheme(admin) + "://" + admin.Host + "/healthz")
	return append(cs, verdict("public.admin", resp != nil && resp.StatusCode == http.StatusOK, "the admin UI answers over "+scheme(admin), "the admin UI: "+d))
}

// rateAttempts bounds the burst: the sign-in limit is a burst of 30 refilled at one a second.
const (
	rateAttempts = 90
	rateBurst    = 30
)

// CheckRateLimit sends GET requests for the sign-in page until the edge refuses one. No sign-in is
// attempted, so no account or address can be locked. The caller's address stays limited for about
// half a minute afterwards.
func CheckRateLimit(ctx context.Context, n Net, addr, domain string, routes []Route, limitInstalled bool) Check {
	const id = "edge.rate-limit"
	admin, ok := adminRoute(routes, domain)
	switch {
	case !ok:
		return skip(id, "the admin UI is not routed")
	case !limitInstalled:
		return skip(id, "no edge limit is installed (`can up --no-edge-limits`, or an older install)")
	}
	c := n.client(addr)
	defer c.CloseIdleConnections()
	start := n.Now()
	sent := 0
	for ; sent < rateAttempts; sent++ {
		resp, err := get(ctx, c, scheme(admin)+"://"+admin.Host+"/login")
		if err != nil {
			return skip(id, fmt.Sprintf("request %d got no answer", sent+1))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return pass(id, fmt.Sprintf("refused (429) after %d requests", sent+1))
		}
	}
	if elapsed := n.Now().Sub(start); sent <= rateBurst+int(elapsed.Seconds())+5 {
		return skip(id, "the connection is too slow to exceed the limit from here: run from a machine nearer the server")
	}
	return fail(id, fmt.Sprintf("%d sign-in page requests in a row were all accepted", sent))
}

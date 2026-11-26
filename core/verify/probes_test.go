package verify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEdge is a server's ports 80 and 443 with a private CA; every dial goes to it.
type fakeEdge struct {
	net   Net
	open  map[string]string // port -> local address
	roots *x509.CertPool
}

func newEdge(t *testing.T, life time.Duration, handler http.Handler, names ...string) *fakeEdge {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * 365 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ = x509.ParseCertificate(der)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"}, DNSNames: names, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(life), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsSrv := httptest.NewUnstartedServer(handler)
	tlsSrv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	tlsSrv.StartTLS()
	plain := httptest.NewServer(handler)
	t.Cleanup(func() { tlsSrv.Close(); plain.Close() })
	e := &fakeEdge{open: map[string]string{"443": tlsSrv.Listener.Addr().String(), "80": plain.Listener.Addr().String()}, roots: x509.NewCertPool()}
	e.roots.AddCert(ca)
	e.net = Net{Roots: e.roots, Now: time.Now, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(address)
		if to, ok := e.open[port]; ok {
			return (&net.Dialer{}).DialContext(ctx, network, to)
		}
		return nil, errors.New("connection refused")
	}}
	return e
}

var routes = []Route{
	{Namespace: "can-system", Host: "acme.example", TLS: true},
	{Namespace: "can-system", Host: "admin.acme.example", TLS: true},
}

func site() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Redirect(w, r, "https://"+r.Host+r.URL.Path, http.StatusTemporaryRedirect)
		}
	})
}

func TestPublicChecks(t *testing.T) {
	ctx := context.Background()
	e := newEdge(t, 60*24*time.Hour, site(), "acme.example", "admin.acme.example")
	cs := append(CheckHTTP(ctx, e.net, "203.0.113.7", "acme.example", routes),
		CheckPorts(ctx, e.net, "203.0.113.7"), CheckTLS(ctx, e.net, "203.0.113.7", routes))
	for _, c := range cs {
		if c.Status != Pass {
			t.Errorf("%s: %s %s", c.ID, c.Status, c.Detail)
		}
	}
	e.open["6443"] = e.open["80"]
	if c := CheckPorts(ctx, e.net, "203.0.113.7"); c.Status != Fail || !strings.Contains(c.Detail, "6443") {
		t.Errorf("open API port: %+v", c)
	}
	e.open = map[string]string{}
	if c := CheckPorts(ctx, e.net, "203.0.113.7"); c.Status != Skip {
		t.Errorf("unreachable server: %+v", c)
	}
	for _, c := range CheckHTTP(ctx, e.net, "203.0.113.7", "acme.example", routes) {
		if c.Status != Fail {
			t.Errorf("unreachable %s: %s", c.ID, c.Status)
		}
	}
}

func TestCertificateProblems(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		life  time.Duration
		names []string
		trust bool
		want  string
	}{
		"expiring":   {5 * 24 * time.Hour, []string{"acme.example", "admin.acme.example"}, true, "expires in"},
		"expired":    {-time.Minute, []string{"acme.example", "admin.acme.example"}, true, "expired"},
		"wrong name": {60 * 24 * time.Hour, []string{"other.example"}, true, "not valid for this name"},
		"untrusted":  {60 * 24 * time.Hour, []string{"acme.example", "admin.acme.example"}, false, "trusted authority"},
	} {
		e := newEdge(t, tc.life, site(), tc.names...)
		if !tc.trust {
			e.net.Roots = x509.NewCertPool()
		}
		if c := CheckTLS(ctx, e.net, "203.0.113.7", routes); c.Status != Fail || !strings.Contains(c.Detail, tc.want) {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if c := CheckTLS(ctx, Net{}, "x", []Route{{Host: "a.example"}}); c.Status != Skip {
		t.Errorf("no TLS routes: %+v", c)
	}
}

func TestCheckDNS(t *testing.T) {
	ok := []DNSName{{"acme.example", "ok"}, {"admin.acme.example", "ok"}}
	missing := []DNSName{{"acme.example", "ok"}, {"admin.acme.example", "missing"}}
	sslip := []Route{{Host: "203-0-113-7.sslip.io"}}
	for _, tc := range []struct {
		got  Check
		want Status
	}{
		{CheckDNS(ok, "acme.example", false, routes), Pass},
		{CheckDNS(missing, "acme.example", false, routes), Fail},
		{CheckDNS(missing, "acme.example", true, routes), Skip},
		{CheckDNS(missing, "acme.example", false, sslip), Skip},
		{CheckDNS(nil, "", false, nil), Skip},
	} {
		if tc.got.Status != tc.want {
			t.Errorf("%+v, want %s", tc.got, tc.want)
		}
	}
}

func TestRateLimit(t *testing.T) {
	ctx := context.Background()
	var n atomic.Int32
	var limit int32 = 31
	var posts atomic.Int32
	e := newEdge(t, time.Hour*24*60, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			posts.Add(1)
		}
		if n.Add(1) > limit {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}), "admin.acme.example")
	if c := CheckRateLimit(ctx, e.net, "203.0.113.7", "acme.example", routes, true); c.Status != Pass || n.Load() != 32 {
		t.Errorf("limited: %+v after %d", c, n.Load())
	}
	limit = 1000
	n.Store(0)
	if c := CheckRateLimit(ctx, e.net, "203.0.113.7", "acme.example", routes, true); c.Status != Fail || n.Load() != rateAttempts {
		t.Errorf("unlimited: %+v after %d", c, n.Load())
	}
	// A link so slow that the bucket refills as fast as it drains proves nothing.
	clock := time.Now()
	slow := e.net
	slow.Now = func() time.Time { clock = clock.Add(2 * time.Minute); return clock }
	if c := CheckRateLimit(ctx, slow, "203.0.113.7", "acme.example", routes, true); c.Status != Skip {
		t.Errorf("slow: %+v", c)
	}
	n.Store(0)
	if c := CheckRateLimit(ctx, e.net, "203.0.113.7", "acme.example", routes, false); c.Status != Skip || n.Load() != 0 {
		t.Errorf("no limit installed: %+v", c)
	}
	if posts.Load() != 0 {
		t.Error("the rate limit probe must not attempt a sign-in")
	}
}

func TestClientIP(t *testing.T) {
	var user string
	var requests atomic.Int32
	e := newEdge(t, time.Hour*24*60, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		b, _ := io.ReadAll(r.Body)
		_, rest, _ := strings.Cut(string(b), `"username":"`)
		user, _, _ = strings.Cut(rest, `"`)
		if r.URL.Path != "/api/v1/login" || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}), "admin.acme.example")
	for _, tc := range []struct {
		server, recorded string
		want             Status
	}{
		{"203.0.113.7", "198.51.100.20", Pass},
		{"203.0.113.7", "10.42.0.9", Fail},
		{"203.0.113.7", "127.0.0.1", Fail},
		{"203.0.113.7", "192.168.1.5", Fail},
		{"192.168.1.2", "192.168.1.5", Pass}, // a server on a LAN
		{"203.0.113.7", "", Skip},            // no audit entry
	} {
		r := &scripted{reply: func(s string) (string, error) {
			if !strings.Contains(s, "audit export") || !strings.Contains(s, user) || !strings.HasPrefix(user, "can-verify-") {
				return "", errors.New("unexpected")
			}
			if tc.recorded == "" {
				return "", nil
			}
			return `{"seq":9,"actor":"` + user + `","action":"admin.login.failed","detail":"{\"ip\":\"` + tc.recorded + `\"}"}` + "\n", nil
		}}
		before := requests.Load()
		c := CheckClientIP(context.Background(), e.net, r, tc.server, "acme.example", routes)
		if c.Status != tc.want || requests.Load() != before+1 {
			t.Errorf("%+v: %+v", tc, c)
		}
		if tc.recorded != "" && strings.Contains(c.Detail, tc.recorded) {
			t.Errorf("the detail names the address: %s", c.Detail)
		}
	}
}

func TestBackupRepo(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	snap := func(at string, tags string) string { return `{"id":"a","time":"` + at + `","tags":[` + tags + `]}` }
	for _, tc := range []struct {
		out  string
		err  error
		want Status
	}{
		{"[" + snap("2026-11-26T03:30:00Z", `"can"`) + "]", nil, Pass},
		{"[" + snap("2026-11-20T03:30:00Z", `"can"`) + "]", nil, Fail},
		{"[" + snap("2026-11-26T03:30:00Z", `"can","inconsistent"`) + "]", nil, Fail},
		{"[]", nil, Fail},
		{"CAN_NOT_INSTALLED\n", nil, Skip},
		{"Fatal: s3:https://bucket.example/x: Access Denied", errors.New("exit 1"), Fail},
	} {
		c := CheckBackupRepo(context.Background(), &scripted{reply: func(string) (string, error) { return tc.out, tc.err }}, now)
		if c.Status != tc.want || strings.Contains(c.Detail, "bucket.example") {
			t.Errorf("%q: %+v", tc.out, c)
		}
	}
}

func TestSMTP(t *testing.T) {
	r := &scripted{reply: func(string) (string, error) { return "CAN_TLS_OK\n", nil }}
	if c := CheckSMTP(context.Background(), r, "smtp.example.org", 587, false); c.Status != Pass ||
		!strings.Contains(r.ran[0], "-starttls smtp") || !strings.Contains(r.ran[0], "-verify_hostname 'smtp.example.org'") {
		t.Errorf("%+v\n%s", c, r.ran)
	}
	if c := CheckSMTP(context.Background(), r, "smtp.example.org", 465, true); c.Status != Pass || strings.Contains(r.ran[1], "starttls") {
		t.Errorf("%+v", c)
	}
	for out, want := range map[string]Status{"CAN_TLS_FAILED\n": Fail, "CAN_NO_OPENSSL\n": Skip} {
		r := &scripted{reply: func(string) (string, error) { return out, nil }}
		if c := CheckSMTP(context.Background(), r, "smtp.example.org", 587, false); c.Status != want || strings.Contains(c.Detail, "example.org") {
			t.Errorf("%s: %+v", out, c)
		}
	}
	// Nothing from company.yaml reaches the shell unchecked.
	n := len(r.ran)
	if c := CheckSMTP(context.Background(), r, "x';reboot;'", 25, false); c.Status != Fail || len(r.ran) != n {
		t.Errorf("hostile host: %+v", c)
	}
	if c := CheckSMTP(context.Background(), r, "", 587, false); c.Status != Skip {
		t.Errorf("%+v", c)
	}
}

func TestSSO(t *testing.T) {
	const issuer = "https://id.example.org/realms/acme"
	doc := `{"issuer":"` + issuer + `","authorization_endpoint":"https://id.example.org/a","token_endpoint":"https://id.example.org/t","jwks_uri":"https://id.example.org/k","id_token_signing_alg_values_supported":["RS256"]}`
	for _, tc := range []struct {
		doc  string
		want Status
	}{
		{doc, Pass},
		{strings.Replace(doc, `acme","auth`, `acme/","auth`, 1), Fail},
		{strings.Replace(doc, "RS256", "HS256", 1), Fail},
		{strings.Replace(doc, "https://id.example.org/t", "http://id.example.org/t", 1), Fail},
		{"<html>", Fail},
	} {
		r := &scripted{reply: func(string) (string, error) { return tc.doc, nil }}
		if c := CheckSSO(context.Background(), r, issuer); c.Status != tc.want {
			t.Errorf("%s: %+v", tc.doc, c)
		}
	}
	r := &scripted{reply: func(string) (string, error) { return "", nil }}
	if c := CheckSSO(context.Background(), r, "https://x.example/'$(reboot)"); c.Status != Fail || len(r.ran) != 0 {
		t.Errorf("hostile issuer: %+v", c)
	}
}

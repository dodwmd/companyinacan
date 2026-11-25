package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is a minimal in-process SMTP server.
type fakeRelay struct {
	addr     string
	roots    *x509.CertPool
	tls      *tls.Config
	implicit bool
	starttls bool              // advertise STARTTLS
	replies  map[string]string // command verb -> reply replacing the default

	mu       sync.Mutex
	commands []string
	data     []string
	secure   []bool // whether each command arrived over TLS
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func newFakeRelay(t *testing.T, implicit, starttls bool) *fakeRelay {
	t.Helper()
	cert, pool := selfSigned(t)
	f := &fakeRelay{roots: pool, implicit: implicit, starttls: starttls, replies: map[string]string{},
		tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRelay) relay(mode string) Relay {
	host, port, _ := net.SplitHostPort(f.addr)
	n, _ := strconv.Atoi(port)
	return Relay{Host: host, Port: n, TLS: mode, From: "no-reply@acme.example"}
}

func (f *fakeRelay) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	secure := false
	if f.implicit {
		c, secure = tls.Server(c, f.tls), true
	}
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, _, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.secure = append(f.secure, secure)
		reply := f.replies[verb]
		f.mu.Unlock()
		if reply != "" {
			say(reply)
			continue
		}
		switch verb {
		case "EHLO", "HELO":
			say("250-fake")
			if f.starttls && !secure {
				say("250-STARTTLS")
			}
			say("250 AUTH PLAIN")
		case "STARTTLS":
			say("220 go ahead")
			c, secure = tls.Server(c, f.tls), true
			r = bufio.NewReader(c)
		case "AUTH":
			say("235 ok")
		case "MAIL", "RCPT":
			say("250 ok")
		case "DATA":
			say("354 send")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			f.mu.Lock()
			f.data = append(f.data, b.String())
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 no")
		}
	}
}

func (f *fakeRelay) log() (cmds []string, secure []bool, data []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([]bool(nil), f.secure...), append([]string(nil), f.data...)
}

func testMessage() []byte {
	return message{ID: "eml_1", From: "no-reply@acme.example", To: "sales@acme.example", ReplyTo: "lead@visitor.example",
		Subject: "Nouvelle demande é " + strings.Repeat("long ", 20), Text: "line one\n.\n.hidden\nBcc: victim@evil.example\n" + strings.Repeat("x", 2000), Date: time.Unix(0, 0)}.build()
}

func TestSendRequiresTLSAndAuthenticatesOnlyAfterIt(t *testing.T) {
	for _, mode := range []string{TLSStartTLS, TLSImplicit} {
		f := newFakeRelay(t, mode == TLSImplicit, mode == TLSStartTLS)
		r := f.relay(mode)
		r.Username, r.Password = "apikey", "s3cr3t-relay-pass"
		tr := SMTP{AllowPrivate: true, RootCAs: f.roots}
		if err := tr.Send(context.Background(), r, "sales@acme.example", testMessage()); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		cmds, secure, data := f.log()
		wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00apikey\x00s3cr3t-relay-pass"))
		seen := false
		for i, c := range cmds {
			if strings.HasPrefix(c, "AUTH") {
				seen = c == wantAuth && secure[i]
			}
			if (strings.HasPrefix(c, "MAIL") || strings.HasPrefix(c, "RCPT") || strings.HasPrefix(c, "AUTH")) && !secure[i] {
				t.Errorf("%s: %q was sent without TLS", mode, c)
			}
		}
		if !seen || len(data) != 1 {
			t.Fatalf("%s: auth or data missing: %v", mode, cmds)
		}
	}
}

func TestSendNeverFallsBackToPlaintext(t *testing.T) {
	f := newFakeRelay(t, false, false) // no STARTTLS on offer (or stripped by an attacker)
	r := f.relay(TLSStartTLS)
	r.Username, r.Password = "u", "p"
	err := SMTP{AllowPrivate: true, RootCAs: f.roots}.Send(context.Background(), r, "sales@acme.example", testMessage())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("want a STARTTLS error, got %v", err)
	}
	cmds, _, data := f.log()
	for _, c := range cmds {
		if strings.HasPrefix(c, "AUTH") || strings.HasPrefix(c, "MAIL") {
			t.Fatalf("sent %q without TLS", c)
		}
	}
	if len(data) != 0 {
		t.Fatal("message sent in clear")
	}
}

func TestSendVerifiesTheCertificate(t *testing.T) {
	f := newFakeRelay(t, true, false)
	err := SMTP{AllowPrivate: true}.Send(context.Background(), f.relay(TLSImplicit), "sales@acme.example", testMessage())
	if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("an untrusted certificate must be refused: %v", err)
	}
}

func TestSendRefusesNonPublicRelaysByDefault(t *testing.T) {
	f := newFakeRelay(t, false, false)
	err := SMTP{}.Send(context.Background(), f.relay(TLSNone), "sales@acme.example", testMessage())
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("loopback relay: %v", err)
	}
	if cmds, _, _ := f.log(); len(cmds) != 0 {
		t.Fatalf("talked to a refused address: %v", cmds)
	}
	// Even with private addresses allowed, link-local (cloud metadata) stays refused.
	for _, host := range []string{"169.254.169.254", "0.0.0.0"} {
		err := SMTP{AllowPrivate: true, Timeout: 2 * time.Second}.Send(context.Background(), Relay{Host: host, Port: 25, TLS: TLSStartTLS, From: "a@b.example"}, "c@d.example", testMessage())
		if err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Fatalf("%s: %v", host, err)
		}
	}
}

func TestSendErrorsAreClassifiedAndCarryNoServerText(t *testing.T) {
	for _, tc := range []struct {
		verb, reply string
		permanent   bool
		code        int
	}{
		{"RCPT", "550 5.1.1 <sales@acme.example> no such user", true, 550},
		{"RCPT", "450 4.2.0 <sales@acme.example> try later", false, 450},
		{"AUTH", "535 5.7.8 bad credentials s3cr3t-relay-pass", false, 535},
		{"MAIL", "553 sender rejected", true, 553},
	} {
		f := newFakeRelay(t, true, false)
		f.replies[tc.verb] = tc.reply
		r := f.relay(TLSImplicit)
		r.Username, r.Password = "u", "s3cr3t-relay-pass"
		err := SMTP{AllowPrivate: true, RootCAs: f.roots}.Send(context.Background(), r, "sales@acme.example", testMessage())
		se, ok := err.(*SendError)
		if !ok || se.Permanent != tc.permanent || se.Code != tc.code {
			t.Fatalf("%s %q: %#v", tc.verb, tc.reply, err)
		}
		for _, leak := range []string{"sales@acme.example", "s3cr3t", "no such user"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error text leaks %q: %s", leak, err)
			}
		}
	}
}

func TestMessageHeadersCannotBeInjected(t *testing.T) {
	raw := testMessage()
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line of %d bytes", len(line))
		}
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("Bcc") != "" || m.Header.Get("To") != "<sales@acme.example>" || m.Header.Get("Reply-To") != "<lead@visitor.example>" {
		t.Fatalf("headers: %v", m.Header)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || !strings.HasPrefix(subject, "Nouvelle demande é long") {
		t.Fatalf("subject %q %v", subject, err)
	}
	if strings.Count(string(raw), "\n") != strings.Count(string(raw), "\r\n") {
		t.Fatal("bare LF in the message")
	}
}

func TestSubjectAndTextValidation(t *testing.T) {
	for _, s := range []string{"", "a\r\nBcc: x@y.example", "a\nb", "a\rb", "a\x00b", "a\tb", "a b", strings.Repeat("x", MaxSubjectBytes+1), "\xff"} {
		if validSubject(s) {
			t.Errorf("subject %q accepted", s)
		}
	}
	for _, s := range []string{"", "a\x00b", "a\x1bb", strings.Repeat("x", MaxTextBytes+1), "\xff"} {
		if validText(s) {
			t.Errorf("text %.20q accepted", s)
		}
	}
}

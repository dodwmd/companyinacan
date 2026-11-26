package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSinkKeepsWhatItReceives(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &sink{}
	go s.accept(ln)
	if err := smtp.SendMail(ln.Addr().String(), nil, "no-reply@acme.example", []string{"sales@acme.example"}, []byte("Subject: hi\r\n\r\n.dotted line\r\nbye\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.list()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := s.list()
	if len(got) != 1 || got[0].From != "no-reply@acme.example" || got[0].To[0] != "sales@acme.example" || !strings.Contains(got[0].Data, "\r\n.dotted line\r\n") {
		t.Fatalf("%+v", got)
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/messages", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent || len(s.list()) != 0 {
		t.Fatal(resp, err)
	}
	_ = resp.Body.Close()
}

// With TLS and credentials the sink takes mail the way can-core sends it: STARTTLS, then AUTH PLAIN.
func TestSinkWithSTARTTLSAndAuth(t *testing.T) {
	certFile, keyFile, pool := testCert(t)
	s := &sink{}
	if err := s.configure("", "", "u", "p"); err == nil {
		t.Fatal("credentials without TLS must be refused")
	}
	if err := s.configure(certFile, keyFile, "relay-user", "relay-password"); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.accept(ln)

	session := func(password string) error {
		c, err := smtp.Dial(ln.Addr().String())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("no STARTTLS offered")
		}
		if err := c.StartTLS(&tls.Config{ServerName: "127.0.0.1", RootCAs: pool, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
		if err := c.Auth(smtp.PlainAuth("", "relay-user", password, "127.0.0.1")); err != nil {
			return err
		}
		if err := c.Mail("no-reply@acme.example"); err != nil {
			return err
		}
		if err := c.Rcpt("sales@acme.example"); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte("Subject: hi\r\n\r\nbye\r\n")); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	if err := session("wrong"); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if err := smtp.SendMail(ln.Addr().String(), nil, "no-reply@acme.example", []string{"sales@acme.example"}, []byte("x\r\n")); err == nil {
		t.Fatal("mail was taken without authentication")
	}
	if err := session("relay-password"); err != nil {
		t.Fatal(err)
	}
	got := s.list()
	if len(got) != 1 || !got[0].TLS || got[0].Auth != "relay-user" {
		t.Fatalf("%+v", got)
	}
}

// testCert writes a self-signed certificate for 127.0.0.1.
func testCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for f, b := range map[string][]byte{
		certFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyFile:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 section 5 / Appendix A: the worked example, byte for byte.
func TestEncryptRFC8291Vector(t *testing.T) {
	plaintext := dec(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	asPriv, err := ecdh.P256().NewPrivateKey(dec(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPub := dec(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	auth := dec(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := dec(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	got, err := encrypt(plaintext, uaPub, auth, asPriv, salt)
	if err != nil {
		t.Fatal(err)
	}
	if base64.RawURLEncoding.EncodeToString(got) != want {
		t.Fatalf("ciphertext differs from RFC 8291 Appendix A\n got %s\nwant %s", base64.RawURLEncoding.EncodeToString(got), want)
	}
}

func TestEncryptRoundTripAndLimits(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	body, err := Encrypt([]byte(`{"t":"Approval waiting"}`), ua.PublicKey().Bytes(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 86 || body[20] != 65 {
		t.Fatalf("header malformed: %d bytes", len(body))
	}
	if _, err := Encrypt(make([]byte, MaxPayload+1), ua.PublicKey().Bytes(), auth); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, err := Encrypt([]byte("x"), []byte("short"), auth); err == nil {
		t.Fatal("bad user-agent key accepted")
	}
}

func TestVAPIDHeader(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	h, err := k.authorization("https://push.example.com/send/abc", "mailto:ops@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "vapid t=") || !strings.HasSuffix(h, ", k="+k.PublicKey()) {
		t.Fatalf("header %q", h)
	}
	jwt := strings.TrimSuffix(strings.TrimPrefix(h, "vapid t="), ", k="+k.PublicKey())
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	var hdr, claims map[string]any
	_ = json.Unmarshal(dec(t, parts[0]), &hdr)
	_ = json.Unmarshal(dec(t, parts[1]), &claims)
	if hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("header %v", hdr)
	}
	if claims["aud"] != "https://push.example.com" || claims["sub"] != "mailto:ops@example.com" {
		t.Fatalf("claims %v", claims)
	}
	exp := int64(claims["exp"].(float64))
	if exp <= now.Unix() || exp > now.Add(24*time.Hour).Unix() {
		t.Fatalf("exp %d must be within 24h (RFC 8292)", exp)
	}
	sig := dec(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw r||s of 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	pub := &k.priv.PublicKey
	if !ecdsa.VerifyASN1(pub, digest[:], asn1Sig(sig)) {
		t.Fatal("signature does not verify")
	}
	if pub.Curve != elliptic.P256() {
		t.Fatal("not P-256")
	}
}

func TestKeyMarshalRoundTrip(t *testing.T) {
	k, _ := GenerateKey()
	k2, err := ParseKey(k.Marshal())
	if err != nil || k2.PublicKey() != k.PublicKey() {
		t.Fatalf("round trip: %v", err)
	}
	if len(dec(t, k.PublicKey())) != 65 {
		t.Fatal("public key is not an uncompressed point")
	}
	for _, bad := range []string{"", "!!", "AAAA", strings.Repeat("A", 43)} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func goodSub(t *testing.T, endpoint string) Subscription {
	t.Helper()
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	return Subscription{Endpoint: endpoint, P256dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
}

func TestValidateSubscription(t *testing.T) {
	ok := goodSub(t, "https://fcm.googleapis.com/fcm/send/abc")
	if err := ok.Validate(false); err != nil {
		t.Fatal(err)
	}
	long := "https://push.example.com/" + strings.Repeat("a", 600)
	bad := map[string]string{
		"http":            "http://push.example.com/x",
		"ip literal":      "https://203.0.113.9/x",
		"ipv6 literal":    "https://[2606:4700::1111]/x",
		"loopback":        "https://127.0.0.1/x",
		"localhost":       "https://localhost/x",
		"userinfo":        "https://u:p@push.example.com/x",
		"odd port":        "https://push.example.com:8443/x",
		"no host":         "https:///x",
		"fragment":        "https://push.example.com/x#f",
		"too long":        long,
		"control":         "https://push.example.com/x\r\nHost: evil",
		"javascript":      "javascript:alert(1)",
		"empty":           "",
		"single label":    "https://intranet/x",
		"internal suffix": "https://api.cluster.local/x",
	}
	for name, ep := range bad {
		s := ok
		s.Endpoint = ep
		if err := s.Validate(false); err == nil {
			t.Errorf("%s: %q accepted", name, ep)
		}
	}
	s := ok
	s.P256dh = "AAAA"
	if s.Validate(false) == nil {
		t.Error("short p256dh accepted")
	}
	s = ok
	s.P256dh = base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...)) // not on the curve
	if s.Validate(false) == nil {
		t.Error("off-curve p256dh accepted")
	}
	s = ok
	s.Auth = "AAAA"
	if s.Validate(false) == nil {
		t.Error("short auth secret accepted")
	}
}

// fakePush is an httptest push service. It records the request and answers with a status.
type fakePush struct {
	srv    *httptest.Server
	got    *http.Request
	body   []byte
	status int
	header http.Header
}

func newFakePush(t *testing.T) *fakePush {
	f := &fakePush{status: 201}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = r
		f.body, _ = io.ReadAll(r.Body)
		for k, v := range f.header {
			w.Header()[k] = v
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(strings.Repeat("x", 100<<10)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testClient(t *testing.T, f *fakePush) *Client {
	k, _ := GenerateKey()
	return &Client{Key: k, Contact: "mailto:ops@example.com", HTTP: f.srv.Client(), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestSendHeadersAndStatuses(t *testing.T) {
	f := newFakePush(t)
	c := testClient(t, f)
	sub := goodSub(t, f.srv.URL+"/send/abc")
	res, err := c.Send(context.Background(), sub, []byte(`{"t":"x"}`), Options{TTL: 3600, Urgency: UrgencyHigh, Topic: "approval-1"})
	if err != nil || res.Status != 201 {
		t.Fatalf("%v %+v", err, res)
	}
	h := f.got.Header
	for k, want := range map[string]string{"Content-Encoding": "aes128gcm", "Ttl": "3600", "Urgency": "high", "Topic": "approval-1", "Content-Type": "application/octet-stream"} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if !strings.HasPrefix(h.Get("Authorization"), "vapid t=") {
		t.Errorf("authorization %q", h.Get("Authorization"))
	}
	if len(f.body) < 86 {
		t.Errorf("body %d bytes", len(f.body))
	}
	if strings.Contains(string(f.body), `"t"`) {
		t.Error("payload is not encrypted")
	}

	f.status = 410
	if _, err := c.Send(context.Background(), sub, []byte("x"), Options{}); err != ErrGone {
		t.Fatalf("410 -> %v, want ErrGone", err)
	}
	f.status = 404
	if _, err := c.Send(context.Background(), sub, []byte("x"), Options{}); err != ErrGone {
		t.Fatalf("404 -> %v, want ErrGone", err)
	}
	f.status, f.header = 429, http.Header{"Retry-After": {"7"}}
	res, err = c.Send(context.Background(), sub, []byte("x"), Options{})
	if err == nil || !res.Retryable || res.RetryAfter != 7*time.Second {
		t.Fatalf("429 -> %v %+v", err, res)
	}
	f.status, f.header = 503, nil
	res, err = c.Send(context.Background(), sub, []byte("x"), Options{})
	if err == nil || !res.Retryable {
		t.Fatalf("503 -> %v %+v", err, res)
	}
	f.status = 400
	res, err = c.Send(context.Background(), sub, []byte("x"), Options{})
	if err == nil || res.Retryable {
		t.Fatalf("400 must not be retried: %v %+v", err, res)
	}
	if strings.Contains(err.Error(), f.srv.URL) {
		t.Fatalf("error leaks the endpoint URL: %v", err)
	}
}

func TestSendNeverFollowsRedirects(t *testing.T) {
	hit := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	f := newFakePush(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	c := NewClient(testKey(t), "mailto:ops@example.com")
	if c.HTTP.CheckRedirect == nil {
		t.Fatal("default client follows redirects")
	}
	// Keep the default redirect policy; only swap the transport so the test server's certificate
	// is trusted and loopback is reachable.
	c.HTTP = &http.Client{Transport: f.srv.Client().Transport, CheckRedirect: c.HTTP.CheckRedirect}
	res, err := c.Send(context.Background(), goodSub(t, f.srv.URL+"/x"), []byte("x"), Options{})
	if err == nil || hit || res.Status != 307 {
		t.Fatalf("redirect handling: err=%v hit=%v status=%d", err, hit, res.Status)
	}
}

func TestDefaultClientRefusesPrivateDestinations(t *testing.T) {
	f := newFakePush(t) // listens on 127.0.0.1
	c := NewClient(testKey(t), "mailto:a@b.c")
	_, err := c.Send(context.Background(), goodSub(t, f.srv.URL+"/x"), []byte("x"), Options{})
	if err == nil || f.got != nil {
		t.Fatalf("loopback push service was contacted: %v", err)
	}
}

func testKey(t *testing.T) *Key {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func asn1Sig(raw []byte) []byte {
	b, _ := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])})
	return b
}

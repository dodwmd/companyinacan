package adminapi

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/notify/webpush"
)

// pushOK is a Web Push sender that accepts everything: no push service is ever contacted.
type pushOK struct{}

func (pushOK) Send(context.Context, webpush.Subscription, []byte, webpush.Options) (webpush.Result, error) {
	return webpush.Result{Status: 201}, nil
}

func subBody(t *testing.T, endpoint string) map[string]any {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	return map[string]any{"endpoint": endpoint, "expirationTime": nil, "keys": map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(make([]byte, 16))}}
}

func TestPushRoutesRequireSessionAndCSRF(t *testing.T) {
	h := newPHarness(t)
	for _, r := range []struct{ m, p string }{{"GET", "/api/v1/push/vapid-public-key"}, {"GET", "/api/v1/push/subscriptions"}, {"POST", "/api/v1/push/subscriptions"}, {"DELETE", "/api/v1/push/subscriptions"}, {"POST", "/api/v1/push/test"}, {"POST", "/api/v1/push/vapid-key/rotate"}} {
		req, _ := http.NewRequest(r.m, h.srv.URL+r.p, bytes.NewReader([]byte("{}")))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", r.m, r.p, resp.StatusCode)
		}
	}
	alice := h.login("alice")
	for _, r := range []struct{ m, p string }{{"POST", "/api/v1/push/subscriptions"}, {"DELETE", "/api/v1/push/subscriptions"}, {"POST", "/api/v1/push/test"}} {
		b, _ := json.Marshal(subBody(t, "https://fcm.googleapis.com/fcm/send/x"))
		req, _ := http.NewRequest(r.m, h.srv.URL+r.p, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(alice.cookie) // session but no CSRF header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s %s without CSRF = %d, want 403", r.m, r.p, resp.StatusCode)
		}
	}
	if code, _ := h.call(alice, "GET", "/api/v1/push/vapid-public-key", nil); code != 200 {
		t.Fatal("any signed-in user may read the public key")
	}
}

func TestPushSubscribeListUnsubscribe(t *testing.T) {
	h := newPHarness(t)
	alice, carol := h.login("alice"), h.login("carol")
	code, body := h.call(alice, "GET", "/api/v1/push/vapid-public-key", nil)
	var k struct {
		PublicKey string `json:"public_key"`
	}
	_ = json.Unmarshal([]byte(body), &k)
	if code != 200 || len(k.PublicKey) != 87 {
		t.Fatalf("%d %s", code, body)
	}
	ep := "https://fcm.googleapis.com/fcm/send/secret-capability-token"
	if code, body := h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, ep)); code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	code, body = h.call(alice, "GET", "/api/v1/push/subscriptions", nil)
	if code != 200 || !strings.Contains(body, "fcm.googleapis.com") {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(body, "secret-capability-token") || strings.Contains(body, "p256dh") {
		t.Fatalf("the list exposes the endpoint capability or keys: %s", body)
	}
	if code, body := h.call(carol, "GET", "/api/v1/push/subscriptions", nil); code != 200 || strings.Contains(body, "fcm") {
		t.Fatalf("carol sees alice's devices: %s", body)
	}
	if code, _ := h.call(carol, "DELETE", "/api/v1/push/subscriptions", map[string]string{"endpoint": ep}); code != 404 {
		t.Fatalf("carol removing alice's device = %d, want 404", code)
	}
	if code, _ := h.call(alice, "DELETE", "/api/v1/push/subscriptions", map[string]string{"endpoint": ep}); code != 204 {
		t.Fatalf("unsubscribe = %d", code)
	}
}

func TestPushSubscribeRejectsUnsafeEndpointsAndOversizedBodies(t *testing.T) {
	h := newPHarness(t)
	alice := h.login("alice")
	for _, ep := range []string{"http://fcm.googleapis.com/x", "https://169.254.169.254/latest", "https://127.0.0.1/x", "https://localhost/x", "https://u:p@fcm.googleapis.com/x", "https://fcm.googleapis.com:8443/x", "file:///etc/passwd", ""} {
		if code, _ := h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, ep)); code != 400 {
			t.Errorf("endpoint %q = %d, want 400", ep, code)
		}
	}
	big := subBody(t, "https://push.example.com/"+strings.Repeat("a", 20000))
	if code, _ := h.call(alice, "POST", "/api/v1/push/subscriptions", big); code != 400 && code != 413 {
		t.Errorf("oversized endpoint = %d", code)
	}
	bad := subBody(t, "https://push.example.com/x")
	bad["keys"].(map[string]string)["auth"] = "AAAA"
	if code, _ := h.call(alice, "POST", "/api/v1/push/subscriptions", bad); code != 400 {
		t.Errorf("short auth secret = %d", code)
	}
	unknown := subBody(t, "https://push.example.com/x")
	unknown["admin"] = true
	if code, _ := h.call(alice, "POST", "/api/v1/push/subscriptions", unknown); code != 400 {
		t.Errorf("unknown field = %d", code)
	}
}

func TestPushPerUserCap(t *testing.T) {
	h := newPHarness(t)
	alice := h.login("alice")
	for i := 0; i < 10; i++ {
		if code, body := h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, "https://push.example.com/s/"+strings.Repeat("a", i+1))); code != 201 {
			t.Fatalf("#%d: %d %s", i, code, body)
		}
	}
	if code, _ := h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, "https://push.example.com/s/extra")); code != 409 {
		t.Fatalf("11th device = %d, want 409", code)
	}
}

func TestPushTestNotificationAndRateLimit(t *testing.T) {
	h := newPHarness(t)
	alice := h.login("alice")
	if code, _ := h.call(alice, "POST", "/api/v1/push/test", map[string]string{}); code != 409 {
		t.Fatalf("no device = %d, want 409", code)
	}
	h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, "https://push.example.com/s/a"))
	if code, body := h.call(alice, "POST", "/api/v1/push/test", map[string]string{}); code != 200 || !strings.Contains(body, `"sent":1`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/push/test", map[string]string{}); code != 429 {
		t.Fatalf("second test = %d, want 429", code)
	}
}

func TestPushRotateNeedsRBACManageAndDropsSubscriptions(t *testing.T) {
	h := newPHarness(t)
	alice, root := h.login("alice"), h.login("root")
	h.call(alice, "POST", "/api/v1/push/subscriptions", subBody(t, "https://push.example.com/s/a"))
	if code, _ := h.call(alice, "POST", "/api/v1/push/vapid-key/rotate", map[string]string{}); code != 403 {
		t.Fatalf("rotate without rbac.manage = %d, want 403", code)
	}
	if code, _ := h.call(root, "POST", "/api/v1/push/vapid-key/rotate", map[string]string{}); code != 204 {
		t.Fatalf("rotate = %d", code)
	}
	if _, body := h.call(alice, "GET", "/api/v1/push/subscriptions", nil); strings.Contains(body, "push.example.com") {
		t.Fatalf("subscriptions survived rotation: %s", body)
	}
}

var _ = io.Discard

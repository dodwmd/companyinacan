package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPWebhook(t *testing.T) {
	var body string
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			b, _ := io.ReadAll(r.Body)
			body = string(b)
		case "/redir":
			http.Redirect(w, r, "/followed", http.StatusFound)
		case "/followed":
			redirected = true
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	h := HTTPWebhook{AllowInsecure: true}
	ctx := context.Background()
	if err := h.Post(ctx, srv.URL+"/ok", "hi"); err != nil || body != `{"text":"hi"}` {
		t.Fatalf("%v %q", err, body)
	}
	if err := h.Post(ctx, srv.URL+"/redir", "x"); err == nil || redirected {
		t.Fatalf("redirects must not be followed: %v %v", err, redirected)
	}
	err := h.Post(ctx, srv.URL+"/secret-token-123", "x")
	if err == nil || strings.Contains(err.Error(), "secret-token-123") {
		t.Fatalf("error must not carry the URL: %v", err)
	}
	if err := (HTTPWebhook{}).Post(ctx, srv.URL+"/ok", "x"); err == nil {
		t.Fatal("plain http accepted without AllowInsecure")
	}
}

package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// launchApp posts a launch as a browser at the admin address and returns the cookies set.
func (h *pHarness) launchApp(s sess, path, body string) (int, string, []*http.Cookie) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+path, strings.NewReader(body))
	req.Host = "admin.example.org"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	req.AddCookie(s.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Cookies()
}

func TestAppsListAndLaunch(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	code, body := h.call(root, "GET", "/api/v1/apps", nil)
	if code != 200 || !strings.Contains(body, `"id":"expenses"`) {
		t.Fatalf("list = %d %s", code, body)
	}
	code, body, cookies := h.launchApp(root, "/api/v1/apps/expenses/launch", `{"return_path":"/reports"}`)
	if code != 200 {
		t.Fatalf("launch = %d %s", code, body)
	}
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if !strings.HasPrefix(out.URL, "https://expenses.example.org/login/ticket?t=cant_") {
		t.Fatalf("url %q", out.URL)
	}
	tk := out.URL[strings.Index(out.URL, "t=")+2:]
	if len(cookies) != 1 || cookies[0].Name != "__Secure-can_launch_expenses" || cookies[0].Domain != "example.org" {
		t.Fatalf("launch cookie: %+v", cookies)
	}
	if _, ok := h.tickets.Redeem(context.Background(), "expenses", tk, ""); ok {
		t.Fatal("redeemed without the cookie")
	}
	r, ok := h.tickets.Redeem(context.Background(), "expenses", tk, cookies[0].Value)
	if !ok || r.Subject != "user:root" || r.ReturnPath != "/reports" {
		t.Fatalf("redeem %+v %v", r, ok)
	}
}

func TestAppsRefusals(t *testing.T) {
	h := newPHarness(t)
	alice := h.login("alice") // no roles
	if code, body := h.call(alice, "GET", "/api/v1/apps", nil); code != 200 || strings.Contains(body, "expenses") {
		t.Fatalf("alice lists %d %s", code, body)
	}
	// No permission, an unknown module and a module without a UI are the same answer.
	for _, id := range []string{"expenses", "ghost"} {
		if code, _ := h.call(alice, "POST", "/api/v1/apps/"+id+"/launch", nil); code != 404 {
			t.Errorf("launch %s = %d", id, code)
		}
	}
	root := h.login("root")
	if code, _ := h.call(root, "POST", "/api/v1/apps/expenses/launch", map[string]string{"return_path": "https://evil.example/"}); code != 400 {
		t.Fatalf("hostile return path = %d", code)
	}
	// CSRF and session: a launch without the token or the cookie is refused.
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/apps/expenses/launch", nil)
	req.AddCookie(root.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("launch without CSRF = %d", resp.StatusCode)
	}
	resp, _ = http.Post(h.srv.URL+"/api/v1/apps/expenses/launch", "application/json", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous launch = %d", resp.StatusCode)
	}
}

func TestLaunchIsRateLimited(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	last := 0
	for i := 0; i < 40; i++ {
		last, _ = h.call(root, "POST", "/api/v1/apps/expenses/launch", nil)
		if last == 429 {
			if i < 30 {
				t.Fatalf("limited after only %d launches", i)
			}
			return
		}
	}
	t.Fatalf("40 launches in a minute were never limited (last %d)", last)
}

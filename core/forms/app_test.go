package forms

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretsFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := map[string]string{
		"CAN_FORMS_TOKEN_KEY": "0123456789abcdef0123456789abcdef",
		"CAN_FORMS_API_TOKEN": "admin-token-0123456789",
	}
	s, err := SecretsFromEnv(env(good))
	if err != nil || string(s.TokenKey) != good["CAN_FORMS_TOKEN_KEY"] || s.APIToken != good["CAN_FORMS_API_TOKEN"] {
		t.Fatalf("%+v %v", s, err)
	}
	cases := map[string]map[string]string{
		"missing key":    {"CAN_FORMS_API_TOKEN": good["CAN_FORMS_API_TOKEN"]},
		"short key":      {"CAN_FORMS_TOKEN_KEY": "short", "CAN_FORMS_API_TOKEN": good["CAN_FORMS_API_TOKEN"]},
		"missing api":    {"CAN_FORMS_TOKEN_KEY": good["CAN_FORMS_TOKEN_KEY"]},
		"short api":      {"CAN_FORMS_TOKEN_KEY": good["CAN_FORMS_TOKEN_KEY"], "CAN_FORMS_API_TOKEN": "short"},
		"short read tok": {"CAN_FORMS_TOKEN_KEY": good["CAN_FORMS_TOKEN_KEY"], "CAN_FORMS_API_TOKEN": good["CAN_FORMS_API_TOKEN"], "CAN_FORMS_API_READ_TOKEN": "x"},
	}
	for name, m := range cases {
		if _, err := SecretsFromEnv(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewAppRequiresSecretsForConfiguredFeatures(t *testing.T) {
	cfg, _ := ParseConfig([]byte("webhook: {url: 'http://x.example/h'}\n" + minimalCfg))
	cfg.DBPath = filepath.Join(t.TempDir(), "f.db")
	sec := Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: adminToken}
	if _, err := NewApp(cfg, sec, Options{}); err == nil || !strings.Contains(err.Error(), "CAN_FORMS_WEBHOOK_SECRET") {
		t.Fatalf("webhook without secret must fail: %v", err)
	}
	cfg2, _ := ParseConfig([]byte("captcha: {provider: turnstile}\n" + minimalCfg))
	cfg2.DBPath = filepath.Join(t.TempDir(), "g.db")
	if _, err := NewApp(cfg2, sec, Options{}); err == nil {
		t.Fatal("captcha without secret must fail")
	}
}

func TestPurgeRunsRetention(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.seed(3) // retention = clock+1h
	h.clock.advance(2 * time.Hour)
	n, err := h.app.Purge(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("purged %d err %v", n, err)
	}
}

func TestServeEndToEndAndGracefulShutdown(t *testing.T) {
	cfg, err := ParseConfig([]byte(harnessCfg))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = filepath.Join(t.TempDir(), "f.db")
	mailer := &fakeMailer{}
	app, err := NewApp(cfg, Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: adminToken, CaptchaSecret: "x"}, Options{Mailer: mailer})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := net.Listen("tcp", "127.0.0.1:0")
	api, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, pub, api) }()

	base := "http://" + pub.Addr().String()
	waitHealthy(t, base+"/healthz")
	// Real clock: fetch a token, wait the minimum fill time, submit.
	resp, err := http.Get(base + "/_forms/contact/token")
	if err != nil {
		t.Fatal(err)
	}
	var tk struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&tk)
	_ = resp.Body.Close()
	time.Sleep(3200 * time.Millisecond)
	v := url.Values{"name": {"Ada"}, "email": {"ada@example.com"}, "message": {"hi"}, "_t": {tk.Token}}
	resp, err = http.PostForm(base+"/_forms/contact", v)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+api.Addr().String()+"/v1/leads", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("api: %v %v", err, resp)
	}
	var list struct{ Items []Lead }
	_ = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if len(list.Items) != 1 {
		t.Fatalf("leads over the wire: %d", len(list.Items))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if len(mailer.all()) != 1 {
		t.Fatalf("pending notification must be flushed on shutdown: %d", len(mailer.all()))
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("server still accepting after shutdown")
	}
}

func waitHealthy(t *testing.T, u string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if r, err := http.Get(u); err == nil {
			_ = r.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not come up")
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forms.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMainCheckConfig(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"check-config", "-config", writeCfg(t, minimalCfg)}, func(string) string { return "" }, &out, &errb); code != 0 {
		t.Fatalf("valid config: %d %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Main([]string{"check-config", "-config", writeCfg(t, "bogus: 1")}, func(string) string { return "" }, &out, &errb); code != 2 || errb.Len() == 0 {
		t.Fatalf("invalid config: %d %q", code, errb.String())
	}
	if code := Main([]string{"nope"}, func(string) string { return "" }, &out, &errb); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code := Main([]string{"version"}, func(string) string { return "" }, &out, &errb); code != 0 || !strings.Contains(out.String(), "can-forms") {
		t.Fatalf("version: %d %q", code, out.String())
	}
}

func TestMainHealthcheck(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	var out, errb bytes.Buffer
	if code := Main([]string{"healthcheck", "-addr", ln.Addr().String()}, func(string) string { return "" }, &out, &errb); code != 0 {
		t.Fatalf("healthy: %d %s", code, errb.String())
	}
	_ = srv.Close()
	if code := Main([]string{"healthcheck", "-addr", ln.Addr().String()}, func(string) string { return "" }, &out, &errb); code != 1 {
		t.Fatalf("unhealthy: %d", code)
	}
}

func TestMainRunFailsWithoutSecrets(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"serve", "-config", writeCfg(t, minimalCfg)}, func(string) string { return "" }, &out, &errb); code != 2 || !strings.Contains(errb.String(), "CAN_FORMS_TOKEN_KEY") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

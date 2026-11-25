package obslog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNewRequestID(t *testing.T) {
	good := []string{"abc-123_DEF.x", strings.Repeat("a", 64)}
	for _, g := range good {
		if got := NewRequestID(g); got != g {
			t.Errorf("valid id %q replaced by %q", g, got)
		}
	}
	bad := []string{"", strings.Repeat("a", 65), "has space", "new\nline", "quote\"", "ünï", "a;b", "<script>"}
	for _, b := range bad {
		got := NewRequestID(b)
		if got == b || len(got) != 16 || strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("untrusted id %q gave %q, want a generated 16-hex id", b, got)
		}
	}
	a, b := NewRequestID(""), NewRequestID("")
	if a == b {
		t.Error("generated ids repeat")
	}
}

func TestRequestIDContext(t *testing.T) {
	ctx := WithRequestID(context.Background(), "rid-1")
	if RequestID(ctx) != "rid-1" || RequestID(context.Background()) != "" {
		t.Fatal("context round trip")
	}
}

func logLine(t *testing.T, fn func(*slog.Logger)) (map[string]any, string) {
	t.Helper()
	var b bytes.Buffer
	fn(New(&b, Options{Service: "can-test", Version: "v9"}))
	raw := b.String()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, raw)
	}
	return m, raw
}

func TestJSONWithServiceAndRequestID(t *testing.T) {
	m, _ := logLine(t, func(l *slog.Logger) {
		l.InfoContext(WithRequestID(context.Background(), "rid-7"), "hello")
	})
	if m["service"] != "can-test" || m["version"] != "v9" || m["request_id"] != "rid-7" || m["msg"] != "hello" {
		t.Fatalf("unexpected record %v", m)
	}
}

func TestRedactsSecretKeys(t *testing.T) {
	const seed = "s3cr3t-VALUE-9f2c"
	keys := []string{"password", "Password", "api_key", "apiKey", "client_secret", "token", "access_token", "Authorization", "cookie", "set-cookie", "private_key", "totp_secret", "bearer", "credentials"}
	for _, k := range keys {
		_, raw := logLine(t, func(l *slog.Logger) { l.Info("x", k, seed) })
		if strings.Contains(raw, seed) {
			t.Errorf("key %q leaked its value: %s", k, raw)
		}
	}
	_, raw := logLine(t, func(l *slog.Logger) {
		l.With("db_password", seed).WithGroup("req").Info("x", slog.Group("hdr", "authorization", seed))
	})
	if strings.Contains(raw, seed) {
		t.Errorf("With/group leaked: %s", raw)
	}
}

func TestRedactsSecretShapedValues(t *testing.T) {
	const seed = "TOPSECRET123"
	vals := []string{
		"Authorization: Bearer " + seed,
		"connect postgres://app:" + seed + "@db:5432/x failed",
		"password=" + seed + " rejected",
		"token: " + seed,
	}
	for _, v := range vals {
		_, raw := logLine(t, func(l *slog.Logger) { l.Info(v, "detail", v, "err", errors.New(v)) })
		if strings.Contains(raw, seed) {
			t.Errorf("value %q leaked: %s", v, raw)
		}
	}
}

func TestLeavesOrdinaryAttrsAlone(t *testing.T) {
	m, _ := logLine(t, func(l *slog.Logger) {
		l.Info("request", "route", "/v1/leads/{id}", "status", 200, "tokens_per_page", 5)
	})
	if m["route"] != "/v1/leads/{id}" || m["status"] != float64(200) {
		t.Fatalf("ordinary attrs altered: %v", m)
	}
}

func TestLogInjectionEscaped(t *testing.T) {
	_, raw := logLine(t, func(l *slog.Logger) {
		l.Info("a\n{\"level\":\"ERROR\",\"msg\":\"forged\"}", "path", "/x\r\nforged line")
	})
	if strings.Count(strings.TrimRight(raw, "\n"), "\n") != 0 {
		t.Fatalf("log line was split: %q", raw)
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("a\r\nb\x00c\x1b[31m", 100); strings.ContainsAny(got, "\r\n\x00\x1b") {
		t.Fatalf("control characters survived: %q", got)
	}
	if got := Sanitize(strings.Repeat("x", 500), 10); len(got) > 14 {
		t.Fatalf("not truncated: %d", len(got))
	}
}

func TestLevelFromString(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, Options{Level: "warn"})
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(b.String(), "hidden") || !strings.Contains(b.String(), "shown") {
		t.Fatalf("level not applied: %s", b.String())
	}
}

func BenchmarkLogRequest(b *testing.B) {
	l := New(discard{}, Options{Service: "can-test"})
	ctx := WithRequestID(context.Background(), "rid")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "request", "method", "GET", "route", "/v1/leads/{id}", "status", 200, "duration_ms", 3)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

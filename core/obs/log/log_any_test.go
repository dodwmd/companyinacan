package obslog

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type creds struct {
	User     string
	Password string
	Note     string
}

func TestRedactsSecretsInsideStructuredValues(t *testing.T) {
	const seed = "SEEDVALUE12345"
	h := http.Header{}
	h.Set("Authorization", "Bearer "+seed)
	h.Set("Cookie", "sid="+seed)
	cases := map[string]any{
		"map":         map[string]string{"password": seed},
		"nested map":  map[string]any{"cfg": map[string]string{"api_key": seed}},
		"struct":      creds{User: "u", Password: seed},
		"struct ptr":  &creds{Password: seed},
		"header":      h,
		"bytes":       []byte("token=" + seed),
		"slice":       []string{"ok", "secret=" + seed},
		"stringer":    stringer("password=" + seed),
		"error wrap":  wrapped{"dial postgres://app:" + seed + "@db/x"},
		"url pointer": strPtr("https://u:" + seed + "@host/x"),
	}
	for name, v := range cases {
		_, raw := logLine(t, func(l *slog.Logger) {
			l.Info("x", "detail", v)
		})
		if strings.Contains(raw, seed) {
			t.Errorf("%s leaked: %s", name, raw)
		}
	}
}

func TestRedactsSecretsInMessages(t *testing.T) {
	const seed = "SEEDVALUE12345"
	for _, msg := range []string{"login with password=" + seed, "Authorization: Bearer " + seed, "cookie: " + seed, "dial postgres://u:" + seed + "@h/db"} {
		_, raw := logLine(t, func(l *slog.Logger) {
			l.Info(msg)
		})
		if strings.Contains(raw, seed) {
			t.Errorf("message %q leaked: %s", msg, raw)
		}
	}
}

func TestPlainStructuredValuesStayStructured(t *testing.T) {
	m, _ := logLine(t, func(l *slog.Logger) {
		l.Info("x", "ids", []int{1, 2, 3})
	})
	if _, ok := m["ids"].([]any); !ok {
		t.Fatalf("a harmless slice should stay a JSON array: %v", m["ids"])
	}
}

type stringer string

func (s stringer) String() string { return string(s) }

type wrapped struct{ msg string }

func (w wrapped) Error() string { return w.msg }

func strPtr(s string) *string { return &s }

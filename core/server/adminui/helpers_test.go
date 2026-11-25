package adminui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

func auditEntry(target string) audit.Entry {
	return audit.Entry{Actor: "root", Action: "test.event", Target: target}
}

func auditFilter(prefix string) audit.Filter { return audit.Filter{ActionPrefix: prefix, Limit: 50} }

func authDecode(s string) ([]byte, error) { return auth.DecodeTOTPSecret(s) }

func authCode(key []byte, now time.Time) string { return auth.Code(key, auth.SHA1, 6, 30, now) }

func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAll(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	return string(b)
}

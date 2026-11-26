package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// In restore mode the listeners answer only the probes and the restore hooks' dump download.
func TestRestoringHandlers(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	code := func(h http.Handler, method, path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}
	admin, module := restoringHandler(ok, adminAllowed), restoringHandler(ok, moduleAllowed)
	for _, c := range []struct {
		h            http.Handler
		method, path string
		want         int
	}{
		{admin, "GET", "/healthz", 200}, {admin, "GET", "/readyz", 200}, {admin, "GET", "/", 503},
		{admin, "POST", "/api/v1/login", 503}, {admin, "GET", "/v1/dumps/a/b", 503},
		{module, "GET", "/v1/dumps/wiki/x", 200}, {module, "PUT", "/v1/dumps/wiki/x", 503},
		{module, "POST", "/v1/email", 503}, {module, "POST", "/v1/events", 503}, {module, "GET", "/healthz", 200},
	} {
		if got := code(c.h, c.method, c.path); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

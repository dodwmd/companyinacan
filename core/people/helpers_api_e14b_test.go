package people

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func newTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T, api *API) *httptest.Server {
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

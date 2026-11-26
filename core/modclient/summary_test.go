package modclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeIntrospect struct {
	id    Identity
	err   error
	calls int
}

func (f *fakeIntrospect) Introspect(context.Context, string) (Identity, error) {
	f.calls++
	return f.id, f.err
}

func TestSummaryHandler(t *testing.T) {
	grant := SummaryTokenPrefix + strings.Repeat("a", 43)
	good := Identity{Active: true, Kind: SummaryKind, Subject: "user:alice", Permissions: []string{"work.read"}}
	var built int
	build := func(_ context.Context, subject string, perms []string) ([]SummaryFigure, error) {
		built++
		if subject != "user:alice" || len(perms) != 1 {
			t.Errorf("build(%q, %v)", subject, perms)
		}
		return nil, nil
	}
	for name, c := range map[string]struct {
		method, auth string
		id           Identity
		err          error
		status       int
		calls        int
	}{
		"ok":                    {auth: "Bearer " + grant, id: good, status: 200, calls: 1},
		"no credential":         {status: 401},
		"personal token":        {auth: "Bearer canpat_" + strings.Repeat("a", 43), id: good, status: 401},
		"malformed grant":       {auth: "Bearer cansum_short", id: good, status: 401},
		"inactive":              {auth: "Bearer " + grant, status: 401, calls: 1},
		"a user token's answer": {auth: "Bearer " + grant, id: Identity{Active: true, Kind: "user", Subject: "user:alice"}, status: 401, calls: 1},
		"a module's identity":   {auth: "Bearer " + grant, id: Identity{Active: true, Kind: "module"}, status: 401, calls: 1},
		"bad subject":           {auth: "Bearer " + grant, id: Identity{Active: true, Kind: SummaryKind, Subject: "service"}, status: 401, calls: 1},
		"can-core down":         {auth: "Bearer " + grant, err: errors.New("down"), status: 503, calls: 1},
		"post":                  {method: "POST", auth: "Bearer " + grant, id: good, status: 405},
	} {
		built = 0
		api := &fakeIntrospect{id: c.id, err: c.err}
		h := SummaryHandler(api, slog.New(slog.DiscardHandler), build)
		method := c.method
		if method == "" {
			method = http.MethodGet
		}
		r := httptest.NewRequest(method, SummaryPath, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status || api.calls != c.calls {
			t.Errorf("%s: status %d calls %d, want %d %d", name, w.Code, api.calls, c.status, c.calls)
		}
		if (w.Code == 200) != (built == 1) {
			t.Errorf("%s: built %d times", name, built)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: cacheable", name)
		}
		if w.Code == 200 && strings.TrimSpace(w.Body.String()) != `{"version":1,"figures":[]}` {
			t.Errorf("%s: %s", name, w.Body)
		}
	}
}

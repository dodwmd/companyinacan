package approvals

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/registry"
)

type fakeResolver struct {
	url string
	err error
}

func (f fakeResolver) Resolve(context.Context, string) (registry.Resolution, error) {
	if f.err != nil {
		return registry.Resolution{}, f.err
	}
	return registry.Resolution{Providers: []registry.Provider{{URL: f.url}}}, nil
}

func TestDirectoryReportingLine(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/people/p1":
			_, _ = w.Write([]byte(`{"id":"p1","manager_id":"m1"}`))
		case "/v1/people/p2":
			_, _ = w.Write([]byte(`{"id":"p2","manager_id":""}`))
		case "/v1/people/boom":
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	d := &DirectoryReportingLine{Resolver: fakeResolver{url: srv.URL}, Token: func() (string, error) { return "tok", nil }}
	ctx := context.Background()
	if m, err := d.ManagerOf(ctx, "party:p1"); err != nil || m != "party:m1" || gotAuth != "Bearer tok" {
		t.Fatalf("manager = %q, %v, auth %q", m, err, gotAuth)
	}
	for _, s := range []string{"party:p2", "party:unknown", "user:alice"} {
		if _, err := d.ManagerOf(ctx, s); !errors.Is(err, ErrNoManager) {
			t.Errorf("%s: %v, want ErrNoManager", s, err)
		}
	}
	if _, err := d.ManagerOf(ctx, "party:boom"); err == nil || errors.Is(err, ErrNoManager) {
		t.Fatalf("a directory outage must not look like 'no manager': %v", err)
	}
	if _, err := (&DirectoryReportingLine{Resolver: fakeResolver{err: registry.ErrNoProvider}}).ManagerOf(ctx, "party:p1"); !errors.Is(err, ErrNoManager) {
		t.Fatalf("no directory installed: %v", err)
	}
	if _, err := (&DirectoryReportingLine{Resolver: fakeResolver{url: srv.URL}, Token: func() (string, error) { return "", nil }}).ManagerOf(ctx, "party:p1"); !errors.Is(err, ErrNoManager) {
		t.Fatal("no credential configured")
	}
}

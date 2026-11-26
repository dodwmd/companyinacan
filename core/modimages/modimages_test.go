package modimages

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modules = "../../modules"

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"can-core:dev":                    "docker.io/library/can-core:dev",
		"ghcr.io/dodwmd/can-work:0.1.0":   "ghcr.io/dodwmd/can-work:0.1.0",
		"arp242/goatcounter":              "docker.io/arp242/goatcounter:latest",
		"docker.io/a/b:1@sha256:ab":       "docker.io/a/b@sha256:ab",
		"localhost:5000/x:2":              "localhost:5000/x:2",
		"docker.io/library/espocrm:9-apc": "docker.io/library/espocrm:9-apc",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestForModule(t *testing.T) {
	got, err := ForModule(modules, "work")
	if err != nil || !slices.Equal(got, []string{Ref("can-work")}) {
		t.Fatalf("work: %v %v", got, err)
	}
	// a script hook runs in can-hooks; the adapter is not built and is switched off
	got, err = ForModule(modules, "wiki")
	if err != nil || len(got) != 2 || !slices.Contains(got, Ref("can-hooks")) || strings.Contains(strings.Join(got, " "), "adapter") {
		t.Fatalf("wiki: %v %v", got, err)
	}
	if got, err = ForModule(modules, "example"); err != nil || len(got) != 0 {
		t.Fatalf("a module without a chart needs nothing: %v %v", got, err)
	}
}

// Every shipped module can be read: a module the preflight cannot read is not checked.
func TestRequiredReadsEveryModule(t *testing.T) {
	dirs, err := filepath.Glob(modules + "/[a-z]*/module.yaml")
	if err != nil || len(dirs) < 10 {
		t.Fatal(dirs, err)
	}
	var ids []string
	for _, d := range dirs {
		ids = append(ids, filepath.Base(filepath.Dir(d)))
	}
	refs, unknown := Required(modules, ids)
	if len(unknown) != 0 {
		t.Fatal(unknown)
	}
	if !slices.Equal(refs[Ref("can-forms")], []string{"forms"}) {
		t.Fatalf("forms: %v", refs)
	}
}

func TestPullable(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") == "repository:o/private:pull" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t"}`))
		case r.Header.Get("Authorization") != "Bearer t":
			repo := strings.TrimPrefix(strings.Split(r.URL.Path, "/manifests/")[0], "/v2/")
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="s",scope="repository:%s:pull"`, srv.URL, repo))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/v2/o/there/manifests/1":
		case r.URL.Path == "/v2/o/broken/manifests/1":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	c := Checker{Client: srv.Client()}
	for repo, want := range map[string]struct {
		ok, err bool
	}{"o/there:1": {ok: true}, "o/there:2": {}, "o/private:1": {}, "o/broken:1": {err: true}} {
		ok, err := c.Pullable(context.Background(), host+"/"+repo)
		if ok != want.ok || (err != nil) != want.err {
			t.Errorf("%s: %v %v", repo, ok, err)
		}
	}
}

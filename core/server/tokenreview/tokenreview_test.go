package tokenreview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseUsername(t *testing.T) {
	id, err := ParseUsername("system:serviceaccount:mod-crm:crm")
	if err != nil || id.Namespace != "mod-crm" || id.ServiceAccount != "crm" {
		t.Fatalf("%+v %v", id, err)
	}
	for _, bad := range []string{"", "admin", "system:node:n1", "system:serviceaccount::x", "system:serviceaccount:a:b:c"} {
		if _, err := ParseUsername(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestFake(t *testing.T) {
	f := NewFake()
	f.Add("tok", "mod-crm", "crm")
	if id, err := f.Review(context.Background(), "tok", Audience); err != nil || id.Namespace != "mod-crm" {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := f.Review(context.Background(), "nope", Audience); !errors.Is(err, ErrInvalidToken) {
		t.Fatal(err)
	}
	if _, err := f.Review(context.Background(), "tok", "other"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("wrong audience accepted")
	}
}

func apiServer(t *testing.T, status map[string]any, code int) (*HTTP, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer core-token" {
			http.Error(w, "bad request", 400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status})
	}))
	t.Cleanup(srv.Close)
	return &HTTP{APIServer: srv.URL, BearerToken: func() (string, error) { return "core-token", nil }}, &got
}

func TestHTTPReviewer(t *testing.T) {
	h, got := apiServer(t, map[string]any{"authenticated": true, "audiences": []string{"can"}, "user": map[string]any{"username": "system:serviceaccount:mod-crm:crm"}}, 200)
	id, err := h.Review(context.Background(), "projected", Audience)
	if err != nil || id != (Identity{"mod-crm", "crm"}) {
		t.Fatalf("%+v %v", id, err)
	}
	spec := (*got)["spec"].(map[string]any)
	if spec["token"] != "projected" || spec["audiences"].([]any)[0] != "can" {
		t.Fatalf("request spec %+v", spec)
	}
}

func TestHTTPReviewerRejections(t *testing.T) {
	cases := map[string]struct {
		status map[string]any
		code   int
	}{
		"unauthenticated": {map[string]any{"authenticated": false}, 200},
		"wrong audience":  {map[string]any{"authenticated": true, "audiences": []string{"https://kubernetes.default.svc"}, "user": map[string]any{"username": "system:serviceaccount:a:b"}}, 200},
		"not a SA":        {map[string]any{"authenticated": true, "audiences": []string{"can"}, "user": map[string]any{"username": "alice"}}, 200},
		"api error":       {map[string]any{}, 500},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := apiServer(t, c.status, c.code)
			if _, err := h.Review(context.Background(), "t", Audience); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

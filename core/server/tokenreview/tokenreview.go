// Package tokenreview abstracts Kubernetes TokenReview: it turns a projected ServiceAccount token
// into a (namespace, service account) identity. Everything in can-core depends on the Reviewer
// interface, so tests and local runs need no cluster.
package tokenreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Audience is the audience of the projected token (SPEC §3.1).
const Audience = "can"

// ErrInvalidToken is returned when the token is not authenticated for the audience.
var ErrInvalidToken = errors.New("token not authenticated")

// Identity is who a token belongs to.
type Identity struct {
	Namespace      string
	ServiceAccount string
}

// Reviewer validates a token for an audience.
type Reviewer interface {
	Review(ctx context.Context, token, audience string) (Identity, error)
}

// ParseUsername parses "system:serviceaccount:<ns>:<name>".
func ParseUsername(u string) (Identity, error) {
	parts := strings.Split(u, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return Identity{}, fmt.Errorf("%w: %q is not a service account", ErrInvalidToken, u)
	}
	return Identity{Namespace: parts[2], ServiceAccount: parts[3]}, nil
}

// Fake is an in-memory Reviewer for tests and local development.
type Fake struct {
	mu     sync.Mutex
	tokens map[string]Identity
	Calls  int
}

// NewFake returns an empty fake.
func NewFake() *Fake { return &Fake{tokens: map[string]Identity{}} }

// Add registers a token.
func (f *Fake) Add(token, namespace, serviceAccount string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = Identity{Namespace: namespace, ServiceAccount: serviceAccount}
}

// Review implements Reviewer.
func (f *Fake) Review(_ context.Context, token, audience string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if audience != Audience {
		return Identity{}, ErrInvalidToken
	}
	id, ok := f.tokens[token]
	if !ok {
		return Identity{}, ErrInvalidToken
	}
	return id, nil
}

// HTTP calls the Kubernetes API's TokenReview endpoint with plain net/http (no client-go, which
// would add ~20 MB of binary and RAM; research R6). It is exercised against an httptest server in
// unit tests and against a real API server only in the k3d integration tests (follow-up).
type HTTP struct {
	APIServer string       // e.g. https://kubernetes.default.svc
	Client    *http.Client // trusts the cluster CA
	// BearerToken returns can-core's own ServiceAccount token (re-read per call: it rotates).
	BearerToken func() (string, error)
}

// Review implements Reviewer.
func (h *HTTP) Review(ctx context.Context, token, audience string) (Identity, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": token, "audiences": []string{audience}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.APIServer, "/")+"/apis/authentication.k8s.io/v1/tokenreviews", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	own, err := h.BearerToken()
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+own)
	req.Header.Set("Content-Type", "application/json")
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Identity{}, fmt.Errorf("tokenreview: API server returned %d", resp.StatusCode)
	}
	var out struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Identity{}, fmt.Errorf("tokenreview: bad response: %w", err)
	}
	if !out.Status.Authenticated {
		return Identity{}, ErrInvalidToken
	}
	found := false
	for _, a := range out.Status.Audiences {
		found = found || a == audience
	}
	if !found {
		return Identity{}, fmt.Errorf("%w: token not issued for audience %q", ErrInvalidToken, audience)
	}
	return ParseUsername(out.Status.User.Username)
}

// FileToken returns a BearerToken func reading path (the in-cluster default is
// /var/run/secrets/kubernetes.io/serviceaccount/token).
func FileToken(path string) func() (string, error) {
	return func() (string, error) {
		b, err := os.ReadFile(path) //nolint:gosec // fixed in-cluster path from configuration
		return strings.TrimSpace(string(b)), err
	}
}

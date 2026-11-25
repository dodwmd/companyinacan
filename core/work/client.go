package work

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a small client for the work API, used by `can work` and by automation written
// in Go. It sends an API token and never follows redirects (a redirect would forward the
// Authorization header elsewhere).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Retries is how many times a request that is safe to repeat is retried after a network
	// error or 5xx. Creates are only retried when they carry an idempotency key.
	Retries int
	Sleep   func(time.Duration)
}

// APIError is an error response from the server.
type APIError struct {
	Status  int
	Code    string
	Message string
	Fields  []ProblemField
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s: %s", e.Status, e.Code, e.Message)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "\n  %s: %s", f.Field, f.Message)
	}
	return b.String()
}

// NewClient builds a client for baseURL (for example http://work.example.com).
func NewClient(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("work URL %q must be an http(s) URL", baseURL)
	}
	if token == "" {
		return nil, errors.New("no API token (set CAN_WORK_TOKEN)")
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Token: token, Retries: 2, Sleep: time.Sleep,
		HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// NewIdempotencyKey returns a fresh random key for a create.
func NewIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "cli-" + hex.EncodeToString(b[:])
}

// Do sends a request and decodes a JSON response into out (when non-nil). header pairs are
// extra request headers. It returns the response headers.
func (c *Client) Do(ctx context.Context, method, path string, body, out any, header ...string) (http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	retriable := method == http.MethodGet || method == http.MethodDelete || method == http.MethodPut
	for i := 0; i+1 < len(header); i += 2 {
		if strings.EqualFold(header[i], "Idempotency-Key") || strings.EqualFold(header[i], "If-Match") {
			retriable = true
		}
	}
	var last error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			c.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		h, err, again := c.once(ctx, method, path, payload, out, header)
		if err == nil {
			return h, nil
		}
		last = err
		if !again || !retriable {
			break
		}
	}
	return nil, last
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any, header []string) (http.Header, error, bool) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err, false
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err, true
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err, true
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode, Code: "error", Message: strings.TrimSpace(string(raw))}
		var e apiError
		if json.Unmarshal(raw, &e) == nil && e.Code != "" {
			ae.Code, ae.Message, ae.Fields = e.Code, e.Message, e.Errors
		}
		return nil, ae, resp.StatusCode >= 500
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err), false
		}
	}
	return resp.Header, nil, false
}

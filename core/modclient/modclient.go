// Package modclient is what an in-house module uses to talk to can-core (specs/016-module-wiring):
// the module API client (token introspection, event publishing, approvals), a bounded cache for
// introspection answers, and the receiver that verifies can-core's signed webhooks.
//
// Everything fails closed: an unreachable or confused can-core never yields an "active" answer, and
// nothing here logs a token or a secret.
package modclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable means can-core could not give an answer (network error, 5xx, 429, or it refused
// this module's own credential). Callers must treat it as "no", and may retry later.
var ErrUnavailable = errors.New("can-core is unavailable")

// StatusError is an HTTP refusal or failure of a platform service. 401, 429 and 5xx also match
// ErrUnavailable.
type StatusError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.Status)
	if e.Detail != "" {
		msg += " " + e.Detail
	}
	if e.Unwrap() != nil {
		return ErrUnavailable.Error() + ": " + msg
	}
	return msg
}

// Unwrap lets errors.Is(err, ErrUnavailable) mean "try again later".
func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusTooManyRequests || e.Status >= 500 {
		return ErrUnavailable
	}
	return nil
}

// maxRetryAfter bounds what a server can ask a caller to wait.
const maxRetryAfter = 6 * time.Hour

// NewStatusError builds a StatusError from a response's status and headers (Retry-After in seconds).
func NewStatusError(status int, h http.Header, detail string) *StatusError {
	e := &StatusError{Status: status, Detail: detail}
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n > 0 {
		e.RetryAfter = min(time.Duration(min(n, int(maxRetryAfter/time.Second)))*time.Second, maxRetryAfter)
	}
	return e
}

// RetryAfter returns how long the service asked to be left alone (0: it did not say).
func RetryAfter(err error) time.Duration {
	var e *StatusError
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

const maxResponse = 64 << 10

// Client talks to the can-core module API with the module's projected ServiceAccount token.
type Client struct {
	BaseURL   string
	TokenFile string // projected token, re-read on every call (it rotates)
	ModuleID  string
	HTTP      *http.Client
}

// FromEnv builds a client from CAN_API_URL, CAN_TOKEN_FILE and CAN_MODULE_ID, the variables can-core
// injects. With CAN_API_URL unset it returns nil, nil: the module runs standalone.
func FromEnv(getenv func(string) string) (*Client, error) {
	base := strings.TrimRight(getenv("CAN_API_URL"), "/")
	if base == "" {
		return nil, nil
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("CAN_API_URL must be an http(s) URL without credentials, query or fragment")
	}
	tok := getenv("CAN_TOKEN_FILE")
	if tok == "" {
		return nil, errors.New("CAN_API_URL is set but CAN_TOKEN_FILE is not: refusing to talk to can-core anonymously")
	}
	return &Client{BaseURL: base, TokenFile: tok, ModuleID: getenv("CAN_MODULE_ID")}, nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *Client) do(ctx context.Context, method, path string, body any, header map[string]string) (int, []byte, error) {
	tok, err := os.ReadFile(c.TokenFile) //nolint:gosec // fixed in-cluster path from configuration
	if err != nil || len(bytes.TrimSpace(tok)) == 0 {
		return 0, nil, fmt.Errorf("%w: the projected token is unreadable", ErrUnavailable)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return resp.StatusCode, b, NewStatusError(resp.StatusCode, resp.Header, "")
	}
	return resp.StatusCode, b, nil
}

// Identity is the answer of can-core's token introspection.
type Identity struct {
	Active      bool      `json:"active"`
	Kind        string    `json:"kind"`
	Subject     string    `json:"subject"`
	Permissions []string  `json:"permissions"`
	ModuleID    string    `json:"module_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Introspect asks can-core who a token belongs to, naming this module as the audience.
func (c *Client) Introspect(ctx context.Context, token string) (Identity, error) {
	status, b, err := c.do(ctx, http.MethodPost, "/v1/tokens/introspect", map[string]string{"token": token, "audience": c.ModuleID}, nil)
	if err != nil {
		return Identity{}, err
	}
	if status != http.StatusOK {
		return Identity{}, fmt.Errorf("introspection answered HTTP %d", status)
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, errors.New("introspection answer is not valid JSON")
	}
	if !id.Active {
		return Identity{}, nil
	}
	return id, nil
}

// PublishEnvelope publishes a module's outbox envelope ({"id","type","data",...}) through can-core.
// The envelope id becomes the publish's source_id so a retry after a lost acknowledgement does not
// deliver the event twice; the source of the event is the authenticated module, never the body.
func (c *Client) PublishEnvelope(ctx context.Context, envelope []byte) error {
	var env struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil || env.Type == "" {
		return errors.New("malformed event envelope")
	}
	status, b, err := c.do(ctx, http.MethodPost, "/v1/events", map[string]any{"type": env.Type, "data": env.Data, "source_id": env.ID}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("can-core refused the event: HTTP %d %s", status, problemDetail(b))
	}
	return nil
}

// Email is a request to the platform's email.send capability. The sender address is can-core's.
type Email struct {
	To      []string `json:"to"`
	ReplyTo string   `json:"reply_to,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	// SourceID makes the request idempotent: repeating it queues nothing new.
	SourceID string `json:"source_id,omitempty"`
}

// SendEmail queues mail through can-core. ErrUnavailable (retry later) covers network errors, 5xx
// and quota refusals; a refusal is a *StatusError.
func (c *Client) SendEmail(ctx context.Context, e Email) error {
	status, b, err := c.do(ctx, http.MethodPost, "/v1/email", e, nil)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return fmt.Errorf("can-core refused the email: %w", &StatusError{Status: status, Detail: problemDetail(b)})
	}
	return nil
}

func problemDetail(b []byte) string {
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(b, &p)
	if len(p.Detail) > 200 {
		p.Detail = p.Detail[:200]
	}
	return p.Detail
}

// ApprovalInput opens an approval request in can-core's engine.
type ApprovalInput struct {
	Type      string
	Requester string // user:<name> or party:<id>
	Ref       string // the module's own reference to the thing being approved
	Title     string
	Amount    int64
	Payload   json.RawMessage
	Key       string // Idempotency-Key: required, so a retry never opens a second request
}

// Approval is the module's view of a request.
type Approval struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// ErrApprovalRefused is a permanent refusal (no policy, no approver, invalid input).
var ErrApprovalRefused = errors.New("can-core refused the approval request")

// Approvals is the can-core approvals engine as modules use it; tests substitute a fake.
type Approvals interface {
	CreateApproval(ctx context.Context, in ApprovalInput) (Approval, error)
	GetApproval(ctx context.Context, id string) (Approval, error)
	CancelApproval(ctx context.Context, id, reason string) error
}

// CreateApproval opens a request (POST /v1/approvals).
func (c *Client) CreateApproval(ctx context.Context, in ApprovalInput) (Approval, error) {
	if in.Key == "" {
		return Approval{}, errors.New("an approval request needs an idempotency key")
	}
	body := map[string]any{"type": in.Type, "requester": in.Requester, "ref": in.Ref, "title": in.Title, "amount": in.Amount}
	if len(in.Payload) > 0 {
		body["payload"] = in.Payload
	}
	status, b, err := c.do(ctx, http.MethodPost, "/v1/approvals", body, map[string]string{"Idempotency-Key": in.Key})
	if err != nil {
		return Approval{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return Approval{}, fmt.Errorf("%w: HTTP %d %s", ErrApprovalRefused, status, problemDetail(b))
	}
	var a Approval
	if err := json.Unmarshal(b, &a); err != nil || a.ID == "" {
		return Approval{}, errors.New("approval answer is not valid")
	}
	return a, nil
}

// GetApproval reads one of this module's requests.
func (c *Client) GetApproval(ctx context.Context, id string) (Approval, error) {
	status, b, err := c.do(ctx, http.MethodGet, "/v1/approvals/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return Approval{}, err
	}
	if status != http.StatusOK {
		return Approval{}, fmt.Errorf("%w: HTTP %d", ErrApprovalRefused, status)
	}
	var a Approval
	if err := json.Unmarshal(b, &a); err != nil {
		return Approval{}, errors.New("approval answer is not valid")
	}
	return a, nil
}

// CancelApproval withdraws a pending request.
func (c *Client) CancelApproval(ctx context.Context, id, reason string) error {
	status, _, err := c.do(ctx, http.MethodPost, "/v1/approvals/"+url.PathEscape(id)+"/cancel", map[string]string{"reason": reason}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusConflict {
		return fmt.Errorf("%w: HTTP %d", ErrApprovalRefused, status)
	}
	return nil
}

// ErrRevokeRefused means can-core will not revoke this subject now: the person is not recorded as
// offboarded yet, the subject is not their linked account, or this module may not ask.
var ErrRevokeRefused = errors.New("can-core refused the revocation")

// RevokeResult counts what a revocation ended.
type RevokeResult struct {
	Tokens   int `json:"tokens"`
	Sessions int `json:"sessions"`
}

// RevokeIdentity asks can-core to end the personal access tokens and sessions of an offboarded
// person (POST /v1/identity/revoke). Idempotent: repeating it ends nothing more and still succeeds.
// can-core checks the offboarding itself; this call cannot revoke an arbitrary subject.
func (c *Client) RevokeIdentity(ctx context.Context, partyID, subject string) (RevokeResult, error) {
	status, b, err := c.do(ctx, http.MethodPost, "/v1/identity/revoke", map[string]string{"party_id": partyID, "subject": subject}, nil)
	if err != nil {
		return RevokeResult{}, err
	}
	if status != http.StatusOK {
		return RevokeResult{}, fmt.Errorf("%w: HTTP %d %s", ErrRevokeRefused, status, problemDetail(b))
	}
	var r RevokeResult
	if err := json.Unmarshal(b, &r); err != nil {
		return RevokeResult{}, errors.New("revocation answer is not valid")
	}
	return r, nil
}

// ---- webhook receiver --------------------------------------------------------------------------

// Event is a can-core event as delivered to a subscriber.
type Event struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

// Header names of a can-core webhook delivery (core/server/webhook).
const (
	headerTimestamp = "X-Can-Timestamp"
	headerSignature = "X-Can-Signature"
)

// Receiver is the http.Handler of a module's event endpoint. It verifies can-core's HMAC signature
// and replay window before the body is parsed, drops redelivered event ids, and answers 5xx for a
// handler failure so can-core retries. The set of seen event ids is per process and bounded
// (4,096): after a restart, or past that many events, a redelivered event reaches the handler again,
// so handlers must be idempotent on their own (the hire hand-off keys on the candidate id, the
// approval handlers on the request id and a pending state).
type Receiver struct {
	// Secret is CAN_WEBHOOK_SECRET. Shorter than 16 characters (or empty) refuses everything.
	Secret string
	Handle func(ctx context.Context, e Event) error
	Now    func() time.Time
	Window time.Duration // default 5 minutes
	// MaxBody bounds the request body (default 1 MiB).
	MaxBody int64

	once sync.Once
	seen *idSet
}

// ServeHTTP implements http.Handler.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(rc.Secret) < 16 {
		http.Error(w, "event delivery is not configured", http.StatusServiceUnavailable)
		return
	}
	now := time.Now
	if rc.Now != nil {
		now = rc.Now
	}
	max := rc.MaxBody
	if max <= 0 {
		max = 1 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "event too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !rc.verify(r.Header, body, now()) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Type == "" {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}
	rc.once.Do(func() { rc.seen = newIDSet(4096) })
	if rc.seen.has(ev.ID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := rc.Handle(r.Context(), ev); err != nil {
		http.Error(w, "event not handled", http.StatusInternalServerError)
		return
	}
	rc.seen.add(ev.ID)
	w.WriteHeader(http.StatusNoContent)
}

// Sign returns the X-Can-Signature value can-core puts on a delivery of body at ts. Modules only
// verify; Sign is exported for tests and fakes.
func Sign(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (rc *Receiver) verify(h http.Header, body []byte, now time.Time) bool {
	ts, err := strconv.ParseInt(h.Get(headerTimestamp), 10, 64)
	sig := h.Get(headerSignature)
	if err != nil || !strings.HasPrefix(sig, "v1=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "v1="))
	if err != nil {
		return false
	}
	want, _ := hex.DecodeString(strings.TrimPrefix(Sign(rc.Secret, time.Unix(ts, 0), body), "v1="))
	if !hmac.Equal(got, want) {
		return false
	}
	win := rc.Window
	if win <= 0 {
		win = 5 * time.Minute
	}
	d := now.Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	return d <= win
}

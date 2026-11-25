package provider

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Hetzner identifies SSH keys by their MD5 fingerprint
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HetznerAPIBase is the Cloud API root.
const HetznerAPIBase = "https://api.hetzner.cloud/v1"

// HetznerClient is a small client for the parts of the Hetzner Cloud API that
// `can up` needs. The token is only ever placed in the Authorization header;
// every error and log line passes through the redactor.
type HetznerClient struct {
	Base   string
	token  string
	HTTP   *http.Client
	Redact *Redactor
	Sleep  func(context.Context, time.Duration)
}

// NewHetznerClient returns a client. The token is registered with redact.
func NewHetznerClient(base, token string, h *http.Client, redact *Redactor, sleep func(context.Context, time.Duration)) *HetznerClient {
	redact.Add(token)
	if base == "" {
		base = HetznerAPIBase
	}
	return &HetznerClient{Base: strings.TrimRight(base, "/"), token: token, HTTP: h, Redact: redact, Sleep: sleep}
}

// APIError is an error response from the API.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Service names the API in messages ("hetzner" when empty).
	Service string
}

func (e *APIError) Error() string {
	svc := e.Service
	if svc == "" {
		svc = "hetzner"
	}
	return fmt.Sprintf("%s API: %s (%s, HTTP %d)", svc, e.Message, e.Code, e.Status)
}

// IsNotFound reports a 404 from the API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func apiCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

const maxAttempts = 4

func (c *HetznerClient) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = b
	}
	var lastErr error
	var wait time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if wait <= 0 {
				wait = time.Duration(1<<attempt) * 500 * time.Millisecond
			}
			c.Sleep(ctx, wait)
			wait = 0
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(payload))
		if err != nil {
			return c.Redact.Err(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "can-cli")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = c.Redact.Err(err)
			if method == http.MethodGet {
				continue
			}
			return lastErr
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(body) > 0 {
				if err := json.Unmarshal(body, out); err != nil {
					return fmt.Errorf("hetzner API: unreadable response: %w", err)
				}
			}
			return nil
		}
		lastErr = c.apiError(resp.StatusCode, body)
		wait = resetWait(resp.Header.Get("RateLimit-Reset"), time.Now())
		retry := resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && method == http.MethodGet)
		if !retry {
			return lastErr
		}
	}
	return lastErr
}

func (c *HetznerClient) apiError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: e.Error.Code, Message: c.Redact.Redact(msg)}
}

// Server is the subset of the API's server object we use.
type HServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// HSSHKey is an uploaded SSH key.
type HSSHKey struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
}

// HFirewall is a cloud firewall.
type HFirewall struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// OwnedBy reports whether labels mark a resource as created by can for the
// company slug. can never adopts or deletes a resource without them.
func OwnedBy(labels map[string]string, companySlug string) bool {
	return labels["managed-by"] == "can" && labels["company"] == companySlug
}

// GetFirewall fetches a firewall by id (IsNotFound when gone).
func (c *HetznerClient) GetFirewall(ctx context.Context, id int64) (HFirewall, error) {
	var r struct {
		Firewall HFirewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodGet, "/firewalls/"+strconv.FormatInt(id, 10), nil, &r)
	return r.Firewall, err
}

// FirewallRule is one inbound rule.
type FirewallRule struct {
	Direction string   `json:"direction"`
	Protocol  string   `json:"protocol"`
	Port      string   `json:"port,omitempty"`
	SourceIPs []string `json:"source_ips"`
}

// CreateServerRequest is the body of POST /servers.
type CreateServerRequest struct {
	Name       string            `json:"name"`
	ServerType string            `json:"server_type"`
	Image      string            `json:"image"`
	Location   string            `json:"location,omitempty"`
	SSHKeys    []int64           `json:"ssh_keys"`
	Firewalls  []FirewallRef     `json:"firewalls,omitempty"`
	UserData   string            `json:"user_data,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	StartAfter bool              `json:"start_after_create"`
}

// FirewallRef attaches a firewall at creation.
type FirewallRef struct {
	Firewall int64 `json:"firewall"`
}

// FindSSHKey looks a key up by fingerprint; ok is false when absent.
func (c *HetznerClient) FindSSHKey(ctx context.Context, fingerprint string) (HSSHKey, bool, error) {
	var r struct {
		Keys []HSSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?fingerprint="+url.QueryEscape(fingerprint), nil, &r); err != nil {
		return HSSHKey{}, false, err
	}
	if len(r.Keys) == 0 {
		return HSSHKey{}, false, nil
	}
	return r.Keys[0], true, nil
}

// UploadSSHKey registers a public key.
func (c *HetznerClient) UploadSSHKey(ctx context.Context, name, publicKey string, labels map[string]string) (HSSHKey, error) {
	var r struct {
		Key HSSHKey `json:"ssh_key"`
	}
	err := c.do(ctx, http.MethodPost, "/ssh_keys", map[string]any{"name": name, "public_key": publicKey, "labels": labels}, &r)
	return r.Key, err
}

// DeleteSSHKey removes a key; a missing key is not an error.
func (c *HetznerClient) DeleteSSHKey(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/ssh_keys/"+strconv.FormatInt(id, 10), nil, nil))
}

// FindFirewall looks a firewall up by name.
func (c *HetznerClient) FindFirewall(ctx context.Context, name string) (HFirewall, bool, error) {
	var r struct {
		Firewalls []HFirewall `json:"firewalls"`
	}
	if err := c.do(ctx, http.MethodGet, "/firewalls?name="+url.QueryEscape(name), nil, &r); err != nil {
		return HFirewall{}, false, err
	}
	if len(r.Firewalls) == 0 {
		return HFirewall{}, false, nil
	}
	return r.Firewalls[0], true, nil
}

// CreateFirewall creates a firewall with the given inbound rules.
func (c *HetznerClient) CreateFirewall(ctx context.Context, name string, rules []FirewallRule, labels map[string]string) (HFirewall, error) {
	var r struct {
		Firewall HFirewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodPost, "/firewalls", map[string]any{"name": name, "rules": rules, "labels": labels}, &r)
	return r.Firewall, err
}

// DeleteFirewall removes a firewall, retrying while it is still attached to a
// server that is being deleted.
func (c *HetznerClient) DeleteFirewall(ctx context.Context, id int64) error {
	var err error
	for i := 0; i < 20; i++ {
		err = c.do(ctx, http.MethodDelete, "/firewalls/"+strconv.FormatInt(id, 10), nil, nil)
		if err == nil || IsNotFound(err) {
			return nil
		}
		if apiCode(err) != "resource_in_use" {
			return err
		}
		c.Sleep(ctx, 2*time.Second)
	}
	return err
}

// FindServer looks a server up by name.
func (c *HetznerClient) FindServer(ctx context.Context, name string) (HServer, bool, error) {
	var r struct {
		Servers []HServer `json:"servers"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers?name="+url.QueryEscape(name), nil, &r); err != nil {
		return HServer{}, false, err
	}
	if len(r.Servers) == 0 {
		return HServer{}, false, nil
	}
	return r.Servers[0], true, nil
}

// GetServer fetches a server by id (IsNotFound when gone).
func (c *HetznerClient) GetServer(ctx context.Context, id int64) (HServer, error) {
	var r struct {
		Server HServer `json:"server"`
	}
	err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &r)
	return r.Server, err
}

// CreateServer creates a server (cloud-init in req.UserData).
func (c *HetznerClient) CreateServer(ctx context.Context, req CreateServerRequest) (HServer, error) {
	var r struct {
		Server HServer `json:"server"`
	}
	err := c.do(ctx, http.MethodPost, "/servers", req, &r)
	return r.Server, err
}

// DeleteServer deletes a server; a missing server is not an error.
func (c *HetznerClient) DeleteServer(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil))
}

func ignoreNotFound(err error) error {
	if IsNotFound(err) {
		return nil
	}
	return err
}

// SSHFingerprint returns the MD5 fingerprint (aa:bb:...) of an
// authorized_keys-format public key, the form Hetzner reports.
func SSHFingerprint(pub string) (string, error) {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return "", fmt.Errorf("not an ssh public key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", fmt.Errorf("not an ssh public key: %w", err)
	}
	sum := md5.Sum(blob) //nolint:gosec // fingerprint, not security
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":"), nil
}

// resetWait turns a RateLimit-Reset header (unix seconds) into a wait,
// capped at one minute; 0 means "use the default backoff".
func resetWait(h string, now time.Time) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
	if err != nil {
		return 0
	}
	d := time.Unix(secs, 0).Sub(now)
	if d <= 0 {
		return 0
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

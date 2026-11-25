package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DOAPIBase is the DigitalOcean API root.
const DOAPIBase = "https://api.digitalocean.com/v2"

// DOClient is a small client for the parts of the DigitalOcean API that
// `can up` needs. The token is only placed in the Authorization header; every
// error passes through the redactor.
//
// The request and response shapes follow the public API reference but have
// NOT been exercised against a live account (see specs/020-*/research.md).
type DOClient struct {
	Base   string
	token  string
	HTTP   *http.Client
	Redact *Redactor
	Sleep  func(context.Context, time.Duration)
}

// NewDOClient returns a client. The token is registered with redact.
func NewDOClient(base, token string, h *http.Client, redact *Redactor, sleep func(context.Context, time.Duration)) *DOClient {
	redact.Add(token)
	if base == "" {
		base = DOAPIBase
	}
	return &DOClient{Base: strings.TrimRight(base, "/"), token: token, HTTP: h, Redact: redact, Sleep: sleep}
}

// do performs one API call. Retries happen only where they cannot duplicate an
// effect: any method on 429 (the request was refused), and GET on 5xx or
// network errors. A POST or DELETE that fails with 5xx is returned as is.
func (c *DOClient) do(ctx context.Context, method, path string, in, out any) error {
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
			if method == http.MethodGet && ctx.Err() == nil {
				continue
			}
			return lastErr
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(body) > 0 {
				if err := json.Unmarshal(body, out); err != nil {
					return fmt.Errorf("digitalocean API: unreadable response: %w", err)
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

func (c *DOClient) apiError(status int, body []byte) error {
	var e struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: e.ID, Message: c.Redact.Redact(msg), Service: "digitalocean"}
}

// DODroplet is the subset of the droplet object we use.
type DODroplet struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Networks struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

// PublicIPv4 returns the droplet's public IPv4 address, or "".
func (d DODroplet) PublicIPv4() string {
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			return n.IP
		}
	}
	return ""
}

// DOKey is an uploaded SSH key.
type DOKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// DOFirewall is a cloud firewall. Tags select the droplets it applies to; the
// API has no separate labels, so can uses them as its ownership marker.
type DOFirewall struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// DOFirewallRule is one rule. Sources are used for inbound, Destinations for outbound.
type DOFirewallRule struct {
	Protocol     string       `json:"protocol"`
	Ports        string       `json:"ports,omitempty"`
	Sources      *DOAddresses `json:"sources,omitempty"`
	Destinations *DOAddresses `json:"destinations,omitempty"`
}

// DOAddresses lists CIDRs.
type DOAddresses struct {
	Addresses []string `json:"addresses"`
}

// DOCreateFirewall is the body of POST /firewalls.
type DOCreateFirewall struct {
	Name         string           `json:"name"`
	InboundRules []DOFirewallRule `json:"inbound_rules"`
	OutboundRule []DOFirewallRule `json:"outbound_rules"`
	Tags         []string         `json:"tags"`
}

// DOCreateDroplet is the body of POST /droplets. SSHKeys holds ids or fingerprints.
type DOCreateDroplet struct {
	Name     string   `json:"name"`
	Region   string   `json:"region"`
	Size     string   `json:"size"`
	Image    string   `json:"image"`
	SSHKeys  []any    `json:"ssh_keys"`
	UserData string   `json:"user_data,omitempty"`
	Tags     []string `json:"tags"`
	IPv6     bool     `json:"ipv6"`
}

// OwnedByTags reports whether droplet tags mark a droplet as created by can
// for the company slug.
func OwnedByTags(tags []string, companySlug string) bool {
	var a, b bool
	for _, t := range tags {
		a = a || t == "managed-by:can"
		b = b || t == "company:"+companySlug
	}
	return a && b
}

// pages reads every page of a list endpoint. fn decodes one page body and
// reports whether more pages follow (links.pages.next). The API's own next URL
// is not followed, so the token can only ever go to the configured base.
func (c *DOClient) pages(ctx context.Context, path string, fn func(body json.RawMessage) error) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= 500; page++ {
		var raw json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+sep+"per_page=200&page="+strconv.Itoa(page), nil, &raw); err != nil {
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
		var l struct {
			Links struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		_ = json.Unmarshal(raw, &l)
		if l.Links.Pages.Next == "" {
			return nil
		}
	}
	return fmt.Errorf("digitalocean API: more than 500 pages of %s", path)
}

// FindKey looks a key up by MD5 fingerprint; ok is false when absent.
func (c *DOClient) FindKey(ctx context.Context, fingerprint string) (DOKey, bool, error) {
	var r struct {
		Key DOKey `json:"ssh_key"`
	}
	err := c.do(ctx, http.MethodGet, "/account/keys/"+url.PathEscape(fingerprint), nil, &r)
	if IsNotFound(err) {
		return DOKey{}, false, nil
	}
	return r.Key, err == nil, err
}

// UploadKey registers a public key.
func (c *DOClient) UploadKey(ctx context.Context, name, publicKey string) (DOKey, error) {
	var r struct {
		Key DOKey `json:"ssh_key"`
	}
	err := c.do(ctx, http.MethodPost, "/account/keys", map[string]string{"name": name, "public_key": publicKey}, &r)
	return r.Key, err
}

// DeleteKey removes a key; a missing key is not an error.
func (c *DOClient) DeleteKey(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/account/keys/"+strconv.FormatInt(id, 10), nil, nil))
}

// ListFirewalls returns every firewall (the API has no name filter).
func (c *DOClient) ListFirewalls(ctx context.Context) ([]DOFirewall, error) {
	var all []DOFirewall
	err := c.pages(ctx, "/firewalls", func(b json.RawMessage) error {
		var r struct {
			Firewalls []DOFirewall `json:"firewalls"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("digitalocean API: unreadable response: %w", err)
		}
		all = append(all, r.Firewalls...)
		return nil
	})
	return all, err
}

// GetFirewall fetches a firewall (IsNotFound when gone).
func (c *DOClient) GetFirewall(ctx context.Context, id string) (DOFirewall, error) {
	var r struct {
		Firewall DOFirewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodGet, "/firewalls/"+url.PathEscape(id), nil, &r)
	return r.Firewall, err
}

// CreateFirewall creates a firewall.
func (c *DOClient) CreateFirewall(ctx context.Context, req DOCreateFirewall) (DOFirewall, error) {
	var r struct {
		Firewall DOFirewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodPost, "/firewalls", req, &r)
	return r.Firewall, err
}

// DeleteFirewall removes a firewall; a missing one is not an error.
func (c *DOClient) DeleteFirewall(ctx context.Context, id string) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/firewalls/"+url.PathEscape(id), nil, nil))
}

// ListDroplets lists droplets filtered by name or tag (query values, may be empty).
func (c *DOClient) ListDroplets(ctx context.Context, name, tag string) ([]DODroplet, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	if tag != "" {
		q.Set("tag_name", tag)
	}
	path := "/droplets"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var all []DODroplet
	err := c.pages(ctx, path, func(b json.RawMessage) error {
		var r struct {
			Droplets []DODroplet `json:"droplets"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("digitalocean API: unreadable response: %w", err)
		}
		all = append(all, r.Droplets...)
		return nil
	})
	return all, err
}

// GetDroplet fetches a droplet (IsNotFound when gone).
func (c *DOClient) GetDroplet(ctx context.Context, id int64) (DODroplet, error) {
	var r struct {
		Droplet DODroplet `json:"droplet"`
	}
	err := c.do(ctx, http.MethodGet, "/droplets/"+strconv.FormatInt(id, 10), nil, &r)
	return r.Droplet, err
}

// CreateDroplet creates a droplet and returns it with the id of its create
// action (0 when the response carries none).
func (c *DOClient) CreateDroplet(ctx context.Context, req DOCreateDroplet) (DODroplet, int64, error) {
	var r struct {
		Droplet DODroplet `json:"droplet"`
		Links   struct {
			Actions []struct {
				ID  int64  `json:"id"`
				Rel string `json:"rel"`
			} `json:"actions"`
		} `json:"links"`
	}
	if err := c.do(ctx, http.MethodPost, "/droplets", req, &r); err != nil {
		return DODroplet{}, 0, err
	}
	var action int64
	for _, a := range r.Links.Actions {
		if a.Rel == "create" || action == 0 {
			action = a.ID
		}
	}
	return r.Droplet, action, nil
}

// DeleteDroplet deletes a droplet; a missing one is not an error.
func (c *DOClient) DeleteDroplet(ctx context.Context, id int64) error {
	return ignoreNotFound(c.do(ctx, http.MethodDelete, "/droplets/"+strconv.FormatInt(id, 10), nil, nil))
}

// ActionStatus returns the status of an action: in-progress, completed or errored.
func (c *DOClient) ActionStatus(ctx context.Context, id int64) (string, error) {
	var r struct {
		Action struct {
			Status string `json:"status"`
		} `json:"action"`
	}
	err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &r)
	return r.Action.Status, err
}

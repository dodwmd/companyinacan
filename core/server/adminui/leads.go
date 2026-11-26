package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/registry"
)

// Errors of the leads client.
var (
	// ErrLeadsUnavailable means no enabled module provides `leads`, or can-core has no credential
	// for it. The UI shows an empty state, not an error.
	ErrLeadsUnavailable = errors.New("leads are not available")
	// ErrLeadNotFound is a 404 from the provider.
	ErrLeadNotFound = errors.New("no such lead")
)

// Lead is one captured submission (capability leads.v1). Its content is untrusted.
type Lead struct {
	ID        string         `json:"id"`
	FormID    string         `json:"form_id"`
	Status    string         `json:"status"`
	Email     string         `json:"email"`
	Name      string         `json:"name"`
	Fields    map[string]any `json:"fields"`
	Note      string         `json:"note"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	// Notification is "retrying" or "failed" when the lead's notification email has not got through.
	Notification string `json:"notification_status"`
}

// LeadQuery selects a page of leads.
type LeadQuery struct {
	Status string
	Cursor string
	Limit  int
}

// LeadPage is one page; NextCursor is empty on the last page.
type LeadPage struct {
	Items      []Lead
	NextCursor string
}

// LeadStatuses are the statuses of leads.v1, in display order.
var LeadStatuses = []string{"new", "contacted", "qualified", "closed", "spam"}

func validStatus(s string) bool {
	for _, v := range LeadStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// LeadsClient is the module API client interface the leads view reads through (a fake in tests).
type LeadsClient interface {
	List(ctx context.Context, q LeadQuery) (LeadPage, error)
	Get(ctx context.Context, id string) (Lead, error)
	SetStatus(ctx context.Context, id, status string) error
}

// Resolver finds providers of a capability (satisfied by *registry.Registry).
type Resolver interface {
	Resolve(ctx context.Context, capability string) (registry.Resolution, error)
}

// maxLeadsResponse bounds what the UI will read from the gateway.
const maxLeadsResponse = 4 << 20

var leadID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// HTTPLeads reads leads through the registered `leads` capability.
type HTTPLeads struct {
	Resolver Resolver
	// Token returns the bearer credential can-core presents to the provider; it is called for
	// every request so rotation needs no restart. ErrLeadsUnavailable means "not configured".
	Token  func() (string, error)
	Client *http.Client
}

// FileToken reads the credential from a file on every call (empty path: never configured).
func FileToken(path string) func() (string, error) {
	return func() (string, error) {
		if path == "" {
			return "", ErrLeadsUnavailable
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", ErrLeadsUnavailable
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", ErrLeadsUnavailable
		}
		return tok, nil
	}
}

func (c *HTTPLeads) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *HTTPLeads) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	res, err := c.Resolver.Resolve(ctx, "leads")
	if errors.Is(err, registry.ErrNoProvider) {
		return ErrLeadsUnavailable
	}
	if err != nil {
		return fmt.Errorf("resolving leads: %w", err)
	}
	if len(res.Providers) != 1 {
		return fmt.Errorf("leads: %d providers", len(res.Providers))
	}
	tok, err := c.Token()
	if err != nil {
		return err
	}
	if tok == "" {
		return ErrLeadsUnavailable
	}
	u := strings.TrimRight(res.Providers[0].URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("leads gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ErrLeadNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("leads gateway answered %d", resp.StatusCode)
	}
	lr := io.LimitReader(resp.Body, maxLeadsResponse+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(b) > maxLeadsResponse {
		return errors.New("leads gateway response too large")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// List implements LeadsClient.
func (c *HTTPLeads) List(ctx context.Context, q LeadQuery) (LeadPage, error) {
	v := url.Values{}
	if q.Status != "" {
		v.Set("status", q.Status)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(q.Limit))
	}
	var out struct {
		Items      []Lead `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/leads", v, nil, &out); err != nil {
		return LeadPage{}, err
	}
	return LeadPage{Items: out.Items, NextCursor: out.NextCursor}, nil
}

// Get implements LeadsClient.
func (c *HTTPLeads) Get(ctx context.Context, id string) (Lead, error) {
	if !leadID.MatchString(id) {
		return Lead{}, ErrLeadNotFound
	}
	var l Lead
	err := c.do(ctx, http.MethodGet, "/v1/leads/"+id, nil, nil, &l)
	return l, err
}

// SetStatus implements LeadsClient. Only the status is ever sent.
func (c *HTTPLeads) SetStatus(ctx context.Context, id, status string) error {
	if !leadID.MatchString(id) {
		return ErrLeadNotFound
	}
	return c.do(ctx, http.MethodPatch, "/v1/leads/"+id, nil, map[string]string{"status": status}, nil)
}

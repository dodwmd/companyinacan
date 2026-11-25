package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/registry"
)

// Resolver finds the provider of a capability (registry.Registry satisfies it).
type Resolver interface {
	Resolve(ctx context.Context, name string) (registry.Resolution, error)
}

// DirectoryReportingLine implements ReportingLine through the `directory` capability
// (schemas/capabilities/directory.v1.json): GET /v1/people/{id} and its manager_id. Only
// "party:<id>" subjects have a reporting line; admin-only users ("user:<name>") do not, so steps
// that need their manager use their fallback.
type DirectoryReportingLine struct {
	Resolver Resolver
	// Token returns can-core's bearer credential for the directory provider ("" = not configured).
	Token  func() (string, error)
	Client *http.Client
}

var partyRe = regexp.MustCompile(`^party:([A-Za-z0-9_.-]{1,64})$`)

// ManagerOf implements ReportingLine. Every failure other than "no manager" is ErrResolver-able by
// the caller: it is returned as a plain error, never as "no manager", so a directory outage cannot
// silently route approvals to a fallback.
func (d *DirectoryReportingLine) ManagerOf(ctx context.Context, subject string) (string, error) {
	m := partyRe.FindStringSubmatch(subject)
	if m == nil {
		return "", ErrNoManager
	}
	res, err := d.Resolver.Resolve(ctx, "directory")
	if errors.Is(err, registry.ErrNoProvider) {
		return "", ErrNoManager // no directory installed: reporting lines do not exist yet
	}
	if err != nil || len(res.Providers) != 1 {
		return "", fmt.Errorf("resolving directory: %v (%d providers)", err, len(res.Providers))
	}
	tok := ""
	if d.Token != nil {
		if tok, err = d.Token(); err != nil {
			return "", err
		}
	}
	if tok == "" {
		return "", ErrNoManager
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(res.Providers[0].URL, "/")+"/v1/people/"+url.PathEscape(m[1]), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	c := d.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("directory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", ErrNoManager
	default:
		return "", fmt.Errorf("directory answered %d", resp.StatusCode)
	}
	var p struct {
		ManagerID string `json:"manager_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return "", fmt.Errorf("directory response: %w", err)
	}
	if p.ManagerID == "" {
		return "", ErrNoManager
	}
	return "party:" + p.ManagerID, nil
}

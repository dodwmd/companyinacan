package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

const scimCT = "application/scim+json"

// SCIMClient pushes accounts to a downstream SCIM 2.0 server (RFC 7644). It looks an account up
// before creating it, so a retry after a lost answer adopts the account instead of duplicating it,
// treats "already gone" as success for disable and delete, and never creates a missing group.
type SCIMClient struct {
	ConnName string
	Base     string // e.g. https://app.example.com/scim/v2, no trailing slash
	Token    string
	HTTP     *http.Client
}

// Name implements Connector.
func (c *SCIMClient) Name() string { return c.ConnName }

// Verified implements Connector: the adapter is tested against a fake SCIM server only.
func (c *SCIMClient) Verified() bool { return false }

func (c *SCIMClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, permanent("bad_request")
		}
		rd = bytes.NewReader(b)
	}
	var st int
	var out []byte
	var err error
	hdr := map[string]string{"Authorization": "Bearer " + c.Token}
	if rd != nil {
		st, out, err = doJSON(ctx, c.HTTP, method, c.Base+path, scimCT, hdr, rd)
	} else {
		st, out, err = doJSON(ctx, c.HTTP, method, c.Base+path, "", hdr, nil)
	}
	return st, out, err
}

// scimQuote quotes a value for a SCIM filter string.
func scimQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// scimList decodes a ListResponse. A 200 that is not one (another API's JSON, a proxy's page)
// must never read as "no such account": totalResults is required (RFC 7644 section 3.4.2), and
// Resources whenever it is not zero.
func scimList[T any](b []byte) ([]T, error) {
	var l struct {
		TotalResults *int `json:"totalResults"`
		Resources    []T  `json:"Resources"`
	}
	if json.Unmarshal(b, &l) != nil || l.TotalResults == nil || *l.TotalResults < 0 || (*l.TotalResults > 0 && len(l.Resources) == 0) {
		return nil, permanent("bad_response")
	}
	return l.Resources, nil
}

type scimRef struct {
	ID string `json:"id"`
}

func (c *SCIMClient) find(ctx context.Context, resource, attr, value string) (string, error) {
	st, b, err := c.do(ctx, http.MethodGet, "/"+resource+"?count=2&filter="+url.QueryEscape(attr+" eq "+scimQuote(value)), nil)
	if err != nil {
		return "", err
	}
	if err := classify(st); err != nil {
		return "", err
	}
	found, err := scimList[scimRef](b)
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		if !extIDRe.MatchString(found[0].ID) {
			return "", permanent("bad_response")
		}
		return found[0].ID, nil
	}
	return "", permanent("ambiguous") // two accounts for one login: a human must look
}

func userBody(id Identity) map[string]any {
	return map[string]any{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":    id.UserName,
		"externalId":  id.PartyID,
		"displayName": id.DisplayName,
		"name":        map[string]string{"givenName": id.GivenName, "familyName": id.FamilyName},
		"emails":      []map[string]any{{"value": id.UserName, "type": "work", "primary": true}},
		"active":      id.Active,
	}
}

// users lists at most two users matching a filter.
func (c *SCIMClient) users(ctx context.Context, attr, value string) ([]scimUser, error) {
	st, b, err := c.do(ctx, http.MethodGet, "/Users?count=2&filter="+url.QueryEscape(attr+" eq "+scimQuote(value)), nil)
	if err != nil {
		return nil, err
	}
	if err := classify(st); err != nil {
		return nil, err
	}
	return scimList[scimUser](b)
}

type scimUser struct {
	ID         string `json:"id"`
	UserName   string `json:"userName"`
	ExternalID string `json:"externalId"`
}

// FindAccount implements AccountFinder. It names an account only when exactly one user has the
// login as userName and it is not marked as another person's. An account that has the address
// only among its emails is reported, not touched.
func (c *SCIMClient) FindAccount(ctx context.Context, id Identity) (string, error) {
	if id.UserName == "" {
		return "", permanent(codeManualCheck)
	}
	found, err := c.users(ctx, "userName", id.UserName)
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		byEmail, err := c.users(ctx, "emails.value", id.UserName)
		if err != nil {
			if code, perm := errCode(err); perm && code == "http_400" {
				return "", nil // the server cannot filter on emails: userName was the lookup
			}
			return "", err
		}
		if len(byEmail) > 0 {
			return "", permanent(codeMismatch)
		}
		return "", nil
	case 1:
		u := found[0]
		// The answer is checked too: a server that ignores filters must not get a stranger disabled.
		if !strings.EqualFold(u.UserName, id.UserName) || (partyRe.MatchString(u.ExternalID) && u.ExternalID != id.PartyID) {
			return "", permanent(codeMismatch)
		}
		if !extIDRe.MatchString(u.ID) {
			return "", permanent("bad_response")
		}
		return u.ID, nil
	}
	return "", permanent("ambiguous")
}

// CreateAccount implements Connector.
func (c *SCIMClient) CreateAccount(ctx context.Context, _ string, id Identity) (string, error) {
	if id.UserName == "" {
		return "", permanent("no_login") // a person with no work email cannot get an account
	}
	if ext, err := c.find(ctx, "Users", "userName", id.UserName); err != nil || ext != "" {
		return ext, err
	}
	st, b, err := c.do(ctx, http.MethodPost, "/Users", userBody(id))
	if err != nil {
		return "", err
	}
	if st == http.StatusConflict { // created by a call whose answer was lost
		return c.find(ctx, "Users", "userName", id.UserName)
	}
	if err := classify(st); err != nil {
		return "", err
	}
	var u struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &u) != nil || !extIDRe.MatchString(u.ID) {
		return "", permanent("bad_response")
	}
	return u.ID, nil
}

func patch(ops ...map[string]any) map[string]any {
	return map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": ops}
}

func (c *SCIMClient) patchUser(ctx context.Context, ext string, ops ...map[string]any) error {
	if !extIDRe.MatchString(ext) {
		return permanent("bad_account")
	}
	st, _, err := c.do(ctx, http.MethodPatch, "/Users/"+url.PathEscape(ext), patch(ops...))
	if err != nil {
		return err
	}
	return classify(st)
}

// UpdateAccount implements Connector.
func (c *SCIMClient) UpdateAccount(ctx context.Context, _, ext string, id Identity) error {
	return c.patchUser(ctx, ext,
		map[string]any{"op": "replace", "path": "displayName", "value": id.DisplayName},
		map[string]any{"op": "replace", "path": "name.givenName", "value": id.GivenName},
		map[string]any{"op": "replace", "path": "name.familyName", "value": id.FamilyName},
		map[string]any{"op": "replace", "path": "active", "value": id.Active})
}

// DisableAccount implements Connector. A missing account is already disabled.
func (c *SCIMClient) DisableAccount(ctx context.Context, _, ext string) error {
	err := c.patchUser(ctx, ext, map[string]any{"op": "replace", "path": "active", "value": false})
	if ce, ok := err.(*ConnError); ok && ce.Code == "http_404" {
		return nil
	}
	return err
}

// DeleteAccount implements Connector.
func (c *SCIMClient) DeleteAccount(ctx context.Context, _, ext string) error {
	if !extIDRe.MatchString(ext) {
		return permanent("bad_account")
	}
	st, _, err := c.do(ctx, http.MethodDelete, "/Users/"+url.PathEscape(ext), nil)
	if err != nil {
		return err
	}
	if st == http.StatusNotFound {
		return nil
	}
	return classify(st)
}

// AddToGroup implements Connector.
func (c *SCIMClient) AddToGroup(ctx context.Context, _, ext, group string) error {
	gid, err := c.find(ctx, "Groups", "displayName", group)
	if err != nil {
		return err
	}
	if gid == "" {
		return permanent("group_not_found")
	}
	st, _, err := c.do(ctx, http.MethodPatch, "/Groups/"+url.PathEscape(gid), patch(map[string]any{"op": "add", "path": "members", "value": []map[string]string{{"value": ext}}}))
	if err != nil {
		return err
	}
	return classify(st)
}

// RemoveFromGroup implements Connector. A missing group or member is already removed.
func (c *SCIMClient) RemoveFromGroup(ctx context.Context, _, ext, group string) error {
	gid, err := c.find(ctx, "Groups", "displayName", group)
	if err != nil || gid == "" {
		return err
	}
	st, _, err := c.do(ctx, http.MethodPatch, "/Groups/"+url.PathEscape(gid), patch(map[string]any{"op": "remove", "path": "members[value eq " + scimQuote(ext) + "]"}))
	if err != nil {
		return err
	}
	if st == http.StatusNotFound {
		return nil
	}
	return classify(st)
}

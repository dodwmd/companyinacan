package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Person is the part of a directory record this module reads. It deliberately has no date of
// birth, national id, address, personal phone or compensation: those fields are never requested,
// so they cannot be exposed by a bug here.
type Person struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	SSOSubject   string `json:"sso_subject"`
	GivenName    string `json:"given_name"`
	FamilyName   string `json:"family_name"`
	Preferred    string `json:"preferred_name"`
	WorkEmail    string `json:"work_email"`
	WorkPhone    string `json:"work_phone"`
	Location     string `json:"location"`
	DepartmentID string `json:"department_id"`
	TeamID       string `json:"team_id"`
	ManagerID    string `json:"manager_id"`
	StartDate    string `json:"start_date"`
	EndDate      string `json:"end_date"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// DisplayName is the preferred name when set, otherwise the given and family names.
func (p Person) DisplayName() string {
	if p.Preferred != "" {
		return p.Preferred
	}
	return strings.TrimSpace(p.GivenName + " " + p.FamilyName)
}

// Employed reports whether the person currently holds access: onboarding, active, on leave or
// offboarding. Leave is not distinguished from active anywhere this module exposes (it can reveal
// health or family circumstances).
func (p Person) Employed() bool {
	switch p.Status {
	case "onboarding", "active", "leave", "offboarding":
		return true
	}
	return false
}

// Identity converts the record for a connector.
func (p Person) Identity() Identity {
	return Identity{PartyID: p.ID, UserName: p.WorkEmail, GivenName: p.GivenName, FamilyName: p.FamilyName, DisplayName: p.DisplayName(), Active: p.Employed()}
}

// PersonPatch is the only change this module makes to a directory record (through SCIM PATCH).
type PersonPatch struct {
	PreferredName *string `json:"preferred_name,omitempty"`
	WorkPhone     *string `json:"work_phone,omitempty"`
}

// ListQuery filters a directory listing.
type ListQuery struct {
	Status string
	Cursor string
	Limit  int
}

// Directory is the `directory` capability as this module uses it; tests substitute a fake.
type Directory interface {
	Get(ctx context.Context, id string) (Person, error)
	List(ctx context.Context, q ListQuery) (people []Person, next string, err error)
	Update(ctx context.Context, id string, p PersonPatch) (Person, error)
}

// HTTPDirectory talks to the people module's directory API with a bearer token that the operator
// created for this module (people.read and people.write, which maps to the hr role there).
type HTTPDirectory struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

const maxDirResponse = 4 << 20

func (d *HTTPDirectory) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (d *HTTPDirectory) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: directory unreachable", ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxDirResponse))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return b, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusUnprocessableEntity, resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: directory refused the change", ErrInvalid)
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: directory refused this module's token", ErrUnavailable) // fail closed: treat as "cannot know"
	}
	return nil, fmt.Errorf("%w: directory answered HTTP %d", ErrUnavailable, resp.StatusCode)
}

// Get implements Directory.
func (d *HTTPDirectory) Get(ctx context.Context, id string) (Person, error) {
	if !partyRe.MatchString(id) {
		return Person{}, ErrNotFound
	}
	b, err := d.do(ctx, http.MethodGet, "/v1/people/"+id, nil)
	if err != nil {
		return Person{}, err
	}
	var p Person
	if json.Unmarshal(b, &p) != nil || p.ID != id {
		return Person{}, fmt.Errorf("%w: directory answer is not a person", ErrUnavailable)
	}
	return p, nil
}

// List implements Directory.
func (d *HTTPDirectory) List(ctx context.Context, q ListQuery) ([]Person, string, error) {
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
	b, err := d.do(ctx, http.MethodGet, "/v1/people?"+v.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Items      []Person `json:"items"`
		NextCursor string   `json:"next_cursor"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, "", fmt.Errorf("%w: directory answer is not a list", ErrUnavailable)
	}
	return out.Items, out.NextCursor, nil
}

// Update implements Directory.
func (d *HTTPDirectory) Update(ctx context.Context, id string, p PersonPatch) (Person, error) {
	if !partyRe.MatchString(id) {
		return Person{}, ErrNotFound
	}
	b, err := d.do(ctx, http.MethodPatch, "/v1/people/"+id, p)
	if err != nil {
		return Person{}, err
	}
	var out Person
	if json.Unmarshal(b, &out) != nil || out.ID != id {
		return Person{}, fmt.Errorf("%w: directory answer is not a person", ErrUnavailable)
	}
	return out, nil
}

// Tasks creates the checklist of tasks for a person from a template in the people module. The
// templates carry the assignees (manager, hr, admin); this module never assigns a task to the
// person it is about and never completes one.
type Tasks interface {
	// Ensure creates the checklist unless the person already has one from that template, and
	// reports whether it created one.
	Ensure(ctx context.Context, personID, templateID, anchorDate string) (bool, error)
}

// HTTPTasks implements Tasks over the people API.
type HTTPTasks struct{ Dir *HTTPDirectory }

// Ensure implements Tasks.
func (t *HTTPTasks) Ensure(ctx context.Context, personID, templateID, anchor string) (bool, error) {
	if !partyRe.MatchString(personID) || templateID == "" {
		return false, ErrInvalid
	}
	b, err := t.Dir.do(ctx, http.MethodGet, "/v1/people/"+personID+"/checklists", nil)
	if err != nil {
		return false, err
	}
	var l struct {
		Items []struct {
			TemplateID string `json:"template_id"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &l) != nil {
		return false, fmt.Errorf("%w: checklist answer is not a list", ErrUnavailable)
	}
	for _, c := range l.Items {
		if c.TemplateID == templateID {
			return false, nil
		}
	}
	body := map[string]string{"template_id": templateID}
	if anchor != "" {
		body["anchor_date"] = anchor
	}
	if _, err := t.Dir.do(ctx, http.MethodPost, "/v1/people/"+personID+"/checklists", body); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, ErrInvalid
		}
		return false, err
	}
	return true, nil
}

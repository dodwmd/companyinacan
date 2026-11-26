package equity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Directory is the people module's `directory` capability as equity uses it. Both calls fail
// closed: an error means "cannot know", never "no".
type Directory interface {
	// PersonForSubject is the id of the person whose sso_subject is exactly subject, "" if none.
	PersonForSubject(ctx context.Context, subject string) (string, error)
	// PersonExists reports whether the id names a person who has not been erased.
	PersonExists(ctx context.Context, id string) (bool, error)
}

// HTTPDirectory talks to the people module's directory API. The token must be one the API answers
// with sso_subject: the read-only identity lookup token can-core copies to this module
// (CAN_PEOPLE_LOOKUP_TOKEN, specs/057-module-capability-wiring), or one with the hr role there.
type HTTPDirectory struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type dirPerson struct {
	ID         string `json:"id"`
	SSOSubject string `json:"sso_subject"`
	Erased     bool   `json:"erased"`
}

const maxDirResponse = 1 << 20

var errDirectory = errors.New("the people directory gave no usable answer")

// get returns the body of a 200 or 404 answer; anything else is an error.
func (d *HTTPDirectory) get(ctx context.Context, path string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, 0, errDirectory
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	req.Header.Set("Accept", "application/json")
	client := d.HTTP
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: unreachable", errDirectory)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil, 0, fmt.Errorf("%w: HTTP %d", errDirectory, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDirResponse))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: unreadable", errDirectory)
	}
	return b, resp.StatusCode, nil
}

// PersonForSubject implements Directory. Every returned record must carry exactly the subject
// asked for, so a people module that ignores the filter (or hides sso_subject from this token)
// is an error and never a match.
func (d *HTTPDirectory) PersonForSubject(ctx context.Context, subject string) (string, error) {
	b, status, err := d.get(ctx, "/v1/people?limit=2&sso_subject="+url.QueryEscape(subject))
	if err != nil {
		return "", err
	}
	var out struct {
		Items []dirPerson `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(b, &out) != nil || out.Items == nil || len(out.Items) > 1 {
		return "", errDirectory
	}
	if len(out.Items) == 0 {
		return "", nil
	}
	p := out.Items[0]
	if p.SSOSubject != subject || !personIDRe.MatchString(p.ID) {
		return "", errDirectory
	}
	return p.ID, nil
}

// PersonExists implements Directory.
func (d *HTTPDirectory) PersonExists(ctx context.Context, id string) (bool, error) {
	if !personIDRe.MatchString(id) {
		return false, nil
	}
	b, status, err := d.get(ctx, "/v1/people/"+id)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	var p dirPerson
	if json.Unmarshal(b, &p) != nil || p.ID != id {
		return false, errDirectory
	}
	return !p.Erased, nil
}

// subjectCache keeps answers about callers briefly: the directory limits requests per token.
// Errors are never cached.
type subjectCache struct {
	mu sync.Mutex
	m  map[string]subjectEntry
}

type subjectEntry struct {
	person  string
	expires time.Time
}

const (
	subjectTTL      = 15 * time.Second
	maxSubjectCache = 1024
)

func (s *Service) personFor(ctx context.Context, subject string) (string, error) {
	now := s.now()
	s.subjects.mu.Lock()
	e, ok := s.subjects.m[subject]
	s.subjects.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.person, nil
	}
	person, err := s.directory.PersonForSubject(ctx, subject)
	if err != nil {
		return "", err
	}
	s.subjects.mu.Lock()
	defer s.subjects.mu.Unlock()
	if s.subjects.m == nil || len(s.subjects.m) >= maxSubjectCache {
		s.subjects.m = map[string]subjectEntry{}
	}
	s.subjects.m[subject] = subjectEntry{person: person, expires: now.Add(subjectTTL)}
	return person, nil
}

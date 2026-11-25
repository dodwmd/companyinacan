package jobs

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Roles. A token carries exactly one role; Jobs optionally narrows it further.
const (
	RoleAdmin         = "admin"
	RoleRecruiter     = "recruiter"
	RoleHiringManager = "hiring_manager"
	RoleInterviewer   = "interviewer"
)

// Roles lists every role, strongest first.
var Roles = []string{RoleAdmin, RoleRecruiter, RoleHiringManager, RoleInterviewer}

// Actions the permission matrix decides. Visibility inside a role (which jobs and which
// applications) is a separate, query-level scope: see scope.go.
type Action string

// Actions.
const (
	ActJobRead       Action = "job.read"
	ActJobWrite      Action = "job.write"
	ActApprove       Action = "job.approve"
	ActAppRead       Action = "app.read"
	ActAppCreate     Action = "app.create"
	ActAppMove       Action = "app.move"
	ActHire          Action = "app.hire"
	ActAssign        Action = "app.assign"
	ActScore         Action = "app.score"
	ActCandidateRead Action = "candidate.read"
	ActCandidateEdit Action = "candidate.edit"
	ActTalentPool    Action = "talentpool.read"
	ActStages        Action = "stages.write"
	ActAdmin         Action = "admin" // tokens, audit, events, erasure, holds, retention, EEO reports
)

var matrix = map[Action][]string{
	ActJobRead:       {RoleAdmin, RoleRecruiter, RoleHiringManager},
	ActJobWrite:      {RoleAdmin, RoleRecruiter},
	ActApprove:       {RoleAdmin},
	ActAppRead:       {RoleAdmin, RoleRecruiter, RoleHiringManager, RoleInterviewer},
	ActAppCreate:     {RoleAdmin, RoleRecruiter},
	ActAppMove:       {RoleAdmin, RoleRecruiter, RoleHiringManager},
	ActHire:          {RoleAdmin, RoleRecruiter},
	ActAssign:        {RoleAdmin, RoleRecruiter, RoleHiringManager},
	ActScore:         {RoleAdmin, RoleRecruiter, RoleHiringManager, RoleInterviewer},
	ActCandidateRead: {RoleAdmin, RoleRecruiter, RoleHiringManager},
	ActCandidateEdit: {RoleAdmin, RoleRecruiter},
	ActTalentPool:    {RoleAdmin, RoleRecruiter},
	ActStages:        {RoleAdmin},
	ActAdmin:         {RoleAdmin},
}

// ErrUnauthenticated means no valid credentials were presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// Principal is an authenticated caller. Jobs limits which jobs the caller can reach (empty
// means every job the role allows); the role may narrow that further (a hiring manager sees
// only the jobs they manage, an interviewer only the applications they are assigned to).
type Principal struct {
	Subject string
	TokenID string
	Kind    string // personal, service or static
	Role    string
	Jobs    []string
}

// Can reports whether the principal's role may perform an action at all.
//
// Actions that are global rather than per job (admin, stages) additionally need a token with
// no job scope: a token restricted to some jobs must not erase, audit or list everything.
func (p *Principal) Can(a Action) bool {
	if (a == ActAdmin || a == ActStages) && !p.Unscoped() {
		return false
	}
	return slices.Contains(matrix[a], p.Role)
}

// CanJob reports whether the token's own job scope covers a job id.
func (p *Principal) CanJob(id string) bool { return len(p.Jobs) == 0 || slices.Contains(p.Jobs, id) }

// Unscoped reports whether the token has no job restriction.
func (p *Principal) Unscoped() bool { return len(p.Jobs) == 0 }

func roleRank(r string) int {
	for i, x := range Roles {
		if x == r {
			return len(Roles) - i
		}
	}
	return 0
}

// Authenticator turns a request into a Principal. It is the seam for can-core token
// introspection (POST /v1/tokens/introspect, SPEC.md 4.3).
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// BearerToken extracts the bearer credential from a request.
func BearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

type staticEntry struct {
	hash [sha256.Size]byte
	p    Principal
}

// StaticTokens authenticates fixed bearer tokens in constant time. The bootstrap admin token
// comes from the environment; day-to-day tokens live in the database.
type StaticTokens struct{ entries []staticEntry }

// Add registers a token. Empty tokens are ignored so unset env vars cannot become credentials.
func (s *StaticTokens) Add(token string, p Principal) {
	if token == "" {
		return
	}
	s.entries = append(s.entries, staticEntry{hash: sha256.Sum256([]byte(token)), p: p})
}

// Authenticate implements Authenticator, comparing against every entry.
func (s *StaticTokens) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := BearerToken(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(tok))
	var found *Principal
	for i := range s.entries {
		if subtle.ConstantTimeCompare(h[:], s.entries[i].hash[:]) == 1 {
			p := s.entries[i].p
			found = &p
		}
	}
	if found == nil {
		return nil, ErrUnauthenticated
	}
	return found, nil
}

// Chain tries each authenticator in order and returns the first success.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}

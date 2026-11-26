package modclient

// The `summary` capability, summary.v1 (specs/053-business-overview): a few aggregate figures a
// module computes for one person, shown on the admin UI's business overview. The contract has no
// free text: a figure is a key, a count and, for money, amounts by currency, so it cannot carry
// personal data by construction.
//
// can-core calls GET /v1/summary with a summary grant: a random, single-use token bound to one
// module and one person that only that module can have introspected. It is not a personal access
// token: no other route of a module accepts it, and can-core keeps no standing credential for any
// module's data.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// SummaryPath is where a module serves summary.v1.
	SummaryPath = "/v1/summary"
	// SummaryCapability is the capability name in module.yaml and the registry.
	SummaryCapability = "summary"
	// SummaryVersion is the version of the answer.
	SummaryVersion = 1
	// SummaryTokenPrefix starts a summary grant.
	SummaryTokenPrefix = "cansum_"
	// SummaryKind is the introspection kind of a summary grant; personal tokens are "user".
	SummaryKind = "summary"
	// maxSummaryRedemptions bounds how many grants a module has introspected at once, so junk sent
	// to this route cannot become a flood of calls to can-core.
	maxSummaryRedemptions = 4
	summaryTimeout        = 3 * time.Second
	// suspectFor is how long an address that went over the failure limit is served one at a time;
	// maxSuspects bounds how many are remembered.
	suspectFor  = time.Minute
	maxSuspects = 1024
)

// SummaryLimit puts GET /v1/summary behind the module's limiter on failed authentication. The zero
// value limits nothing.
//
// Only failures count, and a grant is checked before anything is decided about it, so a valid grant
// is never refused for what its address, or any other, sent before. An address over the limit gets
// 429 for its failures and is served one request at a time, which leaves the remaining
// introspection slots to everyone else (can-core among them).
type SummaryLimit struct {
	// Client is the address a request is counted against, as the module's own limiter sees it.
	Client func(*http.Request) string
	// Failed records one failure of client and reports whether it is still within its allowance.
	Failed func(client string) bool
}

var (
	summaryTokenRe   = regexp.MustCompile(`^cansum_[A-Za-z0-9_-]{43}$`)
	summarySubjectRe = regexp.MustCompile(`^user:[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
)

// SummaryTokenShaped reports whether s has the shape of a summary grant.
func SummaryTokenShaped(s string) bool { return summaryTokenRe.MatchString(s) }

// SummaryAmount is money in one currency's minor unit. Currencies are never added together.
type SummaryAmount struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

// SummaryFigure is one aggregate. Key is one of the keys of schemas/capabilities/summary.v1.json.
type SummaryFigure struct {
	Key     string          `json:"key"`
	Count   int64           `json:"count"`
	Amounts []SummaryAmount `json:"amounts,omitempty"`
}

// Summary is the answer of GET /v1/summary.
type Summary struct {
	Version int             `json:"version"`
	Figures []SummaryFigure `json:"figures"`
}

// SummaryBuilder computes the figures subject may see. permissions are the module's manifest
// permissions the person holds in can-core right now; the module applies its own policy to them
// and leaves out every figure the person may not see (none at all is not an error).
type SummaryBuilder func(ctx context.Context, subject string, permissions []string) ([]SummaryFigure, error)

// SummaryHandler serves GET /v1/summary for an installed module. Every failure to authenticate is
// the same 401; when can-core cannot be asked the answer is 503. Nothing is cached here and the
// token is never logged.
func SummaryHandler(api IntrospectAPI, log *slog.Logger, limit SummaryLimit, build SummaryBuilder) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	slots := make(chan struct{}, maxSummaryRedemptions)
	narrow := make(chan struct{}, 1)
	sus := &suspects{until: map[string]time.Time{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			h.Set("Allow", http.MethodGet)
			summaryError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		client := ""
		if limit.Client != nil {
			client = limit.Client(r)
		}
		refuse := func() {
			if limit.Failed != nil && !limit.Failed(client) {
				sus.add(client, time.Now())
				h.Set("Retry-After", "60")
				summaryError(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
			summaryError(w, http.StatusUnauthorized, "a summary grant is required")
		}
		scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		tok = strings.TrimSpace(tok)
		if !strings.EqualFold(scheme, "Bearer") || !SummaryTokenShaped(tok) {
			refuse()
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), summaryTimeout)
		defer cancel()
		lanes := []chan struct{}{slots}
		if sus.has(client, time.Now()) {
			lanes = []chan struct{}{narrow, slots}
		}
		id, err := introspectIn(ctx, api, tok, lanes)
		if errors.Is(err, errSummaryBusy) {
			summaryError(w, http.StatusServiceUnavailable, "busy")
			return
		}
		if err != nil {
			summaryError(w, http.StatusServiceUnavailable, "cannot check the grant")
			return
		}
		if !id.Active || id.Kind != SummaryKind || !summarySubjectRe.MatchString(id.Subject) {
			refuse()
			return
		}
		figures, err := build(ctx, id.Subject, id.Permissions)
		if err != nil {
			log.Error("summary failed", "error", err)
			summaryError(w, http.StatusInternalServerError, "summary failed")
			return
		}
		if figures == nil {
			figures = []SummaryFigure{}
		}
		h.Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Summary{Version: SummaryVersion, Figures: figures})
	})
}

var errSummaryBusy = errors.New("busy")

// introspectIn asks can-core about a grant while holding a place in each lane.
func introspectIn(ctx context.Context, api IntrospectAPI, tok string, lanes []chan struct{}) (Identity, error) {
	for _, lane := range lanes {
		select {
		case lane <- struct{}{}:
			defer func() { <-lane }()
		case <-ctx.Done():
			return Identity{}, errSummaryBusy
		}
	}
	return api.Introspect(ctx, tok)
}

// suspects are the addresses that recently went over the failure limit.
type suspects struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (s *suspects) add(client string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.until) >= maxSuspects {
		for k, t := range s.until {
			if !now.Before(t) {
				delete(s.until, k)
			}
		}
		if len(s.until) >= maxSuspects {
			s.until = map[string]time.Time{}
		}
	}
	s.until[client] = now.Add(suspectFor)
}

func (s *suspects) has(client string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.until[client])
}

func summaryError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "detail": detail})
}

package forms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

var canSubjectRe = regexp.MustCompile(`^user:[A-Za-z0-9_.@-]{1,64}$`)

func bearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

// CanAuth authenticates the personal access tokens can-core issues by asking can-core's token
// introspection who they belong to (specs/016-module-wiring). Any doubt is "unauthenticated".
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := bearerToken(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	id, err := a.In.Lookup(r.Context(), tok)
	if err != nil || !id.Active || id.Kind != "user" || !canSubjectRe.MatchString(id.Subject) {
		return nil, ErrUnauthenticated
	}
	var perms []string
	for _, p := range id.Permissions {
		if (p == PermLeadsRead || p == PermLeadsWrite || p == PermLeadsErase) && !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return nil, ErrUnauthenticated
	}
	return &Principal{Subject: id.Subject, Permissions: perms}, nil
}

// chainAuth tries each authenticator in order and fails closed when none accepts.
type chainAuth []Authenticator

// Authenticate implements Authenticator.
func (c chainAuth) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}

// canEvent is the lead.created.v1 envelope as published through can-core. It carries the ids only:
// the lead's name, e-mail address and the submitted values stay in this module's database and are
// read through the `leads` capability by someone allowed to (the event is kept by can-core, a
// derived store, and feeds notifications). `fields` is required by the event schema and is empty.
func canEvent(ev Event) []byte {
	b, _ := json.Marshal(map[string]any{"id": ev.ID, "type": ev.Type, "time": ev.Time, "source": ev.Source,
		"data": map[string]any{"lead_id": ev.Data.LeadID, "form_id": ev.Data.FormID, "submitted_at": ev.Data.SubmittedAt, "fields": map[string]string{}}})
	return b
}

// publishCan publishes a lead event through can-core, retrying briefly when can-core is
// unavailable. After the attempts the event is lost (forms keeps no outbox): the lead itself is
// stored, and a later requeue is the operator's call.
func publishCan(ctx context.Context, c *modclient.Client, ev Event, sleep func(context.Context, time.Duration) error) (int, error) {
	body := canEvent(ev)
	backoff := time.Second
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		if err = c.PublishEnvelope(ctx, body); err == nil {
			return attempt, nil
		}
		if !errors.Is(err, modclient.ErrUnavailable) || attempt == 4 {
			return attempt, err
		}
		if serr := sleep(ctx, backoff); serr != nil {
			return attempt, serr
		}
		backoff *= 2
	}
	return 4, err
}

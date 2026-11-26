package files

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Authenticator resolves a bearer token. A nil principal with a nil error means "not valid"; an
// error means the answer could not be had (503), never an accepted token.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// StaticToken authenticates the service token configured for can-core, other modules and
// operators (CAN_FILES_API_TOKEN). It holds files.admin and acts as "svc:static": it owns the
// spaces it creates and sees nothing it was not given a role in.
type StaticToken struct{ Token string }

// Authenticate implements Authenticator. Both sides are hashed so the comparison takes the same
// time whatever the lengths.
func (s StaticToken) Authenticate(_ context.Context, tok string) (*Principal, error) {
	if len(s.Token) < 24 {
		return nil, nil
	}
	a, b := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(s.Token))
	if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
		return &Principal{Subject: "svc:static", Perms: []string{PermAdmin}}, nil
	}
	return nil, nil
}

// StaticRestoreToken authenticates the restore-only credential (CAN_FILES_RESTORE_TOKEN, optional).
// Its principal holds no permission: it opens the purge hook and nothing else.
type StaticRestoreToken struct{ Token string }

// Authenticate implements Authenticator.
func (s StaticRestoreToken) Authenticate(_ context.Context, tok string) (*Principal, error) {
	if len(s.Token) < 32 {
		return nil, nil
	}
	a, b := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(s.Token))
	if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
		return &Principal{Subject: "svc:restore", Restore: true}, nil
	}
	return nil, nil
}

// CanAuth authenticates the personal access tokens can-core issues, by introspection. Any doubt
// is "not valid"; an introspection outage is an error.
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(ctx context.Context, tok string) (*Principal, error) {
	if !modclient.TokenShaped(tok) {
		return nil, nil
	}
	id, err := a.In.Lookup(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("%w: token introspection", ErrUnavailable)
	}
	if !id.Active || id.Kind != "user" || !userRe.MatchString(id.Subject) {
		return nil, nil
	}
	var perms []string
	for _, p := range id.Permissions {
		if p == PermRead || p == PermWrite || p == PermAdmin {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return nil, nil
	}
	return &Principal{Subject: id.Subject, Perms: perms}, nil
}

// MultiAuth tries each authenticator in turn.
type MultiAuth []Authenticator

// Authenticate implements Authenticator.
func (m MultiAuth) Authenticate(ctx context.Context, tok string) (*Principal, error) {
	var firstErr error
	for _, a := range m {
		p, err := a.Authenticate(ctx, tok)
		if p != nil {
			return p, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// GroupResolver returns the opaque directory group ids a person belongs to, so a space can grant
// access to a group. nil means no group grants apply (space roles for users still work).
type GroupResolver interface {
	Groups(ctx context.Context, subject string) ([]string, error)
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

// clientIP returns the address used for rate limiting. Forwarding headers are honoured only when
// the operator says a trusted proxy sets them, and then the right-most entry (appended by that
// proxy) is used, never a client-supplied left-most one.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// Header lines, not Get: Get returns the first line, which the client may have sent.
		if vs := r.Header.Values("X-Forwarded-For"); len(vs) > 0 {
			xff := vs[len(vs)-1]
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

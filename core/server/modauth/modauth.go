// Package modauth authenticates module-API callers: the projected ServiceAccount token is
// validated by a TokenReview and mapped to the installed module record bound to that
// (namespace, service account).
package modauth

import (
	"context"
	"errors"

	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

// Errors.
var (
	// ErrUnauthenticated: the token is invalid or belongs to no installed module.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: the module is installed but not enabled.
	ErrForbidden = errors.New("module is not enabled")
)

// Authenticator resolves tokens to modules.
type Authenticator struct {
	reviewer tokenreview.Reviewer
	db       *store.DB
}

// New returns an Authenticator.
func New(r tokenreview.Reviewer, db *store.DB) *Authenticator {
	return &Authenticator{reviewer: r, db: db}
}

// Authenticate returns the module a token belongs to.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (store.Module, error) {
	if token == "" {
		return store.Module{}, ErrUnauthenticated
	}
	id, err := a.reviewer.Review(ctx, token, tokenreview.Audience)
	if err != nil {
		if errors.Is(err, tokenreview.ErrInvalidToken) {
			return store.Module{}, ErrUnauthenticated
		}
		return store.Module{}, err // infrastructure failure: surface as 5xx, not 401
	}
	m, err := a.db.ModuleByIdentity(ctx, id.Namespace, id.ServiceAccount)
	if errors.Is(err, store.ErrNotFound) {
		return store.Module{}, ErrUnauthenticated
	}
	if err != nil {
		return store.Module{}, err
	}
	if m.State != store.StateEnabled {
		return m, ErrForbidden
	}
	return m, nil
}

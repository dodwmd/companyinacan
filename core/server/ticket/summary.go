package ticket

// Summary grants (specs/053-business-overview): how can-core asks a module for aggregate figures on
// behalf of the signed-in person without holding any credential for the module's data. A grant is a
// random token, kept only as a hash and only in memory, bound to one module (the audience) and one
// person, valid for a few seconds and answered once. The module that receives it asks can-core
// (token introspection) who it is for; the permissions are computed from RBAC at that moment. No
// other module learns anything from it, and no route but /v1/summary accepts it.

import (
	"context"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

const (
	// SummaryGrantTTL is how long a module has to redeem a grant.
	SummaryGrantTTL = 15 * time.Second
	maxGrants       = 1024
)

type grant struct {
	subject, module string
	expires         time.Time
}

// Standing is the module permissions subject holds right now; ok is false when the module is not
// enabled, the account is gone or disabled, or the person holds none of them. Errors deny.
func (s *Service) Standing(ctx context.Context, module, subject string) (permissions []string, ok bool) {
	_, perms, ok := s.standing(ctx, module, subject)
	return perms, ok
}

// MintSummary returns a grant for module to compute subject's figures. It is ErrNotAllowed when the
// person has no standing in the module, and ErrRateLimited when too many grants are outstanding.
func (s *Service) MintSummary(ctx context.Context, subject, module string) (string, error) {
	if _, _, ok := s.standing(ctx, module, subject); !ok {
		return "", ErrNotAllowed
	}
	now := s.now()
	tok := randomSecret(modclient.SummaryTokenPrefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants == nil {
		s.grants = map[string]grant{}
	}
	if len(s.grants) >= maxGrants {
		for k, g := range s.grants {
			if !now.Before(g.expires) {
				delete(s.grants, k)
			}
		}
		if len(s.grants) >= maxGrants {
			return "", ErrRateLimited
		}
	}
	s.grants[hashOf(tok)] = grant{subject: subject, module: module, expires: now.Add(SummaryGrantTTL)}
	return tok, nil
}

// DropSummary forgets a grant that will not be redeemed (the module did not answer).
func (s *Service) DropSummary(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, hashOf(token))
}

// SummaryRedeemed is what a module learns from a grant.
type SummaryRedeemed struct {
	Subject     string
	Permissions []string
	ExpiresAt   time.Time
}

// RedeemSummary answers a grant, once, to the module it was minted for. Every other case (unknown,
// used, expired, another module's, the person gone or without a permission) is false. Another
// module's attempt does not spend it.
func (s *Service) RedeemSummary(ctx context.Context, callerModule, token string) (SummaryRedeemed, bool) {
	if !modclient.SummaryTokenShaped(token) {
		s.penalise(ctx, callerModule)
		return SummaryRedeemed{}, false
	}
	key := hashOf(token)
	s.mu.Lock()
	g, found := s.grants[key]
	if found && g.module == callerModule {
		delete(s.grants, key)
	}
	s.mu.Unlock()
	if !found || g.module != callerModule || !s.now().Before(g.expires) {
		s.penalise(ctx, callerModule)
		return SummaryRedeemed{}, false
	}
	_, perms, ok := s.standing(ctx, callerModule, g.subject)
	if !ok {
		return SummaryRedeemed{}, false
	}
	return SummaryRedeemed{Subject: g.subject, Permissions: perms, ExpiresAt: g.expires}, true
}

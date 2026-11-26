package provision

// A connector this module holds no account for may still have one: the person existed before the
// connector did (staff imported from CSV publish no joiner event), or somebody made it by hand.
// A leaver or mover must never skip such a connector and report success. It looks the account up
// where the connector can, and otherwise dead-letters the step, naming the connector, for a person
// to deal with (the run ends failed and provision.failed.v1 is published).

import (
	"context"
	"errors"
	"strings"
)

// argUnlinked marks a planned step for a connector with no recorded account.
const argUnlinked = "unlinked"

// Codes a person has to act on.
const (
	codeManualCheck = "manual_check_required" // the connector cannot look accounts up, or there is no work email
	codeNotLinked   = "account_not_linked"    // a mover found an account; link it before it can be changed
	codeMismatch    = "identity_mismatch"     // the account found may be somebody else's
	codeSharedLogin = "shared_login"          // another current person has the same work email
	codeNoCore      = "no_core_account"       // the directory links no can-core account to revoke
	codeNotFound    = "no_account_found"      // looked up and not there (the step is skipped)
)

// AccountFinder is implemented by connectors that can find an account by its login. It returns ""
// when there is none and a permanent error when it cannot be sure that one account is this
// person's: nothing is ever changed on a doubtful match.
type AccountFinder interface {
	FindAccount(ctx context.Context, id Identity) (externalID string, err error)
}

// unlinked runs a leaver or mover step at a connector with no recorded account.
func (e *Engine) unlinked(ctx context.Context, run Run, c Connector, key string) (ext, skip string, err error) {
	finder, ok := c.(AccountFinder)
	if !ok {
		return "", "", permanent(codeManualCheck)
	}
	p, err := e.Dir.Get(ctx, run.PartyID)
	if errors.Is(err, ErrNotFound) {
		return "", "", permanent(codeManualCheck)
	}
	if err != nil {
		return "", "", err
	}
	if p.WorkEmail == "" {
		return "", "", permanent(codeManualCheck)
	}
	ext, err = finder.FindAccount(ctx, p.Identity())
	if err != nil {
		return "", "", err
	}
	if ext == "" {
		return "", codeNotFound, nil
	}
	if other, err := e.Store.accountOwnedByAnother(ctx, run.PartyID, c.Name(), ext); err != nil {
		return "", "", err
	} else if other {
		return "", "", permanent(codeMismatch)
	}
	if shared, err := e.loginShared(ctx, p); err != nil {
		return "", "", err
	} else if shared {
		return "", "", permanent(codeSharedLogin)
	}
	if run.Kind != KindLeaver {
		return "", "", permanent(codeNotLinked)
	}
	// Only ever disabled: an account found by lookup is never deleted.
	return ext, "", c.DisableAccount(ctx, key, ext)
}

// maxDirectoryPages bounds the scan for a shared login (100 records a page).
const maxDirectoryPages = 200

// loginShared reports whether another person who still holds access has the same work email, for
// example a returning employee's new record. A directory too large to read through counts as shared.
func (e *Engine) loginShared(ctx context.Context, p Person) (bool, error) {
	cursor := ""
	for range maxDirectoryPages {
		people, next, err := e.Dir.List(ctx, ListQuery{Cursor: cursor, Limit: 100})
		if err != nil {
			return false, err
		}
		for _, o := range people {
			if o.ID != p.ID && o.Employed() && strings.EqualFold(o.WorkEmail, p.WorkEmail) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		cursor = next
	}
	return true, nil
}

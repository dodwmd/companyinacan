package modclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Ticket, handle and browser-nonce shapes (core/server/ticket): refused locally, so junk is never
// sent to can-core.
const (
	TicketPrefix  = "cant_"
	HandlePrefix  = "cans_"
	BindingPrefix = "canb_"
	secretLen     = 43
)

func shaped(s, prefix string) bool {
	if len(s) != len(prefix)+secretLen || s[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// TicketShaped reports whether s could be a launch ticket.
func TicketShaped(s string) bool { return shaped(s, TicketPrefix) }

// BindingShaped reports whether s could be the browser nonce of a launch.
func BindingShaped(s string) bool { return shaped(s, BindingPrefix) }

// LaunchCookieName is the name of the nonce cookie can-core sets in the launching browser for a
// module. Over https it carries the __Secure- prefix, which a browser only accepts from a secure
// origin.
func LaunchCookieName(module string, secure bool) string {
	if secure {
		return "__Secure-can_launch_" + module
	}
	return "can_launch_" + module
}

// maxBindings bounds how many candidate nonces of one request are tried.
const maxBindings = 4

// LaunchBinding reads the browser nonce of a launch from a request to this module's UI, "" when
// there is none. The result is opaque: pass it to PeekTicket or RedeemTicket as it is.
//
// secure says whether this UI is served over https. If it is, only the __Secure- cookie is read: the
// unprefixed name (plain-http development) is ignored, because anything on http could plant it. If
// it is not, both names are read.
//
// Several cookies of one name can arrive (set for different scopes, possibly planted by another host
// of the company domain), and a module cannot tell which is genuine. So all of them that are shaped
// like a nonce (up to maxBindings, separated by spaces) are returned, and PeekTicket and
// RedeemTicket try each until can-core accepts one; a wrong one does not spend the ticket.
func LaunchBinding(r *http.Request, module string, secure bool) string {
	names := []string{LaunchCookieName(module, true)}
	if !secure {
		names = append(names, LaunchCookieName(module, false))
	}
	var out []string
	for _, n := range names {
		for _, c := range r.CookiesNamed(n) {
			if len(out) < maxBindings && BindingShaped(c.Value) && !slices.Contains(out, c.Value) {
				out = append(out, c.Value)
			}
		}
	}
	return strings.Join(out, " ")
}

// LaunchSession is can-core's answer when a module peeks at or redeems a launch ticket or
// revalidates the session handle it got (specs/036-module-ui-sso). Its zero value is "inactive".
type LaunchSession struct {
	Active  bool   `json:"active"`
	Subject string `json:"subject"`
	// Name is a display name; never an email address or a group.
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	// ReturnPath is the landing path chosen at launch, relative to the module's UI (redeem only).
	ReturnPath string `json:"return_path"`
	// Session is the opaque handle to revalidate with; keep it in memory only (redeem only).
	Session string `json:"session"`
	// ExpiresAt is the latest the module session may live (redeem only): end it then, whatever the
	// module's own timeouts say.
	ExpiresAt time.Time `json:"session_expires_at"`
	// AuthenticatedAt is when the person last authenticated to the admin UI (redeem and
	// revalidate); zero when can-core does not know.
	AuthenticatedAt time.Time `json:"authenticated_at"`
}

// SessionAPI is what a module's UI needs for single sign-on; tests substitute a fake.
type SessionAPI interface {
	PeekTicket(ctx context.Context, ticket, binding string) (LaunchSession, error)
	RedeemTicket(ctx context.Context, ticket, binding string) (LaunchSession, error)
	RevalidateSession(ctx context.Context, session string) (LaunchSession, error)
}

func (c *Client) launch(ctx context.Context, path string, body map[string]string) (LaunchSession, error) {
	status, b, err := c.do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return LaunchSession{}, err
	}
	if status != http.StatusOK {
		return LaunchSession{}, fmt.Errorf("%w: HTTP %d", ErrUnavailable, status)
	}
	var s LaunchSession
	if err := json.Unmarshal(b, &s); err != nil {
		return LaunchSession{}, errors.New("launch answer is not valid JSON")
	}
	if !s.Active || s.Subject == "" {
		return LaunchSession{}, nil
	}
	return s, nil
}

// PeekTicket names the account a ticket would sign in, without consuming it (POST
// /v1/tickets/peek). binding is the browser's nonce (LaunchBinding). The answer carries only
// Active, Subject and Name.
func (c *Client) PeekTicket(ctx context.Context, ticket, binding string) (LaunchSession, error) {
	return c.launchBound(ctx, "/v1/tickets/peek", ticket, binding)
}

// launchBound tries the candidate nonces of LaunchBinding in turn until can-core accepts one.
func (c *Client) launchBound(ctx context.Context, path, ticket, binding string) (LaunchSession, error) {
	if !TicketShaped(ticket) {
		return LaunchSession{}, nil
	}
	for i, b := range strings.Fields(binding) {
		if i >= maxBindings || !BindingShaped(b) {
			break
		}
		s, err := c.launch(ctx, path, map[string]string{"ticket": ticket, "binding": b})
		if err != nil || s.Active {
			return s, err
		}
	}
	return LaunchSession{}, nil
}

// RedeemTicket exchanges a launch ticket for the person (POST /v1/tickets/redeem). The audience is
// this module: can-core takes it from the module's own credential. binding is the nonce cookie of
// the browser that presented the ticket; without the right one nothing is redeemed. An unknown,
// used, expired or foreign ticket is the zero LaunchSession, not an error; an error means can-core
// could not answer.
func (c *Client) RedeemTicket(ctx context.Context, ticket, binding string) (LaunchSession, error) {
	s, err := c.launchBound(ctx, "/v1/tickets/redeem", ticket, binding)
	if len(s.Permissions) == 0 {
		return LaunchSession{}, err
	}
	return s, err
}

// RevalidateSession asks whether the person behind a session handle is still allowed
// (POST /v1/tickets/revalidate) and with which permissions now.
func (c *Client) RevalidateSession(ctx context.Context, session string) (LaunchSession, error) {
	if !shaped(session, HandlePrefix) {
		return LaunchSession{}, nil
	}
	s, err := c.launch(ctx, "/v1/tickets/revalidate", map[string]string{"session": session})
	if len(s.Permissions) == 0 {
		return LaunchSession{}, err
	}
	return s, err
}

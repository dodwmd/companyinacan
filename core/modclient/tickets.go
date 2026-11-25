package modclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// LaunchBinding reads the browser nonce of a launch from a request to this module's UI, "" when
// there is none. secure says whether the UI itself is served over https; only then is the
// unprefixed name (plain-http development) ignored, so it cannot be planted over http.
func LaunchBinding(r *http.Request, module string, secure bool) string {
	names := []string{LaunchCookieName(module, true)}
	if !secure {
		names = append(names, LaunchCookieName(module, false))
	}
	for _, n := range names {
		// Several cookies of one name can arrive (set for different scopes): take the first that is
		// shaped like a nonce. can-core checks which is genuine.
		for _, c := range r.CookiesNamed(n) {
			if BindingShaped(c.Value) {
				return c.Value
			}
		}
	}
	return ""
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
	Session   string    `json:"session"`
	ExpiresAt time.Time `json:"session_expires_at"`
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
	if !TicketShaped(ticket) || !BindingShaped(binding) {
		return LaunchSession{}, nil
	}
	return c.launch(ctx, "/v1/tickets/peek", map[string]string{"ticket": ticket, "binding": binding})
}

// RedeemTicket exchanges a launch ticket for the person (POST /v1/tickets/redeem). The audience is
// this module: can-core takes it from the module's own credential. binding is the nonce cookie of
// the browser that presented the ticket; without the right one nothing is redeemed. An unknown,
// used, expired or foreign ticket is the zero LaunchSession, not an error; an error means can-core
// could not answer.
func (c *Client) RedeemTicket(ctx context.Context, ticket, binding string) (LaunchSession, error) {
	if !TicketShaped(ticket) || !BindingShaped(binding) {
		return LaunchSession{}, nil
	}
	s, err := c.launch(ctx, "/v1/tickets/redeem", map[string]string{"ticket": ticket, "binding": binding})
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

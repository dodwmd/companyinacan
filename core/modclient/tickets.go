package modclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Ticket and handle shapes (core/server/ticket): refused locally, so junk is never sent to can-core.
const (
	TicketPrefix = "cant_"
	HandlePrefix = "cans_"
	secretLen    = 43
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

// LaunchSession is can-core's answer when a module redeems a launch ticket or revalidates the
// session handle it got (specs/036-module-ui-sso). Its zero value is "inactive".
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
	RedeemTicket(ctx context.Context, ticket string) (LaunchSession, error)
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
	if !s.Active || s.Subject == "" || len(s.Permissions) == 0 {
		return LaunchSession{}, nil
	}
	return s, nil
}

// RedeemTicket exchanges a launch ticket for the person (POST /v1/tickets/redeem). The audience is
// this module: can-core takes it from the module's own credential. An unknown, used, expired or
// foreign ticket is the zero LaunchSession, not an error; an error means can-core could not answer.
func (c *Client) RedeemTicket(ctx context.Context, ticket string) (LaunchSession, error) {
	if !TicketShaped(ticket) {
		return LaunchSession{}, nil
	}
	return c.launch(ctx, "/v1/tickets/redeem", map[string]string{"ticket": ticket})
}

// RevalidateSession asks whether the person behind a session handle is still allowed
// (POST /v1/tickets/revalidate) and with which permissions now.
func (c *Client) RevalidateSession(ctx context.Context, session string) (LaunchSession, error) {
	if !shaped(session, HandlePrefix) {
		return LaunchSession{}, nil
	}
	return c.launch(ctx, "/v1/tickets/revalidate", map[string]string{"session": session})
}

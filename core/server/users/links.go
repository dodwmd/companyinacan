package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// One-time set-up links: how a new person gets a password and an authenticator, and how either is
// replaced. The link is the only secret; it is stored hashed, works once, expires, and stops working
// when the administrator who made it could no longer make it.

// Link lifetimes and limits.
const (
	InviteTTL = 72 * time.Hour
	ResetTTL  = 24 * time.Hour
	// maxLinkFailures wrong current passwords delete an mfa link. Only the holder of the link can
	// make them, so nobody else is affected.
	maxLinkFailures = 5
	tokenLen        = 43
	// SetupPath is the public page a link opens.
	SetupPath = "/set-password"
)

// Errors of using a link.
var (
	// ErrBadLink is the one answer for an unknown, used, expired, replaced or withdrawn link.
	ErrBadLink     = errors.New("the link is not valid")
	ErrBadCode     = errors.New("the authentication code does not match")
	ErrBadPassword = errors.New("the current password is wrong")
)

// Mailer sends a set-up link.
type Mailer interface {
	// Ready reports whether mail can be sent at all.
	Ready() bool
	// Send queues one message to an account's address. forUser is "user:<name>".
	Send(ctx context.Context, to, forUser, subject, text string) error
}

// EmailSender is the Mailer over can-core's email service: the company's recipient rules apply.
type EmailSender struct{ S *email.Service }

// Ready implements Mailer.
func (m EmailSender) Ready() bool {
	_, problem := m.S.RelayStatus()
	return problem == ""
}

// Send implements Mailer.
func (m EmailSender) Send(ctx context.Context, to, forUser, subject, text string) error {
	_, _, err := m.S.Send(ctx, email.Request{Source: email.SystemSource, Notice: true, ForUser: forUser, To: []string{to}, Subject: subject, Text: text})
	return err
}

// Link is the outcome of making a link. Path and URL are empty when it was mailed: the
// administrator then never sees it.
type Link struct {
	Username  string    `json:"username"`
	Purpose   string    `json:"purpose"`
	Emailed   bool      `json:"emailed"`
	Email     string    `json:"email,omitempty"`
	Path      string    `json:"path,omitempty"`
	URL       string    `json:"url,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// MailProblem: mailing was asked for and was not possible, so the link is here instead.
	MailProblem bool `json:"mail_problem,omitempty"`
}

func linkHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func newLinkToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func cleanEmail(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v != "" && !config.ValidAddress(v) {
		return "", fmt.Errorf("%w: that is not an email address", ErrInvalid)
	}
	return v, nil
}

func (s *Service) newLink(userID int64, actor, purpose string) (string, store.SetupLink) {
	token := newLinkToken()
	ttl := ResetTTL
	if purpose == store.LinkInvite {
		ttl = InviteTTL
	}
	now := s.now()
	return token, store.SetupLink{UserID: userID, TokenHash: linkHash(token), TOTPSecret: auth.NewTOTPSecret(), Purpose: purpose, CreatedBy: actor, CreatedAt: now, ExpiresAt: now.Add(ttl)}
}

// changeEmail stores a new address inside tx and records that it changed (never the address).
func (s *Service) changeEmail(ctx context.Context, tx *sql.Tx, a Actor, userID int64, subject, address string) error {
	if err := store.SetEmail(ctx, tx, userID, address, a.User.Username); err != nil {
		return err
	}
	_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.email.change", Target: subject})
	return err
}

// Create makes a local account with no role and a one-time link to set it up.
func (s *Service) Create(ctx context.Context, a Actor, username, address string, send bool) (Link, error) {
	if err := s.sensitive(ctx, a); err != nil {
		return Link{}, err
	}
	username = strings.TrimSpace(username)
	if !usernameRe.MatchString(username) {
		return Link{}, fmt.Errorf("%w: a user name is 1 to 64 letters, digits and . _ @ -, starting with a letter or digit", ErrInvalid)
	}
	address, err := cleanEmail(address)
	if err != nil {
		return Link{}, err
	}
	var token string
	var rec store.SetupLink
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		id, err := s.DB.InsertPendingAccount(ctx, tx, username)
		if err != nil {
			return err
		}
		if address != "" {
			if err := s.changeEmail(ctx, tx, a, id, "user:"+username, address); err != nil {
				return err
			}
		}
		// An assignment written for this name before the account existed must not become real now.
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE subject=?`, "user:"+username).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: roles are already recorded for that name; remove them first", ErrExists)
		}
		token, rec = s.newLink(id, a.User.Username, store.LinkInvite)
		if err := store.PutSetupLink(ctx, tx, rec); err != nil {
			return err
		}
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.create", Target: "user:" + username})
		return err
	})
	switch {
	case errors.Is(err, store.ErrExists):
		return Link{}, ErrExists
	case errors.Is(err, store.ErrDeletedName), errors.Is(err, store.ErrReservedName):
		return Link{}, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	case err != nil:
		return Link{}, err
	}
	return s.deliver(ctx, username, address, token, rec, send), nil
}

// SendLink replaces an account's credentials with a new one-time link. purpose "mfa" resets the
// authentication code only (the password is asked for when the link is used); "reset" resets the
// password and the code. An account that never set a password gets a new invitation whichever is
// asked. address, when not empty, replaces the stored one. The account's sessions and tokens end.
func (s *Service) SendLink(ctx context.Context, a Actor, username, purpose, address string, send bool) (Link, error) {
	if err := s.sensitive(ctx, a); err != nil {
		return Link{}, err
	}
	if purpose != store.LinkMFA && purpose != store.LinkReset && purpose != store.LinkInvite {
		return Link{}, fmt.Errorf("%w: unknown kind of link", ErrInvalid)
	}
	address, err := cleanEmail(address)
	if err != nil {
		return Link{}, err
	}
	if username == a.User.Username {
		return Link{}, ErrSelf
	}
	acc, err := s.account(ctx, username)
	if err != nil {
		return Link{}, err
	}
	subject := "user:" + acc.Username
	// Whoever holds the link controls the account, so it must give its maker nothing new.
	if ok, err := s.covers(ctx, a.User.Username, subject); err != nil {
		return Link{}, err
	} else if !ok {
		return Link{}, fmt.Errorf("%w: the account holds permissions you do not hold", ErrForbidden)
	}
	var token string
	var rec store.SetupLink
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		acc, err := store.AccountTx(ctx, tx, acc.Username)
		if err != nil {
			return err
		}
		if err := ownerGuard(ctx, tx, a, subject); err != nil {
			return err
		}
		if acc.AuthSource != store.SourceLocal {
			return fmt.Errorf("%w: a single sign-on user has no password here", ErrInvalid)
		}
		if !acc.DisabledAt.IsZero() {
			return fmt.Errorf("%w: enable the account first", ErrInvalid)
		}
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM admin_users WHERE id=?`, acc.ID).Scan(&hash); err != nil {
			return err
		}
		kind := purpose
		switch {
		case hash == store.UnusedPassword:
			kind = store.LinkInvite
		case kind == store.LinkInvite:
			return fmt.Errorf("%w: the account is already set up", ErrInvalid)
		}
		// Two owners resetting each other must not leave nobody who can sign in.
		if kind != store.LinkInvite {
			if err := keepsAnOwner(ctx, tx, subject); err != nil {
				return err
			}
		}
		// The delegations an account received are power its next holder must not pick up.
		if err := store.EndPending(ctx, tx, subject); err != nil {
			return err
		}
		if err := store.ClearCredentials(ctx, tx, acc.ID, kind == store.LinkMFA); err != nil {
			return err
		}
		if err := store.EndAccess(ctx, tx, subject, s.now(), false); err != nil {
			return err
		}
		switch {
		case address == "":
			address = acc.Email
		case address != acc.Email:
			if err := s.changeEmail(ctx, tx, a, acc.ID, subject, address); err != nil {
				return err
			}
		}
		token, rec = s.newLink(acc.ID, a.User.Username, kind)
		if err := store.PutSetupLink(ctx, tx, rec); err != nil {
			return err
		}
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: a.User.Username, Action: "users.link", Target: subject, Detail: map[string]any{"purpose": kind}})
		return err
	})
	if s.RBAC != nil {
		s.RBAC.Invalidate()
	}
	if err != nil {
		return Link{}, mapErr(err)
	}
	return s.deliver(ctx, acc.Username, address, token, rec, send), nil
}

// deliver mails the link when that is asked for and possible; otherwise the caller shows it once.
func (s *Service) deliver(ctx context.Context, username, address, token string, rec store.SetupLink, send bool) Link {
	out := Link{Username: username, Purpose: rec.Purpose, ExpiresAt: rec.ExpiresAt, Path: SetupPath + "?token=" + url.QueryEscape(token)}
	if s.BaseURL != "" {
		out.URL = strings.TrimRight(s.BaseURL, "/") + out.Path
	}
	if !send || address == "" || s.Mail == nil || !s.Mail.Ready() {
		return out
	}
	if out.URL == "" {
		out.MailProblem = true
		return out
	}
	text := fmt.Sprintf("A link to set up your sign-in (user name %s) was made for you.\n\n%s\n\nIt works once, until %s UTC. If you did not expect it, ignore this message and tell your administrator.\n",
		username, out.URL, rec.ExpiresAt.UTC().Format("2006-01-02 15:04"))
	if err := s.Mail.Send(ctx, address, "user:"+username, "Set up your sign-in", text); err != nil {
		if s.Log != nil {
			s.Log.Warn("users: the set-up link could not be mailed", "error", err)
		}
		out.MailProblem = true
		return out
	}
	out.Emailed, out.Email, out.Path, out.URL = true, address, "", ""
	return out
}

// Setup is what the page behind a link needs.
type Setup struct {
	Username string
	// Purpose: invite and reset ask for a new password; mfa asks for the current one.
	Purpose string
	Secret  string
}

// open checks a link: known, unexpired, the account enabled, and its maker still able to make it.
func (s *Service) open(ctx context.Context, token string) (store.SetupLink, store.AdminUser, error) {
	var none store.SetupLink
	if len(token) != tokenLen {
		return none, store.AdminUser{}, ErrBadLink
	}
	l, u, err := s.DB.SetupLinkByHash(ctx, linkHash(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return none, u, ErrBadLink
	}
	if err != nil {
		return none, u, err
	}
	maker, err := s.DB.AdminByUsername(ctx, l.CreatedBy)
	if errors.Is(err, store.ErrNotFound) || (err == nil && s.DB.AdminDisabled(ctx, maker.ID)) {
		return none, u, ErrBadLink
	}
	if err != nil {
		return none, u, err
	}
	if s.RBAC != nil {
		may, err := s.RBAC.Check(ctx, "user:"+maker.Username, rbac.PermManage, "")
		if err != nil {
			return none, u, err
		}
		covers, err := s.RBAC.Covers(ctx, maker.Username, "user:"+u.Username)
		if err != nil {
			return none, u, err
		}
		if !may || !covers {
			return none, u, ErrBadLink
		}
	}
	return l, u, nil
}

// OpenLink returns what the page behind a link shows. It consumes nothing.
func (s *Service) OpenLink(ctx context.Context, token string) (Setup, error) {
	l, u, err := s.open(ctx, token)
	if err != nil {
		return Setup{}, err
	}
	return Setup{Username: u.Username, Purpose: l.Purpose, Secret: l.TOTPSecret}, nil
}

// UseLink sets the account's credentials and consumes the link. password is the new password, or for
// an mfa link the current one. code comes from the authenticator the shown secret was added to; it
// is checked first, so a wrong one costs no password hashing. Signing in is a separate step.
func (s *Service) UseLink(ctx context.Context, token, password, code, ip string) error {
	l, u, err := s.open(ctx, token)
	if err != nil {
		return err
	}
	step, ok := auth.ValidateTOTP(l.TOTPSecret, code, s.now())
	if !ok {
		return ErrBadCode
	}
	hash := u.PasswordHash
	if l.Purpose == store.LinkMFA {
		ok, err := s.Auth.CheckPassword(ctx, password, u.PasswordHash)
		if err != nil {
			return err
		}
		if !ok {
			burned, err := s.DB.FailSetupLink(ctx, l.TokenHash, maxLinkFailures)
			if err != nil {
				return err
			}
			if burned {
				return ErrBadLink
			}
			return ErrBadPassword
		}
	} else {
		if len(password) < auth.MinPasswordLen || len(password) > auth.MaxPasswordLen {
			return auth.ErrWeakPassword
		}
		if hash, err = s.Auth.NewPasswordHash(ctx, password); err != nil {
			return err
		}
	}
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if err := store.ConsumeSetupLink(ctx, tx, l.TokenHash, hash, step, s.now()); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: u.Username, Action: "users.setup", Target: "user:" + u.Username, Detail: map[string]any{"purpose": l.Purpose, "ip": ip}})
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return ErrBadLink
	}
	if err != nil {
		return err
	}
	// Failed guesses under the old credentials must not slow or refuse the new, valid ones.
	if _, err := s.DB.ClearLoginAttempts(ctx, u.Username); err != nil && s.Log != nil {
		s.Log.Warn("users: clearing sign-in failure counters failed", "error", err)
	}
	return nil
}

package email

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Statuses of a message.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusDead    = "dead"
)

// Retry and retention policy.
const (
	MaxAttempts      = 8
	maxPendingAge    = 72 * time.Hour      // older unsent mail is given up on
	deadContentTTL   = 7 * 24 * time.Hour  // dead letters keep their content (and can be requeued) this long
	rowTTL           = 30 * 24 * time.Hour // finished rows are deleted after this
	maxPendingSource = 500                 // queued messages one source may have waiting
	batchSize        = 20
)

// Errors of Send, Get and Requeue.
var (
	ErrInvalid    = errors.New("invalid message")
	ErrNotAllowed = errors.New("this sender may not send that message")
	ErrConflict   = errors.New("source_id was already used for a different message")
	ErrNotFound   = errors.New("no such message")
)

// QuotaError says a sending limit is reached; nothing was queued.
type QuotaError struct {
	Limit      string
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string { return "email quota reached: " + e.Limit }

var messages = obs.Default.Counter("can_email_messages_total", "Outbox messages by result.", "result")

// Request is one thing to send.
type Request struct {
	Source   string // module id, or SystemSource
	To       []string
	ReplyTo  string
	Subject  string
	Text     string
	SourceID string // idempotency key of the sender; empty = none

	// Notice marks the notification channel: Source is the module that caused it (its switch and
	// quota apply) or SystemSource, and the recipient must be allowed for ForUser. Modules cannot
	// set it.
	Notice  bool
	ForUser string
}

// errNoticeAddress is what a person is told when their notification address is not accepted.
var errNoticeAddress = fmt.Errorf("%w: notification mail only goes to the company domain, email.notification_recipients or your verified single sign-on address", ErrNotAllowed)

// noticeAllowed reports whether notification mail for user may go to addr. An address a person
// typed is not verified, so it has to be one the operator vouches for or the identity provider did.
// Must not be called inside a transaction (single database connection).
func (s *Service) noticeAllowed(ctx context.Context, p Policy, user, addr string) (bool, error) {
	if p.NoticeAllowed(addr) {
		return true, nil
	}
	name, ok := strings.CutPrefix(user, "user:")
	if !ok || !p.ssoVerified {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sso_identities i JOIN admin_users u ON u.id=i.user_id WHERE u.username=? AND lower(i.email)=lower(?) AND i.email_verified=1`, name, addr).Scan(&n)
	return n > 0, err
}

// Service is the outbox.
type Service struct {
	db        *store.DB
	audit     *audit.Log
	source    Source
	transport Transport
	log       *slog.Logger
	wake      chan struct{}
	Now       func() time.Time
	// After a session failure no delivery is tried before pausedUntil (unix seconds). failures
	// belongs to the single worker.
	pausedUntil atomic.Int64
	failures    int
	stalled     map[string]bool // messages left without an answer since the relay last gave one
}

// New returns the service. transport nil means SMTP with its safe defaults.
func New(db *store.DB, al *audit.Log, source Source, transport Transport, log *slog.Logger) *Service {
	if transport == nil {
		transport = SMTP{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, audit: al, source: source, transport: transport, log: log, wake: make(chan struct{}, 1), Now: time.Now}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "eml_" + hex.EncodeToString(b)
}

// mask keeps enough of an address to recognise it: the first character and the domain.
func mask(a string) string {
	local, domain := splitAddr(a)
	_, n := utf8.DecodeRuneInString(local)
	return local[:n] + "***@" + domain
}

func validate(r Request) (Request, error) {
	bad := func(what string) (Request, error) { return r, fmt.Errorf("%w: %s", ErrInvalid, what) }
	if len(r.To) == 0 || len(r.To) > MaxRecipients {
		return bad(fmt.Sprintf("to needs 1 to %d addresses", MaxRecipients))
	}
	seen := map[string]bool{}
	var to []string
	for _, a := range r.To {
		if !config.ValidAddress(a) {
			return bad("to must be plain email addresses")
		}
		if k := strings.ToLower(a); !seen[k] {
			seen[k] = true
			to = append(to, a)
		}
	}
	r.To = to
	if r.ReplyTo != "" && !config.ValidAddress(r.ReplyTo) {
		return bad("reply_to must be a plain email address")
	}
	if !validSubject(r.Subject) {
		return bad(fmt.Sprintf("subject must be one line of at most %d bytes", MaxSubjectBytes))
	}
	if !validText(r.Text) {
		return bad(fmt.Sprintf("text must be plain text of at most %d bytes", MaxTextBytes))
	}
	if r.SourceID != "" && !events.SourceIDRe.MatchString(r.SourceID) {
		return bad("source_id must be 1 to 128 characters of letters, digits and _ . : -")
	}
	return r, nil
}

func requestHash(r Request) string {
	b, _ := json.Marshal([]any{r.To, r.ReplyTo, r.Subject, r.Text})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Send validates the request, checks the source's rules and quotas and queues one message per
// recipient. The rows and their audit entries commit together, before Send returns. With a
// source_id already used, it returns the first request's ids and duplicate true.
func (s *Service) Send(ctx context.Context, r Request) (ids []string, duplicate bool, err error) {
	if r, err = validate(r); err != nil {
		return nil, false, err
	}
	p, err := s.source.Load()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrNotConfigured, err.Error())
	}
	rules := p.Rules(r.Source)
	if !rules.Enabled {
		messages.With("refused").Inc()
		return nil, false, fmt.Errorf("%w: email is turned off for this module", ErrNotAllowed)
	}
	if r.Notice {
		for _, a := range r.To {
			ok, err := s.noticeAllowed(ctx, p, r.ForUser, a)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				messages.With("refused").Inc()
				return nil, false, errNoticeAddress
			}
		}
	} else {
		if r.Source == SystemSource {
			return nil, false, fmt.Errorf("%w: can-core's own mail is notification mail", ErrNotAllowed)
		}
		for _, a := range r.To {
			if !config.RecipientAllowed(a, rules.Recipients) {
				messages.With("refused").Inc()
				s.log.Warn("email refused: recipient not on the module's allow-list", "module", r.Source)
				return nil, false, fmt.Errorf("%w: a recipient is not on the allow-list of this module (company.yaml email.modules)", ErrNotAllowed)
			}
		}
	}
	if p.RelayErr != nil {
		return nil, false, p.RelayErr
	}
	hash := ""
	if r.SourceID != "" {
		hash = requestHash(r)
	}
	now := s.Now()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		ids, duplicate = nil, false
		if r.SourceID != "" {
			rows, err := tx.QueryContext(ctx, `SELECT id, request_hash FROM email_outbox WHERE source=? AND source_id=? ORDER BY seq`, r.Source, r.SourceID)
			if err != nil {
				return err
			}
			var first string
			for rows.Next() {
				var id, h string
				if err := rows.Scan(&id, &h); err != nil {
					_ = rows.Close()
					return err
				}
				ids, first = append(ids, id), h
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(ids) > 0 {
				if first != hash {
					return ErrConflict
				}
				duplicate = true
				return nil
			}
		}
		for _, q := range []struct {
			name   string
			window time.Duration
			limit  int
		}{{"per hour", time.Hour, rules.PerHour}, {"per day", 24 * time.Hour, rules.PerDay}} {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_outbox WHERE source=? AND created_at>?`, r.Source, now.Add(-q.window).Unix()).Scan(&n); err != nil {
				return err
			}
			if n+len(r.To) > q.limit {
				return &QuotaError{Limit: q.name, RetryAfter: q.window / 12}
			}
		}
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_outbox WHERE source=? AND status='pending'`, r.Source).Scan(&waiting); err != nil {
			return err
		}
		if waiting+len(r.To) > maxPendingSource {
			return &QuotaError{Limit: "messages waiting to be sent", RetryAfter: 10 * time.Minute}
		}
		for i, a := range r.To {
			id := newID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO email_outbox(id, source, notice, for_user, source_id, seq, request_hash, recipient, recipient_mask, reply_to, subject, body, status, next_attempt_at, created_at, queued_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)`,
				id, r.Source, r.Notice, r.ForUser, r.SourceID, i, hash, a, mask(a), r.ReplyTo, r.Subject, r.Text, now.Unix(), now.Unix(), now.Unix()); err != nil {
				return err
			}
			if _, err := s.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor(r.Source), Action: "email.queue", Target: id, Detail: map[string]any{"source": r.Source, "bytes": len(r.Text)}}); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			messages.With("refused").Inc()
			s.log.Warn("email refused: quota reached", "source", r.Source, "limit", qe.Limit)
		}
		return nil, false, err
	}
	if !duplicate {
		messages.With("queued").Add(float64(len(ids)))
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return ids, duplicate, nil
}

func actor(source string) string {
	if source == SystemSource {
		return "can-core"
	}
	return "module:" + source
}

// Message is the status of one queued message. It never carries the address, subject or body.
type Message struct {
	ID          string
	Source      string
	Recipient   string // masked
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	NextAttempt time.Time
	FinishedAt  time.Time // zero while pending
	Requeueable bool
}

const messageColumns = `id, source, recipient_mask, status, attempts, last_error, created_at, next_attempt_at, finished_at, recipient<>''`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var created, next, finished int64
	var content bool
	if err := row.Scan(&m.ID, &m.Source, &m.Recipient, &m.Status, &m.Attempts, &m.LastError, &created, &next, &finished, &content); err != nil {
		return m, err
	}
	m.CreatedAt, m.NextAttempt = time.Unix(created, 0), time.Unix(next, 0)
	if finished != 0 {
		m.FinishedAt = time.Unix(finished, 0)
	}
	m.Requeueable = m.Status == StatusDead && content
	return m, nil
}

// Get returns one of the source's own messages. Another source's message and a missing id are the
// same ErrNotFound.
func (s *Service) Get(ctx context.Context, source, id string) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM email_outbox WHERE id=? AND source=?`, id, source))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// List returns the newest messages (at most 500) for the delivery page.
func (s *Service) List(ctx context.Context, limit int) ([]Message, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM email_outbox ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Requeue gives a dead message that still has its content a fresh set of attempts.
func (s *Service) Requeue(ctx context.Context, by, id string) error {
	now := s.Now().Unix()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE email_outbox SET status='pending', attempts=0, last_error='', next_attempt_at=?, queued_at=?, finished_at=0
			WHERE id=? AND status='dead' AND recipient<>''`, now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: by, Action: "email.requeue", Target: id})
		return err
	})
	if err == nil {
		s.pausedUntil.Store(0) // an operator retrying has usually just fixed the relay
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return err
}

// RelayStatus describes the configured relay without any credential. problem is empty when mail can
// be sent.
func (s *Service) RelayStatus() (r Relay, problem string) {
	p, err := s.source.Load()
	if err != nil {
		return Relay{}, err.Error()
	}
	if p.RelayErr != nil {
		return Relay{}, strings.TrimPrefix(p.RelayErr.Error(), ErrNotConfigured.Error()+": ")
	}
	return Relay{Host: p.Relay.Host, Port: p.Relay.Port, TLS: p.Relay.TLS, From: p.Relay.From}, ""
}

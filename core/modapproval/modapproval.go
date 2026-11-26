// Package modapproval is a module's side of can-core's approval engine for records that wait for a
// decision (timesheets, expenses, purchase orders, time off; specs/070-approvals-inbox).
//
// The module keeps one row per waiting record in its own database (table approval_queue), written in
// the same transaction as the record. A Pump turns the rows into requests in can-core: when can-core
// cannot be reached the record simply stays waiting and nothing can approve it, and a large backlog is
// sent a little at a time, below can-core's per-module limit. The decision comes back as the signed
// approval.decided.v1 event, matched by the row's ref, which is also the idempotency key: a lost
// answer loses neither the request nor its decision.
package modapproval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Row statuses.
const (
	Queued   = "queued"   // not yet in can-core
	Pending  = "pending"  // waiting in can-core's inbox
	Refused  = "refused"  // can-core would not take it (no approver, no policy); tried again later
	Approved = "approved" // decided, but the module could not apply it (see Note)
	Rejected = "rejected" // decided, but the module could not apply it, or (purchase orders) turned down
	cancel   = "cancel"   // withdrawn: still to be cancelled in can-core
)

// The most requests a Pump opens, below can-core's limits for one module (120 an hour, 600 a day) so
// that a backlog leaves room for the approvals the module asks for directly.
const (
	DefaultMaxHour = 90
	DefaultMaxDay  = 500
)

const (
	retryRefused = time.Hour
	defaultPause = 5 * time.Minute
	batch        = 50
)

// DB is *sql.DB or *sql.Tx.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Row is one record waiting for a decision. Kind is the approval type declared in module.yaml.
type Row struct {
	Ref        string
	Kind       string
	ItemID     string
	Version    int64
	ApprovalID string
	Status     string
	Requester  string // the person who asked, as the module knows them
	Manager    string
	Note       string
}

const cols = `ref, kind, item_id, version, approval_id, status, requester, manager, note`

func scan(s interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := s.Scan(&r.Ref, &r.Kind, &r.ItemID, &r.Version, &r.ApprovalID, &r.Status, &r.Requester, &r.Manager, &r.Note)
	return r, err
}

func one(row *sql.Row) (Row, bool, error) {
	r, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, nil
	}
	return r, err == nil, err
}

// Get returns the open row of a record.
func Get(ctx context.Context, q DB, kind, itemID string) (Row, bool, error) {
	return one(q.QueryRowContext(ctx, `SELECT `+cols+` FROM approval_queue WHERE kind = ? AND item_id = ? AND status <> 'cancel'`, kind, itemID))
}

// ByRef returns the open row with this ref.
func ByRef(ctx context.Context, q DB, ref string) (Row, bool, error) {
	return one(q.QueryRowContext(ctx, `SELECT `+cols+` FROM approval_queue WHERE ref = ? AND status <> 'cancel'`, ref))
}

// Enqueue records that a record waits for a decision, replacing (and withdrawing) any earlier row. It
// returns the ref. Call it in the transaction that makes the record wait.
func Enqueue(ctx context.Context, q DB, kind, itemID string, version int64, requester, manager string, now time.Time) (string, error) {
	if err := Withdraw(ctx, q, kind, itemID); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ref := kind + ":" + itemID + ":" + hex.EncodeToString(b[:])
	_, err := q.ExecContext(ctx, `INSERT INTO approval_queue (ref, kind, item_id, version, status, requester, manager, queued_at) VALUES (?,?,?,?,?,?,?,?)`,
		ref, kind, itemID, version, Queued, requester, manager, now.Unix())
	return ref, err
}

// Withdraw closes a record's row: a request waiting in can-core is cancelled by the next Pump pass.
// Nothing that identifies a person is kept.
func Withdraw(ctx context.Context, q DB, kind, itemID string) error {
	if _, err := q.ExecContext(ctx, `UPDATE approval_queue SET status = 'cancel', requester = '', manager = '', note = '' WHERE kind = ? AND item_id = ? AND status = 'pending' AND approval_id <> ''`, kind, itemID); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DELETE FROM approval_queue WHERE kind = ? AND item_id = ? AND status <> 'cancel'`, kind, itemID)
	return err
}

// Drop forgets a row.
func Drop(ctx context.Context, q DB, ref string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM approval_queue WHERE ref = ?`, ref)
	return err
}

// Settle records an outcome the module has not applied to the record.
func Settle(ctx context.Context, q DB, ref, status, note string) error {
	_, err := q.ExecContext(ctx, `UPDATE approval_queue SET status = ?, note = ? WHERE ref = ? AND status <> 'cancel'`, status, clip(note), ref)
	return err
}

func clip(s string) string {
	if r := []rune(s); len(r) > 300 {
		return string(r[:300])
	}
	return s
}

var (
	userRe  = regexp.MustCompile(`^user:[A-Za-z0-9_.@-]{1,64}$`)
	partyRe = regexp.MustCompile(`^party:[A-Za-z0-9_.-]{1,64}$`)
	plainRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// Subject names a module's requester to can-core. A can-core account (user:<name>) stays as it is; a
// subject of the module's own tokens becomes party:<it>, which no account can be mistaken for. The
// module keeps its own separation-of-duties check on the original.
func Subject(s string) string {
	switch {
	case userRe.MatchString(s), partyRe.MatchString(s):
		return s
	case plainRe.MatchString(s):
		return "party:" + s
	}
	h := sha256.Sum256([]byte(s))
	return "party:h-" + hex.EncodeToString(h[:12])
}

// IsAccount reports whether s names a can-core account.
func IsAccount(s string) bool { return userRe.MatchString(s) }

// Decision is the data of an approval.decided.v1 event.
type Decision struct {
	ApprovalID string   `json:"request_id"`
	Type       string   `json:"type"`
	Ref        string   `json:"ref"`
	Outcome    string   `json:"outcome"` // approved, rejected, cancelled or expired
	Approvers  []string `json:"approvers"`
}

// Decode reads a can-core decision event; ok is false for anything else.
func Decode(ev modclient.Event) (Decision, bool) {
	var d Decision
	if ev.Type != "approval.decided.v1" || ev.Source != "can-core" || json.Unmarshal(ev.Data, &d) != nil || d.Ref == "" || d.ApprovalID == "" {
		return Decision{}, false
	}
	return d, true
}

// Pump sends a module's rows to can-core.
type Pump struct {
	Client modclient.Approvals
	DB     DB
	// Input describes a waiting record (title, amount, payload). false: it no longer waits, so its row
	// is dropped. Type, Requester, Manager, Ref and Key are filled in by the Pump.
	Input func(ctx context.Context, r Row) (modclient.ApprovalInput, bool, error)
	// Reconcile queues the records that wait without a row (an upgrade, a restore).
	Reconcile func(ctx context.Context) error
	Log       *slog.Logger
	Now       func() time.Time

	MaxHour, MaxDay int // 0: the defaults

	mu     sync.Mutex
	sent   []time.Time // requests opened in the last day
	paused time.Time
	once   sync.Once
	kick   chan struct{}
}

func (p *Pump) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pump) channel() chan struct{} {
	p.once.Do(func() { p.kick = make(chan struct{}, 1) })
	return p.kick
}

// Kick asks Run for a pass now.
func (p *Pump) Kick() {
	if p == nil {
		return
	}
	select {
	case p.channel() <- struct{}{}:
	default:
	}
}

// Run calls Sync until ctx ends.
func (p *Pump) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := p.Sync(ctx); err != nil && ctx.Err() == nil && p.Log != nil {
			p.Log.Warn("approvals: cannot reach can-core; records stay waiting", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.channel():
		}
	}
}

// Sync does one pass: queue what has no row, cancel what was withdrawn, send what is due. It stops at
// the first sign that can-core is unavailable (also its rate limit) and returns that error.
func (p *Pump) Sync(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Reconcile != nil {
		if err := p.Reconcile(ctx); err != nil {
			return err
		}
	}
	if p.now().Before(p.paused) {
		return nil
	}
	if err := p.cancels(ctx); err != nil {
		return p.pause(err)
	}
	rows, err := p.due(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !p.budget() {
			return nil
		}
		if err := p.send(ctx, r); err != nil {
			return p.pause(err)
		}
	}
	return nil
}

// SendRef sends one row now and returns it as it is afterwards. A refusal is recorded in the row; an
// unavailable can-core is returned as the error, and the row stays queued.
func (p *Pump) SendRef(ctx context.Context, ref string) (Row, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok, err := ByRef(ctx, p.DB, ref)
	if err != nil || !ok {
		return r, err
	}
	if r.Status == Queued || r.Status == Refused {
		if !p.budget() {
			return r, nil // queued: a later pass sends it
		}
		if err := p.send(ctx, r); err != nil {
			return r, p.pause(err)
		}
		r, _, err = ByRef(ctx, p.DB, ref)
	}
	return r, err
}

func (p *Pump) pause(err error) error {
	var se *modclient.StatusError
	if errors.As(err, &se) && se.Status == 429 {
		d := se.RetryAfter
		if d <= 0 {
			d = defaultPause
		}
		p.paused = p.now().Add(d)
	}
	return err
}

func (p *Pump) budget() bool {
	now := p.now()
	keep := p.sent[:0]
	hour := 0
	for _, t := range p.sent {
		if now.Sub(t) < 24*time.Hour {
			keep = append(keep, t)
			if now.Sub(t) < time.Hour {
				hour++
			}
		}
	}
	p.sent = keep
	maxHour, maxDay := p.MaxHour, p.MaxDay
	if maxHour <= 0 {
		maxHour = DefaultMaxHour
	}
	if maxDay <= 0 {
		maxDay = DefaultMaxDay
	}
	return hour < maxHour && len(p.sent) < maxDay
}

func (p *Pump) list(ctx context.Context, query string, args ...any) ([]Row, error) {
	rows, err := p.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Row
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Pump) due(ctx context.Context) ([]Row, error) {
	return p.list(ctx, `SELECT `+cols+` FROM approval_queue WHERE status = 'queued' OR (status = 'refused' AND retry_at <= ?) ORDER BY queued_at, ref LIMIT ?`, p.now().Unix(), batch)
}

func (p *Pump) cancels(ctx context.Context) error {
	rows, err := p.list(ctx, `SELECT `+cols+` FROM approval_queue WHERE status = 'cancel' ORDER BY queued_at, ref LIMIT ?`, batch)
	if err != nil {
		return err
	}
	for _, r := range rows {
		err := p.Client.CancelApproval(ctx, r.ApprovalID, "withdrawn in the module")
		if errors.Is(err, modclient.ErrUnavailable) {
			return err
		}
		if err := Drop(ctx, p.DB, r.Ref); err != nil {
			return err
		}
	}
	return nil
}

// send opens one request. Only an unavailable can-core (or a database error) is returned.
func (p *Pump) send(ctx context.Context, r Row) error {
	in, waiting, err := p.Input(ctx, r)
	if err != nil {
		return err
	}
	if !waiting {
		return Drop(ctx, p.DB, r.Ref)
	}
	in.Type, in.Ref, in.Key = r.Kind, r.Ref, r.Ref
	in.Requester = Subject(r.Requester)
	if IsAccount(r.Manager) {
		in.Manager = r.Manager
	}
	ap, err := p.Client.CreateApproval(ctx, in)
	switch {
	case errors.Is(err, modclient.ErrUnavailable):
		return err
	case err != nil:
		// A refusal, or an answer that makes no sense: nothing is approved, try again later.
		if p.Log != nil {
			p.Log.Warn("approvals: can-core did not take the request", "ref", r.Ref, "error", err)
		}
		_, derr := p.DB.ExecContext(ctx, `UPDATE approval_queue SET status = 'refused', note = ?, retry_at = ? WHERE ref = ? AND status IN ('queued', 'refused')`,
			clip(err.Error()), p.now().Add(retryRefused).Unix(), r.Ref)
		return derr
	}
	p.sent = append(p.sent, p.now())
	res, err := p.DB.ExecContext(ctx, `UPDATE approval_queue SET status = 'pending', approval_id = ?, note = '' WHERE ref = ? AND status IN ('queued', 'refused')`, ap.ID, r.Ref)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Withdrawn or decided while the request travelled. Cancelling a decided request does nothing.
		if _, still, err := ByRef(ctx, p.DB, r.Ref); err != nil || still {
			return err
		}
		if err := p.Client.CancelApproval(ctx, ap.ID, "withdrawn in the module"); errors.Is(err, modclient.ErrUnavailable) {
			return err
		}
	}
	return nil
}

package forms

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/obs/obssql"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations is the embedded schema history ("migrations/NNN_name.sql"), for the backup and
// restore subcommands (core/modbackup).
func Migrations() fs.FS { return migrationFS }

// Store errors.
var (
	ErrNotFound      = errors.New("lead not found")
	ErrInvalidStatus = errors.New("invalid lead status")
)

// Lead statuses.
var statuses = []string{"new", "contacted", "qualified", "closed", "spam"}

// ValidStatus reports whether s is a known lead status.
func ValidStatus(s string) bool {
	for _, v := range statuses {
		if v == s {
			return true
		}
	}
	return false
}

// Lead is one stored submission.
type Lead struct {
	Seq    int64             `json:"-"`
	ID     string            `json:"id"`
	FormID string            `json:"form_id"`
	Status string            `json:"status"`
	Email  string            `json:"email,omitempty"`
	Name   string            `json:"name,omitempty"`
	Fields map[string]string `json:"fields"`
	Note   string            `json:"note,omitempty"`
	// Consents are the consent checkboxes that were ticked, by field name.
	Consents       []string  `json:"consents,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	RetentionUntil time.Time `json:"retention_until"`
	// Notification is "retrying" or "failed" while the notification email has not got through.
	Notification string `json:"notification_status,omitempty"`

	// emailDue, when set on Insert, queues the notification email in the same transaction.
	emailDue time.Time
}

// Delivery is the recorded outcome of one notification channel for a lead.
type Delivery struct {
	Kind      string    `json:"kind"`
	OK        bool      `json:"ok"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	At        time.Time `json:"at"`
}

// ListFilter selects leads, newest first. BeforeSeq is the paging cursor (exclusive).
type ListFilter struct {
	FormID    string
	Status    string
	Since     time.Time
	BeforeSeq int64
	Limit     int
}

// Store is the SQLite-backed lead store.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore opens (creating if needed) the database at path and applies migrations.
func OpenStore(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "secure_delete(ON)") // purged personal data is overwritten, not left in free pages
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: serialises writes, keeps idle memory minimal.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	obssql.WatchDefault(s.db)
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if m.version <= cur {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, s.now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration (0 when none).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ld_" + hex.EncodeToString(b[:]), nil
}

// Insert stores l, filling ID, Seq, Status (default "new") and timestamps when unset.
func (s *Store) Insert(ctx context.Context, l *Lead) error {
	id, err := newID()
	if err != nil {
		return err
	}
	l.ID = id
	if l.Status == "" {
		l.Status = "new"
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = s.now()
	}
	l.UpdatedAt = l.CreatedAt
	fields, err := json.Marshal(l.Fields)
	if err != nil {
		return err
	}
	consents, err := json.Marshal(append([]string{}, l.Consents...))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO leads (id, form_id, status, email, name, fields, note, consents, created_at, updated_at, retention_until)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.FormID, l.Status, l.Email, l.Name, string(fields), l.Note, string(consents),
		l.CreatedAt.Unix(), l.UpdatedAt.Unix(), l.RetentionUntil.Unix())
	if err != nil {
		return err
	}
	if !l.emailDue.IsZero() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO lead_email (lead_id, next_attempt_at, created_at) VALUES (?, ?, ?)`, l.ID, l.emailDue.Unix(), l.CreatedAt.Unix()); err != nil {
			return err
		}
	}
	if l.Seq, err = res.LastInsertId(); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil {
		leadsCreated.With().Inc()
	}
	return err
}

// leadsCreated counts stored leads. It has no labels: form ids come from operator config but a
// lead's content is personal data and never reaches a metric.
var leadsCreated = obs.Default.Counter("can_forms_leads_created_total", "Leads stored.")

const leadCols = `seq, id, form_id, status, email, name, fields, note, created_at, updated_at, retention_until, consents,
	(SELECT CASE WHEN e.status = 'failed' THEN 'failed' WHEN e.status = 'pending' AND e.attempts > 0 THEN 'retrying' ELSE '' END FROM lead_email e WHERE e.lead_id = leads.id)`

type scanner interface{ Scan(...any) error }

func scanLead(r scanner) (*Lead, error) {
	var l Lead
	var fields, consents string
	var c, u, ret int64
	var notification sql.NullString
	if err := r.Scan(&l.Seq, &l.ID, &l.FormID, &l.Status, &l.Email, &l.Name, &fields, &l.Note, &c, &u, &ret, &consents, &notification); err != nil {
		return nil, err
	}
	l.Notification = notification.String
	if err := json.Unmarshal([]byte(fields), &l.Fields); err != nil {
		return nil, fmt.Errorf("lead %s: corrupt fields: %w", l.ID, err)
	}
	if err := json.Unmarshal([]byte(consents), &l.Consents); err != nil {
		return nil, fmt.Errorf("lead %s: corrupt consents: %w", l.ID, err)
	}
	if len(l.Consents) == 0 {
		l.Consents = nil
	}
	l.CreatedAt, l.UpdatedAt, l.RetentionUntil = time.Unix(c, 0).UTC(), time.Unix(u, 0).UTC(), time.Unix(ret, 0).UTC()
	return &l, nil
}

// Get returns one lead by id.
func (s *Store) Get(ctx context.Context, id string) (*Lead, error) {
	l, err := scanLead(s.db.QueryRowContext(ctx, `SELECT `+leadCols+` FROM leads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// List returns leads newest first, honouring the filter. Limit defaults to 50, max 1000.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Lead, error) {
	where := []string{"1=1"}
	var args []any
	if f.FormID != "" {
		where = append(where, "form_id = ?")
		args = append(args, f.FormID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since.Unix())
	}
	if f.BeforeSeq > 0 {
		where = append(where, "seq < ?")
		args = append(args, f.BeforeSeq)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+leadCols+` FROM leads WHERE `+strings.Join(where, " AND ")+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// Update changes the status (when non-empty) and/or note (when non-nil) and returns the lead.
func (s *Store) Update(ctx context.Context, id, status string, note *string) (*Lead, error) {
	if status != "" && !ValidStatus(status) {
		return nil, ErrInvalidStatus
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE leads SET status = CASE WHEN ? = '' THEN status ELSE ? END,
		                  note = CASE WHEN ? THEN ? ELSE note END,
		                  updated_at = ? WHERE id = ?`,
		status, status, note != nil, deref(note), s.now().Unix(), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// PurgeExpired deletes leads whose retention has passed and returns how many.
func (s *Store) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	// Retention needs no tombstone: a restored lead keeps its retention_until and is purged again.
	res, err := s.db.ExecContext(ctx, `DELETE FROM leads WHERE retention_until <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return n, err
	}
	// Also when nothing expired: an earlier run may have stopped before it got here. A blocked
	// checkpoint then is not worth a report.
	if err := s.checkpoint(ctx); err != nil && n > 0 {
		return n, err
	}
	return n, nil
}

// RecordDelivery stores the outcome of a notification channel for a lead.
func (s *Store) RecordDelivery(ctx context.Context, leadID, kind string, ok bool, attempts int, lastErr string) error {
	okInt := 0
	if ok {
		okInt = 1
	}
	if len(lastErr) > 500 {
		lastErr = lastErr[:500]
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO lead_deliveries (lead_id, kind, ok, attempts, last_error, at) VALUES (?, ?, ?, ?, ?, ?)`,
		leadID, kind, okInt, attempts, lastErr, s.now().Unix())
	return err
}

// Deliveries lists the delivery outcomes for a lead, oldest first.
func (s *Store) Deliveries(ctx context.Context, leadID string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, ok, attempts, last_error, at FROM lead_deliveries WHERE lead_id = ? ORDER BY id`, leadID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var ok int
		var at int64
		if err := rows.Scan(&d.Kind, &ok, &d.Attempts, &d.LastError, &at); err != nil {
			return nil, err
		}
		d.OK, d.At = ok == 1, time.Unix(at, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

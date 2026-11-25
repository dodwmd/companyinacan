package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// AuditEvent is one auditable action. Resource and Actor are opaque ids (person ids or
// "svc:<subject>"), never names. Fields lists the sensitive fields a read returned or a
// write changed.
type AuditEvent struct {
	Time     time.Time
	Actor    string
	Action   string // read, write, deny, erase, hold, ...
	Resource string // e.g. person:pers_..., meeting:mtg_...
	Fields   []string
	Outcome  string // ok | denied
}

// Auditor is the audit hook. The service records every sensitive read (before returning data:
// when Record fails, the read is refused) and every write. can-core can supply an
// implementation that forwards to its own audit log; the default SQLAuditor keeps a
// hash-chained append-only table in the service database.
type Auditor interface {
	Record(ctx context.Context, e AuditEvent) error
	// RecordBatch writes all events or none (one read that returns many records is audited per
	// record, atomically).
	RecordBatch(ctx context.Context, es []AuditEvent) error
}

// SQLAuditor writes an append-only (triggers refuse UPDATE and DELETE), hash-chained table:
// each row's hash covers the previous hash, so editing or removing an old row breaks every
// later hash. That detects accidental damage and edits by anyone who does not rewrite the whole
// chain; it does not stop a database administrator who can recompute it. To make tampering
// evident against that person too, keep the latest hash somewhere they cannot write (can-core's
// audit log, or a periodic copy of `can-board verify-audit` output).
type SQLAuditor struct {
	db  *sql.DB
	now func() time.Time
	mu  sync.Mutex
}

// Record implements Auditor.
func (a *SQLAuditor) Record(ctx context.Context, e AuditEvent) error {
	return a.RecordBatch(ctx, []AuditEvent{e})
}

// RecordBatch implements Auditor: one transaction, so the chain never holds half a batch.
func (a *SQLAuditor) RecordBatch(ctx context.Context, es []AuditEvent) error {
	if len(es) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var prev string
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, e := range es {
		if e.Time.IsZero() {
			e.Time = a.now()
		}
		fields := strings.Join(e.Fields, ",")
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome)))
		hash := hex.EncodeToString(h[:])
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource, fields, outcome, prev, hash) VALUES (?,?,?,?,?,?,?,?)`,
			e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome, prev, hash); err != nil {
			return err
		}
		prev = hash
	}
	return tx.Commit()
}

// VerifyAudit recomputes the hash chain and reports the first broken entry.
func (s *Service) VerifyAudit(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, action, resource, fields, outcome, prev, hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	prev := ""
	for rows.Next() {
		var seq, at int64
		var actor, action, resource, fields, outcome, p, hash string
		if err := rows.Scan(&seq, &at, &actor, &action, &resource, &fields, &outcome, &p, &hash); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, at, actor, action, resource, fields, outcome)))
		if p != prev || hash != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("audit log entry %d fails verification", seq)
		}
		prev = hash
	}
	return rows.Err()
}

// auditRead records a read and fails closed: the caller must not return data when it errors.
func (s *Service) auditRead(ctx context.Context, a Actor, resource string, fields []string) error {
	return s.audit.Record(ctx, AuditEvent{Time: s.now(), Actor: a.auditID(), Action: "read", Resource: resource, Fields: fields, Outcome: "ok"})
}

// auditReadMany records one read per resource atomically (a list or export is audited per record).
func (s *Service) auditReadMany(ctx context.Context, a Actor, action string, resources []string, fields []string) error {
	es := make([]AuditEvent, 0, len(resources))
	for _, r := range resources {
		es = append(es, AuditEvent{Time: s.now(), Actor: a.auditID(), Action: action, Resource: r, Fields: fields, Outcome: "ok"})
	}
	return s.audit.RecordBatch(ctx, es)
}

// auditDeny records a refused access attempt. Best effort: the refusal stands either way.
func (s *Service) auditDeny(ctx context.Context, a Actor, action, resource string, fields []string) {
	record, notice := s.deny.check(a.auditID(), s.now())
	if notice {
		_ = s.audit.Record(ctx, AuditEvent{Time: s.now(), Actor: a.auditID(), Action: "deny-throttled", Resource: "-", Outcome: "denied"})
	}
	if !record {
		return
	}
	_ = s.audit.Record(ctx, AuditEvent{Time: s.now(), Actor: a.auditID(), Action: action, Resource: resource, Fields: fields, Outcome: "denied"})
}

// RecordTx writes events inside the caller's transaction, so a mutation and its audit entry
// commit together or not at all (the service holds one connection, so the chain stays ordered).
func (a *SQLAuditor) RecordTx(ctx context.Context, tx *sql.Tx, es []AuditEvent) error {
	var prev string
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, e := range es {
		if e.Time.IsZero() {
			e.Time = a.now()
		}
		fields := strings.Join(e.Fields, ",")
		h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome)))
		hash := hex.EncodeToString(h[:])
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource, fields, outcome, prev, hash) VALUES (?,?,?,?,?,?,?,?)`,
			e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome, prev, hash); err != nil {
			return err
		}
		prev = hash
	}
	return nil
}

// txAuditor is implemented by auditors that can join the caller's transaction.
type txAuditor interface {
	RecordTx(ctx context.Context, tx *sql.Tx, es []AuditEvent) error
}

// auditIn records a mutation's audit entry inside tx. With an auditor that cannot join the
// transaction it records just before the commit, and the commit still fails if recording does.
func (s *Service) auditIn(ctx context.Context, tx *sqlTx, a Actor, action, resource string, fields []string) error {
	ev := AuditEvent{Time: s.now(), Actor: a.auditID(), Action: action, Resource: resource, Fields: fields, Outcome: "ok"}
	if ta, ok := s.audit.(txAuditor); ok {
		return ta.RecordTx(ctx, tx, []AuditEvent{ev})
	}
	return s.audit.Record(ctx, ev)
}

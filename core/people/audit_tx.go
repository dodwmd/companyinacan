package people

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// RecordTx writes an event inside the caller's transaction, so a mutation and its audit entry
// commit together or not at all (the service holds one connection, so the chain stays ordered).
func (a *SQLAuditor) RecordTx(ctx context.Context, tx *sql.Tx, e AuditEvent) error {
	if e.Time.IsZero() {
		e.Time = a.now()
	}
	var prev string
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	fields := strings.Join(e.Fields, ",")
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome)))
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource, fields, outcome, prev, hash) VALUES (?,?,?,?,?,?,?,?)`,
		e.Time.Unix(), e.Actor, e.Action, e.Resource, fields, e.Outcome, prev, hex.EncodeToString(h[:]))
	return err
}

// txAuditor is implemented by auditors that can join the caller's transaction.
type txAuditor interface {
	RecordTx(ctx context.Context, tx *sql.Tx, e AuditEvent) error
}

// auditIn records a mutation's audit entry inside tx. With an auditor that cannot join the
// transaction it records just before the commit, and the commit still fails if recording does.
func (s *Service) auditIn(ctx context.Context, tx *sqlTx, a Actor, action, resource string, fields []string) error {
	ev := AuditEvent{Time: s.now(), Actor: a.auditID(), Action: action, Resource: resource, Fields: fields, Outcome: "ok"}
	if ta, ok := s.audit.(txAuditor); ok {
		return ta.RecordTx(ctx, tx, ev)
	}
	return s.audit.Record(ctx, ev)
}

// auditReadMany records one read per resource: a list is audited per record, and the read is
// refused when any entry cannot be recorded.
func (s *Service) auditReadMany(ctx context.Context, a Actor, resources []string, fields []string) error {
	for _, r := range resources {
		if err := s.auditRead(ctx, a, r, fields); err != nil {
			return err
		}
	}
	return nil
}

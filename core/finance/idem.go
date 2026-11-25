package finance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Scrub truncates the write-ahead log so erased personal data does not linger in it
// (secure_delete already zeroes freed pages in the main file).
func (s *Store) Scrub(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// idemRun runs fn at most once per (subject, key). A retry with the same key and the same
// request returns the stored response and replayed=true; the same key with a different
// request is an error. An empty key disables the check. It must run inside a transaction.
// The scope names the operation so one key cannot be replayed across operations.
func idemRun[T any](ctx context.Context, s *Store, tx dbq, subject, key, scope string, req any, fn func() (T, error)) (v T, replayed bool, err error) {
	if key == "" {
		v, err = fn()
		return v, false, err
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return v, false, err
	}
	sum := sha256.Sum256(append([]byte(scope+"\x00"), reqJSON...))
	hash := hex.EncodeToString(sum[:])
	var oldHash, resp string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, response FROM idempotency WHERE subject = ? AND key = ?`, subject, key).Scan(&oldHash, &resp)
	switch {
	case err == nil:
		if oldHash != hash {
			return v, false, ErrIdempotencyMismatch
		}
		if err := json.Unmarshal([]byte(resp), &v); err != nil {
			return v, false, err
		}
		return v, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return v, false, err
	}
	if v, err = fn(); err != nil {
		return v, false, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency (subject, key, request_hash, response, status, created_at) VALUES (?, ?, ?, ?, 201, ?)`, subject, key, hash, string(b), ts(s.now()))
	return v, false, err
}

// readCtx bounds a read-side operation.
func readCtx(ctx context.Context) (context.CancelFunc, context.Context) {
	c, cancel := context.WithTimeout(ctx, readTimeout)
	return cancel, c
}

// recordUnmatched keeps a verified provider payment that could not be applied, once per event
// id: the provider redelivers until it gets a 200, and each delivery must not add a row.
func (s *Store) recordUnmatched(ctx context.Context, provider string, n *PaymentNotice, cause error) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM audit_log WHERE action = 'payment.unmatched' AND target = ? LIMIT 1`, n.EventID).Scan(&one)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return s.audit(ctx, tx, "provider:"+provider, "payment.unmatched", n.EventID,
			map[string]any{"invoice_id": n.InvoiceID, "amount_minor": n.AmountMinor, "currency": n.Currency, "reason": cause.Error()})
	})
}

// auditRead records a read of personal data or an export (SPEC.md section 5: audited
// sensitive reads). It uses the writer connection, and callers return the error, so a read
// that must be audited fails closed when the audit cannot be written.
func (s *Store) auditRead(ctx context.Context, a *Principal, action, target string, detail any) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, a.Subject, action, target, detail) })
}

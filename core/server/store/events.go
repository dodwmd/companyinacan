package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
)

// Event is a persisted event envelope (the outbox row).
type Event struct {
	ID       string
	Type     string
	Time     time.Time
	Source   string
	DataJSON string
}

// DeliveryTarget is one subscriber to deliver an event to.
type DeliveryTarget struct {
	ModuleID string
	URL      string
}

// Delivery is the delivery state of one event to one module.
type Delivery struct {
	ID            int64
	ModuleID      string
	URL           string
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	Event         Event
	HMACSecret    string // joined from the module record; only set by DueDeliveries
}

// Attempt is one HTTP attempt in the delivery log.
type Attempt struct {
	Number     int
	At         time.Time
	StatusCode int
	Error      string
	Duration   time.Duration
}

// InsertEvent stores the event and one pending delivery per target in a single transaction.
func (db *DB) InsertEvent(ctx context.Context, e Event, targets []DeliveryTarget) error {
	return db.InTx(ctx, func(tx *sql.Tx) error { return insertEventTx(ctx, tx, e, targets) })
}

func insertEventTx(ctx context.Context, tx *sql.Tx, e Event, targets []DeliveryTarget) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,type,time,source,data_json) VALUES (?,?,?,?,?)`,
		e.ID, e.Type, e.Time.Unix(), e.Source, e.DataJSON); err != nil {
		return err
	}
	for _, t := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(event_id,module_id,url,status,attempts,next_attempt_at) VALUES (?,?,?,?,0,?)`,
			e.ID, t.ModuleID, t.URL, StatusPending, e.Time.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// DueDeliveries returns pending deliveries whose next attempt time has arrived, oldest first.
func (db *DB) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.id,d.module_id,d.url,d.status,d.attempts,d.next_attempt_at,
		e.id,e.type,e.time,e.source,e.data_json, COALESCE(m.hmac_secret,'')
		FROM deliveries d JOIN events e ON e.id=d.event_id JOIN modules m ON m.id=d.module_id
		WHERE d.status='pending' AND m.state='enabled' AND d.next_attempt_at <= ? ORDER BY d.next_attempt_at, d.id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var next, et int64
		if err := rows.Scan(&d.ID, &d.ModuleID, &d.URL, &d.Status, &d.Attempts, &next,
			&d.Event.ID, &d.Event.Type, &et, &d.Event.Source, &d.Event.DataJSON, &d.HMACSecret); err != nil {
			return nil, err
		}
		d.NextAttemptAt, d.Event.Time = time.Unix(next, 0), time.Unix(et, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordAttempt appends to the delivery log and updates the delivery's status and next attempt.
func (db *DB) RecordAttempt(ctx context.Context, deliveryID int64, a Attempt, status string, next time.Time) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET attempts=attempts+1, status=?, next_attempt_at=? WHERE id=?`,
			status, next.Unix(), deliveryID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO delivery_attempts(delivery_id,attempt,at,status_code,error,duration_ms)
			VALUES (?, (SELECT attempts FROM deliveries WHERE id=?), ?,?,?,?)`,
			deliveryID, deliveryID, a.At.Unix(), a.StatusCode, a.Error, a.Duration.Milliseconds())
		return err
	})
}

// ListDeliveries returns the most recent deliveries (newest first) for the delivery log view.
func (db *DB) ListDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.id,d.module_id,d.url,d.status,d.attempts,d.next_attempt_at,e.id,e.type,e.time,e.source,e.data_json
		FROM deliveries d JOIN events e ON e.id=d.event_id ORDER BY d.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var next, et int64
		if err := rows.Scan(&d.ID, &d.ModuleID, &d.URL, &d.Status, &d.Attempts, &next, &d.Event.ID, &d.Event.Type, &et, &d.Event.Source, &d.Event.DataJSON); err != nil {
			return nil, err
		}
		d.NextAttemptAt, d.Event.Time = time.Unix(next, 0), time.Unix(et, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Attempts returns the attempts of one delivery in order.
func (db *DB) Attempts(ctx context.Context, deliveryID int64) ([]Attempt, error) {
	rows, err := db.QueryContext(ctx, `SELECT attempt,at,status_code,error,duration_ms FROM delivery_attempts WHERE delivery_id=? ORDER BY id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var at, ms int64
		if err := rows.Scan(&a.Number, &at, &a.StatusCode, &a.Error, &ms); err != nil {
			return nil, err
		}
		a.At, a.Duration = time.Unix(at, 0), time.Duration(ms)*time.Millisecond
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrNotDead is returned by RequeueDelivery for a delivery that is not dead.
var ErrNotDead = errors.New("delivery is not dead")

// RequeueDelivery makes a dead delivery pending again with a fresh attempt budget.
func (db *DB) RequeueDelivery(ctx context.Context, id int64, now time.Time) error {
	return db.InTx(ctx, func(tx *sql.Tx) error { return RequeueDeliveryTx(ctx, tx, id, now) })
}

// RequeueDeliveryTx is RequeueDelivery inside the caller's transaction (so it can be committed
// together with its audit entry).
func RequeueDeliveryTx(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE deliveries SET status='pending', attempts=0, next_attempt_at=? WHERE id=? AND status='dead'`, now.Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM deliveries WHERE id=?`, id).Scan(&one); err != nil {
			return notFound(err)
		}
		return ErrNotDead
	}
	return nil
}

// PruneEvents deletes events (and cascading deliveries) older than before whose deliveries are
// all finished. The outbox is a derived store with a TTL (SPEC §3.7).
func (db *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM events WHERE time < ? AND NOT EXISTS
		(SELECT 1 FROM deliveries d JOIN modules m ON m.id=d.module_id
		 WHERE d.event_id=events.id AND d.status='pending' AND m.state='enabled')`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

package store

import (
	"context"
	"time"
)

// GetDelivery returns one delivery with its event envelope (HMACSecret is never loaded).
func (db *DB) GetDelivery(ctx context.Context, id int64) (Delivery, error) {
	var d Delivery
	var next, et int64
	err := db.QueryRowContext(ctx, `SELECT d.id,d.module_id,d.url,d.status,d.attempts,d.next_attempt_at,e.id,e.type,e.time,e.source,e.data_json
		FROM deliveries d JOIN events e ON e.id=d.event_id WHERE d.id=?`, id).
		Scan(&d.ID, &d.ModuleID, &d.URL, &d.Status, &d.Attempts, &next, &d.Event.ID, &d.Event.Type, &et, &d.Event.Source, &d.Event.DataJSON)
	d.NextAttemptAt, d.Event.Time = time.Unix(next, 0), time.Unix(et, 0)
	return d, notFound(err)
}

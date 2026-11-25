package files

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Event types published by the files module. Payload schemas are schemas/events/<type>.json.
// Payloads carry opaque ids, sizes and a checksum only: never a file name, description, share
// label or content, so an event never outlives an erasure with personal data in it.
const (
	EventUploaded = "file.uploaded.v1"
	EventShared   = "file.shared.v1"
	EventDeleted  = "file.deleted.v1"
	EventInfected = "file.infected.v1"

	eventSource        = "files"
	maxBackoff         = 6 * time.Hour
	deliveryWindow     = 7 * 24 * time.Hour // undelivered after this long: dead-lettered, not dropped
	deadRetention      = 90 * 24 * time.Hour
	deliveredRetention = 7 * 24 * time.Hour
	outboxBatch        = 20
)

type envelope struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

func uploadedEvent(fileID, spaceID string, version int, st *staged, actor, ref string, at time.Time) map[string]any {
	d := map[string]any{"file_id": fileID, "space_id": spaceID, "version": version, "size": st.size, "content_type": st.ct,
		"sha256": st.sha, "actor": actor, "uploaded_at": at.UTC()}
	if ref != "" {
		d["ref"] = ref
	}
	return d
}

func infectedEvent(spaceID, fileID string, st *staged, actor string, at time.Time) map[string]any {
	d := map[string]any{"space_id": spaceID, "size": st.size, "sha256": st.sha, "actor": actor, "detected_at": at.UTC()}
	if fileID != "" {
		d["file_id"] = fileID
	}
	return d
}

func deletedEvent(fileID, spaceID, reason, actor string, at time.Time) map[string]any {
	return map[string]any{"file_id": fileID, "space_id": spaceID, "reason": reason, "actor": actor, "deleted_at": at.UTC()}
}

// emit queues an event in the caller's transaction, so a change and its event commit or roll back
// together. It is a no-op when the deployment has no can-core to publish through.
func (s *Store) emit(ctx context.Context, tx dbq, typ string, data any) error {
	if !s.events {
		return nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	ev := envelope{ID: "evt_" + randHex(12), Type: typ, Time: now, Source: eventSource, Data: b}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox (event_id, type, payload, created_at, next_attempt) VALUES (?,?,?,?,?)`, ev.ID, typ, string(payload), ts(now), ts(now))
	return err
}

// Publisher sends an event envelope through can-core (*modclient.Client).
type Publisher interface {
	PublishEnvelope(ctx context.Context, envelope []byte) error
}

// Dispatcher delivers the outbox through can-core, at least once, with exponential backoff.
// Receivers de-duplicate on the event id, which is also the publish's source id.
type Dispatcher struct {
	Store   *Store
	Pub     Publisher
	Log     *slog.Logger
	Backoff time.Duration
}

// Run delivers until ctx is done, waking on new events and on a timer.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if _, err := d.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("event delivery failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.Store.kick:
		case <-t.C:
		}
	}
}

// DeliverDue sends every due event once and returns how many were delivered.
func (d *Dispatcher) DeliverDue(ctx context.Context) (int, error) {
	s := d.Store
	now := s.now()
	type row struct {
		id       int64
		eventID  string
		typ      string
		payload  string
		attempts int
		created  int64
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, event_id, type, payload, attempts, created_at FROM outbox WHERE (delivered_at IS NULL) AND (dead_at IS NULL) AND (next_attempt <= ?) ORDER BY id LIMIT ?`, ts(now), outboxBatch)
	if err != nil {
		return 0, err
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.eventID, &r.typ, &r.payload, &r.attempts, &r.created); err != nil {
			_ = rows.Close()
			return 0, err
		}
		due = append(due, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range due {
		if now.Sub(fromUnix(r.created)) > deliveryWindow {
			if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET dead_at = ? WHERE (id = ?)`, ts(now), r.id); err != nil {
				return sent, err
			}
			d.Log.Error("event dead-lettered: not delivered within the delivery window", "event_id", r.eventID, "type", r.typ, "attempts", r.attempts)
			continue
		}
		if sendErr := d.Pub.PublishEnvelope(ctx, []byte(r.payload)); sendErr == nil {
			sent++
			_, err = s.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ?, attempts = attempts + 1, last_error = '' WHERE (id = ?)`, ts(now), r.id)
		} else {
			delay := d.Backoff
			if delay <= 0 {
				delay = 30 * time.Second
			}
			delay = min(delay<<min(r.attempts, 12), maxBackoff)
			_, err = s.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_attempt = ?, last_error = ? WHERE (id = ?)`, ts(now.Add(delay)), "publish failed", r.id)
		}
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// purgeOutbox removes delivered events after a week and dead ones after 90 days.
func (s *Store) purgeOutbox(ctx context.Context, tx dbq, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE ((delivered_at IS NOT NULL) AND (created_at < ?)) OR ((dead_at IS NOT NULL) AND (dead_at < ?))`,
		ts(now.Add(-deliveredRetention)), ts(now.Add(-deadRetention)))
	return err
}

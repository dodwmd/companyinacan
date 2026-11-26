package store

import (
	"context"
	"testing"
)

// QA of #52, 6: mail queued before notifications recorded their module must not go out as the platform's own.
func TestDigestsQueuedBeforeOriginsAreAttributedToTheirModule(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertModule(ctx, Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE schema_migrations SET applied_at=1000 WHERE version=11`)
	add := func(id int, typ, data, status string, at int) {
		t.Helper()
		exec(`INSERT INTO notifications(id, subject, type, dedupe_key, data_json, link, urgent, created_at) VALUES (?, 'user:a', ?, ?, ?, '', 0, ?)`, id, typ, id, data, at)
		exec(`INSERT INTO notify_deliveries(notification_id, channel, status, detail, at) VALUES (?, 'email', ?, '', ?)`, id, status, at)
	}
	add(1, "crm.followup", `{}`, "queued", 900)
	add(2, "gone.reminder", `{"title":"Reset your password"}`, "queued", 900) // a module removed since
	add(3, "approval.requested", `{"request_id":"apr_1"}`, "queued", 900)
	add(4, "lead.created", `{"lead_id":"l1"}`, "queued", 900) // a rule's notice: ids only
	add(5, "crm.followup", `{}`, "queued", 1100)              // queued after origins existed: the platform's, as recorded
	add(6, "crm.followup", `{}`, "sent", 900)

	body, err := migrationFS.ReadFile("migrations/0013_digest_origin_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(body))

	for id, want := range map[int]string{1: "crm", 2: "gone", 3: "", 4: "", 5: "", 6: ""} {
		var origin string
		var authored bool
		if err := db.QueryRowContext(ctx, `SELECT origin, authored FROM notifications WHERE id=?`, id).Scan(&origin, &authored); err != nil {
			t.Fatal(err)
		}
		if origin != want || authored != (want != "") {
			t.Errorf("notification %d: origin %q authored %v, want %q", id, origin, authored, want)
		}
	}
}

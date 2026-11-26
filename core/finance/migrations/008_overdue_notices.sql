-- Which overdue invoices have been announced (invoice.overdue.v1), so that a reminder is raised once
-- per due date however often the sweep runs and whatever restarts in between (specs/060-reminders).
CREATE TABLE overdue_notices (
    invoice_id  INTEGER NOT NULL PRIMARY KEY REFERENCES invoices (id) ON DELETE CASCADE,
    due_date    TEXT    NOT NULL,
    notified_at TEXT    NOT NULL
);

-- E03c: approvals through can-core's engine. One row per thing awaiting or holding a can-core
-- approval (today: invoices). version is the invoice version the request was opened for: an edit
-- after the decision makes the decision stale. Holds ids only: no names, amounts or text.
CREATE TABLE core_approvals (
    ref          TEXT    PRIMARY KEY,                      -- invoice:<id>
    approval_id  TEXT    NOT NULL UNIQUE,                  -- can-core's request id
    version      INTEGER NOT NULL,
    attempt      INTEGER NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    requested_by TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    decided_at   INTEGER
);

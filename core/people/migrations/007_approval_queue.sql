-- E11g: time off requests waiting for can-core's approval engine (specs/070-approvals-inbox,
-- core/modapproval). One open row per request; ids and account subjects only. Requests already
-- pending when this is applied are queued by the service at its next pass.
CREATE TABLE approval_queue (
    ref         TEXT    PRIMARY KEY,           -- timeoff:<leave request id>:<nonce>: also the idempotency key
    kind        TEXT    NOT NULL,              -- timeoff
    item_id     TEXT    NOT NULL,
    version     INTEGER NOT NULL DEFAULT 0,
    approval_id TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL CHECK (status IN ('queued', 'pending', 'refused', 'approved', 'rejected', 'cancel')),
    requester   TEXT    NOT NULL,
    manager     TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    queued_at   INTEGER NOT NULL,
    retry_at    INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX approval_queue_item ON approval_queue (kind, item_id) WHERE status <> 'cancel';
CREATE INDEX approval_queue_due ON approval_queue (status, retry_at, queued_at);

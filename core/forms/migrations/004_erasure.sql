-- Erasure of single leads (specs/054-lead-erasure). None of these tables holds personal data.

-- One tombstone per erased lead, read by can-core into the backup's erasure ledger.
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    form_id    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (subject, class)
);

-- lead.erased.v1 still to be handed over, per channel; a row is removed once it is accepted.
CREATE TABLE erasure_notices (
    subject         TEXT    NOT NULL,
    channel         TEXT    NOT NULL CHECK (channel IN ('can', 'webhook')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    PRIMARY KEY (subject, channel)
);

-- Who searched or erased. The target is a lead id or "-", never an address.
CREATE TABLE lead_audit (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    at     INTEGER NOT NULL,
    actor  TEXT    NOT NULL,
    action TEXT    NOT NULL,
    target TEXT    NOT NULL,
    count  INTEGER NOT NULL DEFAULT 0
);

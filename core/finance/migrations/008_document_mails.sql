-- specs/062-invoice-email: documents mailed to customers and the payment reminder schedule.
-- No table here holds an address, a name or a message: ids, states, times and a random key only.

-- One row per document handed (or about to be handed) to can-core's email.send. It is written
-- before the call, with `nonce` as the idempotency key, so that a restart can neither lose nor
-- repeat a message.
CREATE TABLE document_mails (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (kind IN ('document', 'statement', 'reminder')),
    invoice_id   INTEGER REFERENCES invoices (id) ON DELETE CASCADE, -- NULL for a statement
    party_id     INTEGER NOT NULL REFERENCES parties (id) ON DELETE CASCADE,
    step         INTEGER NOT NULL DEFAULT 0,                         -- reminder number, from 1
    status       TEXT    NOT NULL CHECK (status IN ('pending', 'retry', 'queued', 'sent', 'failed', 'not_allowed', 'cancelled', 'unknown')),
    nonce        TEXT    NOT NULL UNIQUE,
    core_id      TEXT    NOT NULL DEFAULT '',                        -- can-core's message id
    link_expires INTEGER NOT NULL,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    handed_at    INTEGER NOT NULL DEFAULT 0                          -- when it left for the mail service
);
-- A reminder step is claimed once per invoice, whatever restarts or runs in parallel.
CREATE UNIQUE INDEX document_mails_step ON document_mails (invoice_id, step) WHERE kind = 'reminder';
CREATE INDEX document_mails_invoice ON document_mails (invoice_id, id);
CREATE INDEX document_mails_party ON document_mails (party_id, id);
CREATE INDEX document_mails_open ON document_mails (status, id) WHERE status IN ('pending', 'retry', 'queued');

-- Reminders switched off for one invoice ('stopped', or 'disputed' by the customer) ...
CREATE TABLE invoice_reminder_holds (
    invoice_id INTEGER PRIMARY KEY REFERENCES invoices (id) ON DELETE CASCADE,
    reason     TEXT    NOT NULL CHECK (reason IN ('stopped', 'disputed')),
    set_by     TEXT    NOT NULL,
    set_at     INTEGER NOT NULL
);
-- ... or for everything a party owes.
CREATE TABLE party_reminder_stops (
    party_id INTEGER PRIMARY KEY REFERENCES parties (id) ON DELETE CASCADE,
    set_by   TEXT    NOT NULL,
    set_at   INTEGER NOT NULL
);
-- The schedule. No row: reminders are off.
CREATE TABLE dunning_policy (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    enabled          INTEGER NOT NULL,
    days_after_due   TEXT    NOT NULL, -- JSON array of days after the due date
    max_overdue_days INTEGER NOT NULL,
    updated_by       TEXT    NOT NULL,
    updated_at       INTEGER NOT NULL,
    restored_at      INTEGER NOT NULL DEFAULT 0 -- set when a restore switched reminders off
);

-- Captured form submissions. Timestamps are unix seconds (UTC).
-- retention_until is set at insert time from the form's retention_days and drives purging.
CREATE TABLE leads (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    id              TEXT    NOT NULL UNIQUE,
    form_id         TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'new'
                    CHECK (status IN ('new', 'contacted', 'qualified', 'closed', 'spam')),
    email           TEXT    NOT NULL DEFAULT '',
    name            TEXT    NOT NULL DEFAULT '',
    fields          TEXT    NOT NULL,
    note            TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    retention_until INTEGER NOT NULL
);

CREATE INDEX leads_form_seq   ON leads (form_id, seq DESC);
CREATE INDEX leads_status_seq ON leads (status, seq DESC);
CREATE INDEX leads_retention  ON leads (retention_until);

-- Finance schema. Timestamps are unix seconds (UTC); dates are YYYY-MM-DD text; every amount
-- is an integer count of the currency's minor unit. Defence in depth: the rules live in the
-- store, and the triggers below refuse the edits the rules forbid (issued invoices, payments,
-- contract versions and the audit log are append-only) even if a bug or a manual SQL session
-- tries. The retention purge opens the gate for the length of its own transaction.

CREATE TABLE retention_gate (open INTEGER NOT NULL, audit_before INTEGER NOT NULL DEFAULT 0);
INSERT INTO retention_gate (open, audit_before) VALUES (0, 0);

-- Customers and vendors share one party model.
CREATE TABLE parties (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (kind IN ('customer', 'vendor', 'both')),
    name         TEXT    NOT NULL,
    contact_name TEXT    NOT NULL DEFAULT '',
    email        TEXT    NOT NULL DEFAULT '',
    phone        TEXT    NOT NULL DEFAULT '',
    address      TEXT    NOT NULL DEFAULT '',
    tax_id       TEXT    NOT NULL DEFAULT '',
    currency     TEXT    NOT NULL DEFAULT '',
    notes        TEXT    NOT NULL DEFAULT '',
    erased_at    INTEGER,
    version      INTEGER NOT NULL DEFAULT 1,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- Tax is data: a rate has a code, a name and a rate in parts per million. A line copies the
-- rate when it is written, so changing a rate never alters an issued invoice.
CREATE TABLE tax_rates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    rate_ppm   INTEGER NOT NULL CHECK (rate_ppm BETWEEN 0 AND 1000000),
    active     INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE products (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    sku              TEXT    NOT NULL UNIQUE,
    name             TEXT    NOT NULL,
    description      TEXT    NOT NULL DEFAULT '',
    unit_price_minor INTEGER NOT NULL CHECK (unit_price_minor >= 0),
    currency         TEXT    NOT NULL,
    tax_code         TEXT    NOT NULL DEFAULT '',
    active           INTEGER NOT NULL DEFAULT 1,
    version          INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- Numbering. A number is taken from next_number in the same transaction that approves the
-- document, so a rollback gives it back: numbers are gap-free per series.
CREATE TABLE series (
    code        TEXT    PRIMARY KEY,
    next_number INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE invoices (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                 TEXT    NOT NULL CHECK (kind IN ('invoice', 'credit_note')),
    status               TEXT    NOT NULL CHECK (status IN ('draft', 'approved', 'sent', 'partially_paid', 'paid', 'void')),
    series               TEXT    NOT NULL,
    number               INTEGER,
    number_text          TEXT    UNIQUE,
    party_id             INTEGER NOT NULL REFERENCES parties (id),
    party_snapshot       TEXT    NOT NULL DEFAULT '',  -- JSON name/address/tax id frozen at approval
    project              TEXT    NOT NULL DEFAULT '',  -- opaque project key owned by `work`
    currency             TEXT    NOT NULL,
    base_currency        TEXT    NOT NULL,
    fx_rate_e8           INTEGER NOT NULL DEFAULT 100000000,
    issue_date           TEXT    NOT NULL,
    due_date             TEXT    NOT NULL,
    subtotal_minor       INTEGER NOT NULL DEFAULT 0,
    discount_minor       INTEGER NOT NULL DEFAULT 0,
    tax_minor            INTEGER NOT NULL DEFAULT 0,
    total_minor          INTEGER NOT NULL DEFAULT 0,
    base_total_minor     INTEGER NOT NULL DEFAULT 0,
    paid_minor           INTEGER NOT NULL DEFAULT 0,
    credited_minor       INTEGER NOT NULL DEFAULT 0,
    original_id          INTEGER REFERENCES invoices (id),
    reason               TEXT    NOT NULL DEFAULT '',
    notes                TEXT    NOT NULL DEFAULT '',
    payment_instructions TEXT    NOT NULL DEFAULT '',
    link_epoch           INTEGER NOT NULL DEFAULT 0,
    recurring_id         INTEGER,
    recurring_period     TEXT,
    created_by           TEXT    NOT NULL,
    updated_by           TEXT    NOT NULL,
    approved_by          TEXT,
    approved_at          INTEGER,
    sent_at              INTEGER,
    paid_at              INTEGER,
    voided_at            INTEGER,
    void_reason          TEXT    NOT NULL DEFAULT '',
    version              INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    UNIQUE (series, number),
    CHECK ((status = 'draft') = (number IS NULL))
);
CREATE INDEX invoices_party  ON invoices (party_id);
CREATE INDEX invoices_status ON invoices (status, due_date);
CREATE UNIQUE INDEX invoices_recurring ON invoices (recurring_id, recurring_period) WHERE recurring_id IS NOT NULL;

CREATE TABLE invoice_lines (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id       INTEGER NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    position         INTEGER NOT NULL,
    description      TEXT    NOT NULL,
    sku              TEXT    NOT NULL DEFAULT '',
    qty_milli        INTEGER NOT NULL,
    unit_price_minor INTEGER NOT NULL,
    discount_bp      INTEGER NOT NULL DEFAULT 0,
    tax_code         TEXT    NOT NULL DEFAULT '',
    tax_rate_ppm     INTEGER NOT NULL DEFAULT 0,
    gross_minor      INTEGER NOT NULL,
    discount_minor   INTEGER NOT NULL,
    tax_minor        INTEGER NOT NULL,
    total_minor      INTEGER NOT NULL,
    UNIQUE (invoice_id, position)
);

-- Payments are append-only: a mistake is corrected by a credit note or a documented
-- adjustment, never by editing the ledger.
CREATE TABLE payments (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id   INTEGER NOT NULL REFERENCES invoices (id),
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    currency     TEXT    NOT NULL,
    method       TEXT    NOT NULL DEFAULT '',
    reference    TEXT    NOT NULL DEFAULT '',
    received_on  TEXT    NOT NULL,
    source       TEXT    NOT NULL CHECK (source IN ('manual', 'import', 'provider')),
    external_id  TEXT    NOT NULL DEFAULT '',
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX payments_invoice ON payments (invoice_id);
CREATE UNIQUE INDEX payments_external ON payments (source, external_id) WHERE external_id <> '';

CREATE TABLE recurring (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id     INTEGER NOT NULL REFERENCES parties (id),
    name         TEXT    NOT NULL,
    currency     TEXT    NOT NULL,
    fx_rate_e8   INTEGER NOT NULL DEFAULT 100000000,
    project      TEXT    NOT NULL DEFAULT '',
    series       TEXT    NOT NULL,
    lines        TEXT    NOT NULL,                      -- JSON line inputs, priced when generated
    interval     TEXT    NOT NULL CHECK (interval IN ('weekly', 'monthly', 'quarterly', 'yearly')),
    next_date    TEXT    NOT NULL,
    end_date     TEXT,
    due_days     INTEGER NOT NULL DEFAULT 30,
    notes        TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'ended')),
    created_by   TEXT    NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE contract_templates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    body       TEXT    NOT NULL,
    variables  TEXT    NOT NULL,                        -- JSON array of variable names
    version    INTEGER NOT NULL DEFAULT 1,
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE contracts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    party_id         INTEGER NOT NULL REFERENCES parties (id),
    template_id      INTEGER REFERENCES contract_templates (id),
    title            TEXT    NOT NULL,
    status           TEXT    NOT NULL CHECK (status IN ('draft', 'sent', 'signed', 'active', 'expired', 'terminated')),
    current_version  INTEGER NOT NULL DEFAULT 1,
    start_date       TEXT,
    end_date         TEXT,
    renewal_date     TEXT,
    reminder_days    INTEGER NOT NULL DEFAULT 30,
    reminded_for     TEXT    NOT NULL DEFAULT '',        -- the renewal date a reminder was already raised for
    esign_provider   TEXT    NOT NULL DEFAULT '',
    esign_id         TEXT    NOT NULL DEFAULT '',
    signed_at        INTEGER,
    signed_sha256    TEXT    NOT NULL DEFAULT '',
    terminated_at    INTEGER,
    terminate_reason TEXT    NOT NULL DEFAULT '',
    created_by       TEXT    NOT NULL,
    updated_by       TEXT    NOT NULL,
    version          INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX contracts_party ON contracts (party_id);

CREATE TABLE contract_versions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    contract_id INTEGER NOT NULL REFERENCES contracts (id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    body        TEXT    NOT NULL,
    variables   TEXT    NOT NULL,
    sha256      TEXT    NOT NULL,
    created_by  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (contract_id, version)
);

CREATE TABLE timesheets (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    owner         TEXT    NOT NULL,
    week_start    TEXT    NOT NULL,                     -- the Monday
    status        TEXT    NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'submitted', 'approved', 'rejected')),
    submitted_at  INTEGER,
    decided_by    TEXT,
    decided_at    INTEGER,
    reject_reason TEXT    NOT NULL DEFAULT '',
    version       INTEGER NOT NULL DEFAULT 1,
    created_by    TEXT    NOT NULL,
    updated_by    TEXT    NOT NULL DEFAULT '',  -- last person to change the entries
    submitted_by  TEXT    NOT NULL DEFAULT '',  -- who submitted it (the owner or an admin on their behalf)
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (owner, week_start)
);

CREATE TABLE time_entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timesheet_id INTEGER NOT NULL REFERENCES timesheets (id) ON DELETE CASCADE,
    day         TEXT    NOT NULL,
    project     TEXT    NOT NULL,                       -- opaque project key owned by `work`
    task        TEXT    NOT NULL DEFAULT '',            -- opaque issue key owned by `work`
    minutes     INTEGER NOT NULL CHECK (minutes BETWEEN 1 AND 1440),
    billable    INTEGER NOT NULL DEFAULT 0,
    description TEXT    NOT NULL DEFAULT '',
    invoice_id  INTEGER REFERENCES invoices (id)
);
CREATE INDEX time_entries_sheet   ON time_entries (timesheet_id);
CREATE INDEX time_entries_project ON time_entries (project, day);

CREATE TABLE period_locks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    through_date TEXT    NOT NULL,
    locked_by    TEXT    NOT NULL,
    note         TEXT    NOT NULL DEFAULT '',
    at           INTEGER NOT NULL
);

CREATE TABLE expenses (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    owner                 TEXT    NOT NULL,
    description           TEXT    NOT NULL,
    amount_minor          INTEGER NOT NULL CHECK (amount_minor > 0),
    currency              TEXT    NOT NULL,
    category              TEXT    NOT NULL,
    incurred_on           TEXT    NOT NULL,
    project               TEXT    NOT NULL DEFAULT '',
    receipt_filename      TEXT    NOT NULL DEFAULT '',  -- metadata only: the service stores no file bytes
    receipt_content_type  TEXT    NOT NULL DEFAULT '',
    receipt_size          INTEGER NOT NULL DEFAULT 0,
    receipt_sha256        TEXT    NOT NULL DEFAULT '',
    status                TEXT    NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'submitted', 'approved', 'rejected', 'reimbursed')),
    decided_by            TEXT,
    decided_at            INTEGER,
    reject_reason         TEXT    NOT NULL DEFAULT '',
    reimbursement_ref     TEXT    NOT NULL DEFAULT '',
    version               INTEGER NOT NULL DEFAULT 1,
    created_by            TEXT    NOT NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);
CREATE INDEX expenses_owner ON expenses (owner);

CREATE TABLE purchase_orders (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    vendor_id    INTEGER NOT NULL REFERENCES parties (id),
    number       INTEGER,
    number_text  TEXT    UNIQUE,
    status       TEXT    NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'received', 'cancelled')),
    currency     TEXT    NOT NULL,
    project      TEXT    NOT NULL DEFAULT '',
    lines        TEXT    NOT NULL,                      -- JSON of priced lines
    subtotal_minor INTEGER NOT NULL,
    tax_minor    INTEGER NOT NULL,
    total_minor  INTEGER NOT NULL,
    notes        TEXT    NOT NULL DEFAULT '',
    created_by   TEXT    NOT NULL,
    updated_by   TEXT    NOT NULL,
    approved_by  TEXT,
    approved_at  INTEGER,
    version      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE idempotency (
    subject      TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    request_hash TEXT    NOT NULL,
    response     TEXT    NOT NULL,
    status       INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (subject, key)
);

CREATE TABLE api_tokens (
    id           TEXT    PRIMARY KEY,
    kind         TEXT    NOT NULL CHECK (kind IN ('personal', 'service')),
    subject      TEXT    NOT NULL,
    name         TEXT    NOT NULL DEFAULT '',
    secret_hash  BLOB    NOT NULL,
    scopes       TEXT    NOT NULL,
    parties      TEXT    NOT NULL DEFAULT '[]',         -- JSON party ids; empty = all
    projects     TEXT    NOT NULL DEFAULT '[]',         -- JSON project keys; empty = all
    own_only     INTEGER NOT NULL DEFAULT 0,
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    revoked_at   INTEGER,
    last_used_at INTEGER
);

-- Append-only audit of admin actions, approvals, exports and reads of personal data. It
-- never holds secrets or contact details.
CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    actor  TEXT    NOT NULL,
    action TEXT    NOT NULL,
    target TEXT    NOT NULL DEFAULT '',
    detail TEXT    NOT NULL DEFAULT '{}',
    at     INTEGER NOT NULL
);

CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL,
    payload      TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL,
    delivered_at INTEGER,
    dead_at      INTEGER,
    last_error   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX outbox_due  ON outbox (next_attempt) WHERE delivered_at IS NULL AND dead_at IS NULL;
CREATE INDEX outbox_dead ON outbox (dead_at) WHERE dead_at IS NOT NULL;

-- ---- immutability triggers ----------------------------------------------------------

-- Issued documents keep their content. Allowed changes after approval: status, payment and
-- credit totals, timestamps, the link epoch, approval and void fields, and version.
CREATE TRIGGER invoices_immutable BEFORE UPDATE OF kind, series, number, number_text, party_id, party_snapshot,
    project, currency, base_currency, fx_rate_e8, issue_date, due_date, subtotal_minor, discount_minor, tax_minor,
    total_minor, base_total_minor, original_id, reason, notes, payment_instructions, created_by ON invoices
    WHEN OLD.status <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'issued invoices are immutable: void and issue a credit note');
END;
-- A draft takes a number only by the approval transition; it may not skip draft otherwise.
CREATE TRIGGER invoices_no_delete BEFORE DELETE ON invoices
    WHEN OLD.status <> 'draft' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'issued invoices cannot be deleted before their retention period ends');
END;
CREATE TRIGGER invoice_lines_no_insert BEFORE INSERT ON invoice_lines
    WHEN (SELECT status FROM invoices WHERE id = NEW.invoice_id) <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'lines of an issued invoice are immutable');
END;
CREATE TRIGGER invoice_lines_no_update BEFORE UPDATE ON invoice_lines
    WHEN (SELECT status FROM invoices WHERE id = OLD.invoice_id) <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'lines of an issued invoice are immutable');
END;
CREATE TRIGGER invoice_lines_no_delete BEFORE DELETE ON invoice_lines
    WHEN (SELECT status FROM invoices WHERE id = OLD.invoice_id) <> 'draft' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'lines of an issued invoice are immutable');
END;

CREATE TRIGGER payments_no_update BEFORE UPDATE ON payments
BEGIN
    SELECT RAISE(ABORT, 'payments are append-only');
END;
CREATE TRIGGER payments_no_delete BEFORE DELETE ON payments WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'payments are append-only');
END;

CREATE TRIGGER contract_versions_no_update BEFORE UPDATE ON contract_versions
BEGIN
    SELECT RAISE(ABORT, 'contract versions are immutable');
END;
CREATE TRIGGER contract_versions_no_delete BEFORE DELETE ON contract_versions WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'contract versions are immutable');
END;

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'the audit log is append-only');
END;
-- Entries are deleted only by the retention purge, which opens the gate and names the cut-off
-- (never less than a year old: the purge enforces that floor).
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit_log
    WHEN (SELECT open FROM retention_gate) = 0 OR OLD.at >= (SELECT audit_before FROM retention_gate)
BEGIN
    SELECT RAISE(ABORT, 'the audit log is append-only');
END;

-- E15c accounting interfaces: chart of accounts, double-entry journal, period close, bank accounts,
-- statement import, reconciliation and export batches. Every amount is an integer count of the
-- currency's minor unit. As with invoices, the rules live in the store and the triggers below refuse the
-- edits the rules forbid (a posted journal entry is immutable, a closed period takes no new entry, a
-- bank transaction keeps what the bank said) even if a bug or a manual SQL session tries. The retention
-- purge opens the gate (retention_gate) for the length of its own transaction.

CREATE TABLE ledger_accounts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    type       TEXT    NOT NULL CHECK (type IN ('asset', 'liability', 'equity', 'income', 'expense')),
    tax_code   TEXT    NOT NULL DEFAULT '',                -- a tax_rates.code this account collects or pays
    active     INTEGER NOT NULL DEFAULT 1,
    used       INTEGER NOT NULL DEFAULT 0,                 -- set by the first journal line
    version    INTEGER NOT NULL DEFAULT 1,
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
-- Once an account carries a posting its code, type and tax mapping are fixed, and it cannot be deleted:
-- the history it reports must keep meaning what it meant. The name and the active flag may change.
CREATE TRIGGER ledger_accounts_immutable BEFORE UPDATE OF code, type, tax_code ON ledger_accounts
    WHEN OLD.used = 1
BEGIN
    SELECT RAISE(ABORT, 'an account that has been used cannot change its code, type or tax code');
END;
CREATE TRIGGER ledger_accounts_no_delete BEFORE DELETE ON ledger_accounts
    WHEN OLD.used = 1 AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'an account that has been used cannot be deleted');
END;

-- Which account each automatic posting uses (keys: ar, income, tax_payable, cash, expense,
-- expense_payable, opening_equity). Automatic posting is on only when every key is mapped; the moment it
-- first turns on is the cut-over, recorded as one 'opening' entry per currency.
CREATE TABLE ledger_mappings (
    key        TEXT    PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES ledger_accounts (id),
    updated_by TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Close and reopen history. The latest row is in force; nothing is edited, so the history is the audit.
CREATE TABLE ledger_closes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    action         TEXT    NOT NULL CHECK (action IN ('close', 'reopen')),
    closed_through TEXT    NOT NULL,                       -- '' after a full reopen
    reason         TEXT    NOT NULL DEFAULT '',
    by             TEXT    NOT NULL,
    at             INTEGER NOT NULL
);
CREATE TRIGGER ledger_closes_no_update BEFORE UPDATE ON ledger_closes
BEGIN
    SELECT RAISE(ABORT, 'the close history is append-only');
END;
CREATE TRIGGER ledger_closes_no_delete BEFORE DELETE ON ledger_closes WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'the close history is append-only');
END;

-- One entry is in one currency, so "balanced" is the same as sum(debit) = sum(credit) in minor units.
-- Entries are inserted as drafts and flipped to posted by an UPDATE, which is where the database checks
-- the balance, the line count and the period lock.
CREATE TABLE journal_entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    status      TEXT    NOT NULL CHECK (status IN ('draft', 'posted')),
    source      TEXT    NOT NULL CHECK (source IN ('opening', 'manual', 'payroll', 'invoice', 'credit_note', 'invoice_void', 'payment', 'expense', 'expense_reimbursed', 'reversal')),
    source_ref  TEXT    NOT NULL DEFAULT '',
    entry_date  TEXT    NOT NULL,
    currency    TEXT    NOT NULL,
    memo        TEXT    NOT NULL DEFAULT '',
    number      INTEGER,
    number_text TEXT    UNIQUE,
    reversal_of INTEGER REFERENCES journal_entries (id),
    reversed_by INTEGER REFERENCES journal_entries (id),
    created_by  TEXT    NOT NULL,
    posted_by   TEXT,
    posted_at   INTEGER,
    created_at  INTEGER NOT NULL,
    CHECK ((status = 'draft') = (number IS NULL))
);
CREATE INDEX journal_entries_date ON journal_entries (entry_date, id);
CREATE UNIQUE INDEX journal_entries_number ON journal_entries (number) WHERE number IS NOT NULL;
-- A business event posts once, however many times it is retried.
CREATE UNIQUE INDEX journal_source_once ON journal_entries (source, source_ref) WHERE source_ref <> '' AND source <> 'reversal';
CREATE UNIQUE INDEX journal_reversal_once ON journal_entries (reversal_of) WHERE reversal_of IS NOT NULL;

CREATE TABLE journal_lines (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id     INTEGER NOT NULL REFERENCES journal_entries (id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    account_id   INTEGER NOT NULL REFERENCES ledger_accounts (id),
    debit_minor  INTEGER NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor INTEGER NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    memo         TEXT    NOT NULL DEFAULT '',
    UNIQUE (entry_id, position),
    CHECK ((debit_minor > 0) <> (credit_minor > 0))
);
CREATE INDEX journal_lines_account ON journal_lines (account_id, entry_id);

CREATE TRIGGER journal_lines_mark_used AFTER INSERT ON journal_lines
BEGIN
    UPDATE ledger_accounts SET used = 1 WHERE id = NEW.account_id AND used = 0;
END;
CREATE TRIGGER journal_entries_no_posted_insert BEFORE INSERT ON journal_entries WHEN NEW.status <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'entries are inserted as drafts and posted by update, so the balance is checked');
END;
CREATE TRIGGER journal_entries_post_check BEFORE UPDATE OF status ON journal_entries
    WHEN OLD.status = 'draft' AND NEW.status = 'posted'
BEGIN
    SELECT RAISE(ABORT, 'a journal entry needs at least two lines')
        WHERE (SELECT COUNT(*) FROM journal_lines WHERE entry_id = OLD.id) < 2;
    SELECT RAISE(ABORT, 'a journal entry must balance: debits equal credits')
        WHERE (SELECT COALESCE(SUM(debit_minor), 0) - COALESCE(SUM(credit_minor), 0) FROM journal_lines WHERE entry_id = OLD.id) <> 0;
    SELECT RAISE(ABORT, 'the period is closed')
        WHERE NEW.entry_date <= COALESCE((SELECT closed_through FROM ledger_closes ORDER BY id DESC LIMIT 1), '');
END;
-- A posted entry keeps its content. The only change allowed afterwards is the link to the entry that
-- reverses it, set once.
CREATE TRIGGER journal_entries_immutable BEFORE UPDATE ON journal_entries
    WHEN OLD.status = 'posted' AND (NEW.status <> OLD.status OR NEW.source <> OLD.source OR NEW.source_ref <> OLD.source_ref
        OR NEW.entry_date <> OLD.entry_date OR NEW.currency <> OLD.currency OR NEW.memo <> OLD.memo
        OR NEW.number IS NOT OLD.number OR NEW.number_text IS NOT OLD.number_text OR NEW.reversal_of IS NOT OLD.reversal_of
        OR NEW.created_by <> OLD.created_by OR NEW.posted_by IS NOT OLD.posted_by OR NEW.posted_at IS NOT OLD.posted_at
        OR (OLD.reversed_by IS NOT NULL AND NEW.reversed_by IS NOT OLD.reversed_by))
BEGIN
    SELECT RAISE(ABORT, 'posted journal entries are immutable: correct them with a reversal');
END;
CREATE TRIGGER journal_entries_no_delete BEFORE DELETE ON journal_entries
    WHEN OLD.status = 'posted' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'posted journal entries cannot be deleted before their retention period ends');
END;
CREATE TRIGGER journal_lines_no_insert BEFORE INSERT ON journal_lines
    WHEN (SELECT status FROM journal_entries WHERE id = NEW.entry_id) <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'lines of a posted journal entry are immutable');
END;
CREATE TRIGGER journal_lines_no_update BEFORE UPDATE ON journal_lines
    WHEN (SELECT status FROM journal_entries WHERE id = OLD.entry_id) <> 'draft'
        OR (SELECT status FROM journal_entries WHERE id = NEW.entry_id) <> 'draft'
BEGIN
    SELECT RAISE(ABORT, 'lines of a posted journal entry are immutable');
END;
CREATE TRIGGER journal_lines_no_delete BEFORE DELETE ON journal_lines
    WHEN (SELECT status FROM journal_entries WHERE id = OLD.entry_id) <> 'draft' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'lines of a posted journal entry are immutable');
END;

-- Bank accounts. The account number is sealed (XChaCha20-Poly1305, the row id is the additional data)
-- and only its last four characters are kept in the clear, for display and for matching a statement.
CREATE TABLE bank_accounts (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL,
    currency          TEXT    NOT NULL,
    ledger_account_id INTEGER NOT NULL REFERENCES ledger_accounts (id),
    sealed_number     TEXT    NOT NULL,
    last4             TEXT    NOT NULL,
    active            INTEGER NOT NULL DEFAULT 1,
    version           INTEGER NOT NULL DEFAULT 1,
    created_by        TEXT    NOT NULL,
    created_at        INTEGER NOT NULL
);

CREATE TABLE bank_statements (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    bank_account_id INTEGER NOT NULL REFERENCES bank_accounts (id),
    format          TEXT    NOT NULL CHECK (format IN ('csv', 'ofx', 'camt053')),
    sha256          TEXT    NOT NULL,
    size_bytes      INTEGER NOT NULL,
    txn_count       INTEGER NOT NULL,
    new_count       INTEGER NOT NULL,
    duplicate_count INTEGER NOT NULL,
    imported_by     TEXT    NOT NULL,
    imported_at     INTEGER NOT NULL,
    UNIQUE (bank_account_id, sha256)
);

-- amount_minor is signed from the account's point of view: money in is positive.
CREATE TABLE bank_transactions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    bank_account_id     INTEGER NOT NULL REFERENCES bank_accounts (id),
    statement_id        INTEGER NOT NULL REFERENCES bank_statements (id),
    txn_key             TEXT    NOT NULL,                  -- the bank's transaction id, else a hash of the row
    key_group           TEXT    NOT NULL DEFAULT '',       -- rows that differ only in occurrence number; '' for a bank id
    booked_on           TEXT    NOT NULL,
    amount_minor        INTEGER NOT NULL CHECK (amount_minor <> 0),
    currency            TEXT    NOT NULL,
    reference           TEXT    NOT NULL DEFAULT '',
    counterparty        TEXT    NOT NULL DEFAULT '',
    description         TEXT    NOT NULL DEFAULT '',
    status              TEXT    NOT NULL DEFAULT 'unmatched' CHECK (status IN ('unmatched', 'matched', 'ignored')),
    matched_payment_id  INTEGER REFERENCES payments (id),
    decided_by          TEXT,
    decided_at          INTEGER,
    ignore_reason       TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    UNIQUE (bank_account_id, txn_key)
);
CREATE INDEX bank_transactions_queue ON bank_transactions (bank_account_id, status, booked_on);
CREATE INDEX bank_transactions_statement ON bank_transactions (statement_id, key_group);
-- A payment is the money of at most one bank transaction.
CREATE UNIQUE INDEX bank_transactions_payment ON bank_transactions (matched_payment_id) WHERE matched_payment_id IS NOT NULL;
CREATE TRIGGER bank_transactions_keep_facts BEFORE UPDATE OF bank_account_id, statement_id, txn_key, key_group, booked_on, amount_minor, currency, reference, counterparty, description ON bank_transactions
BEGIN
    SELECT RAISE(ABORT, 'a bank transaction keeps what the bank reported');
END;
CREATE TRIGGER bank_transactions_decide_once BEFORE UPDATE ON bank_transactions
    WHEN OLD.status <> 'unmatched' AND (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'a bank transaction that was matched or ignored is final');
END;
CREATE TRIGGER bank_transactions_no_delete BEFORE DELETE ON bank_transactions WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'bank transactions cannot be deleted before their retention period ends');
END;

-- A sign-off that a month of a bank account is reconciled: nothing in it is still unmatched. Append-only: when a
-- later import adds rows to the month it is signed off again, and the latest row is the one in force.
CREATE TABLE bank_reconciliations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    bank_account_id INTEGER NOT NULL REFERENCES bank_accounts (id),
    period          TEXT    NOT NULL,                      -- YYYY-MM
    txn_count       INTEGER NOT NULL,
    matched_count   INTEGER NOT NULL,
    ignored_count   INTEGER NOT NULL,
    reconciled_by   TEXT    NOT NULL,
    reconciled_at   INTEGER NOT NULL
);
CREATE INDEX bank_reconciliations_period ON bank_reconciliations (bank_account_id, period);

-- Export batches: the cursor is the last posting number (journal_entries.number, assigned when an entry
-- posts, gap-free) handed to an adapter. Entry ids would not do: a draft posted after later entries were
-- exported has a lower id and would never be exported. So an entry is never exported twice nor skipped; the content is kept so a batch can be downloaded again byte for byte.
CREATE TABLE export_cursors (
    adapter       TEXT    PRIMARY KEY,
    last_number   INTEGER NOT NULL
);
CREATE TABLE export_batches (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    adapter     TEXT    NOT NULL,
    first_number INTEGER NOT NULL,
    last_number  INTEGER NOT NULL,
    entry_count INTEGER NOT NULL,
    sha256      TEXT    NOT NULL,
    content     BLOB    NOT NULL,
    created_by  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE TRIGGER export_batches_no_update BEFORE UPDATE ON export_batches
BEGIN
    SELECT RAISE(ABORT, 'export batches are append-only');
END;
CREATE TRIGGER export_batches_no_delete BEFORE DELETE ON export_batches WHEN (SELECT open FROM retention_gate) = 0
BEGIN
    SELECT RAISE(ABORT, 'export batches are append-only');
END;

-- A payroll file is imported once.
CREATE TABLE payroll_imports (
    sha256      TEXT    PRIMARY KEY,
    entry_ids   TEXT    NOT NULL,
    imported_by TEXT    NOT NULL,
    imported_at INTEGER NOT NULL
);

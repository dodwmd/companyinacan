-- equity service schema. Timestamps are unix seconds (UTC); dates are YYYY-MM-DD text; share and
-- option quantities are integers in 1/10000 of a share ("qty4"); prices are integer minor units.
-- Columns named sens_* hold XChaCha20-Poly1305 ciphertext (v1.<key-id>.<b64>), never plaintext.
-- Everything here belongs to a data class (modules/equity/module.yaml).

CREATE TABLE entities (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    jurisdiction  TEXT NOT NULL DEFAULT '',   -- free text; no jurisdiction rules are implemented
    currency      TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE share_classes (
    id          TEXT PRIMARY KEY,
    entity_id   TEXT NOT NULL REFERENCES entities (id),
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('common', 'preferred')),
    par_minor   INTEGER NOT NULL CHECK (par_minor >= 0),
    currency    TEXT NOT NULL,
    authorised  INTEGER NOT NULL CHECK (authorised > 0),
    created_at  INTEGER NOT NULL,
    UNIQUE (entity_id, name)
);

-- class equity-stakeholder (sens_*): the identity of a holder. The ledger refers to the id only.
CREATE TABLE stakeholders (
    id                TEXT PRIMARY KEY,
    entity_id         TEXT NOT NULL REFERENCES entities (id),
    kind              TEXT NOT NULL CHECK (kind IN ('founder', 'employee', 'investor', 'advisor')),
    person_id         TEXT,                  -- opaque directory person id (employees, founders, advisors)
    sens_name         TEXT,
    sens_email        TEXT,
    sens_tax_id       TEXT,
    legal_hold        INTEGER NOT NULL DEFAULT 0,
    legal_hold_reason TEXT NOT NULL DEFAULT '',
    erased_at         INTEGER,               -- identity pseudonymised; the ledger rows remain
    created_by        TEXT NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX stakeholders_person ON stakeholders (entity_id, person_id) WHERE person_id IS NOT NULL;
CREATE INDEX stakeholders_person_any ON stakeholders (person_id) WHERE person_id IS NOT NULL;

CREATE TABLE plans (
    id          TEXT PRIMARY KEY,
    entity_id   TEXT NOT NULL REFERENCES entities (id),
    class_id    TEXT NOT NULL REFERENCES share_classes (id),
    name        TEXT NOT NULL,
    authorised  INTEGER NOT NULL CHECK (authorised > 0),
    adopted_on  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed')),
    created_at  INTEGER NOT NULL,
    UNIQUE (entity_id, name)
);

-- class equity-grants (sens_notes) and equity-ledger (the terms are the record of what was granted).
-- Terms are frozen once the grant leaves draft (trigger below); amending means a new version.
CREATE TABLE grants (
    id                 TEXT PRIMARY KEY,
    entity_id          TEXT NOT NULL REFERENCES entities (id),
    plan_id            TEXT NOT NULL REFERENCES plans (id),
    stakeholder_id     TEXT NOT NULL REFERENCES stakeholders (id),
    type               TEXT NOT NULL CHECK (type IN ('ISO', 'NSO', 'RSU', 'SAR')),
    status             TEXT NOT NULL CHECK (status IN ('draft', 'pending_approval', 'approved', 'active', 'cancelled', 'forfeited', 'expired')),
    version            INTEGER NOT NULL DEFAULT 1,
    supersedes_id      TEXT REFERENCES grants (id),
    quantity           INTEGER NOT NULL CHECK (quantity > 0),
    currency           TEXT NOT NULL,
    strike_minor       INTEGER NOT NULL CHECK (strike_minor >= 0),
    grant_date         TEXT NOT NULL,
    vest_start         TEXT NOT NULL,
    schedule           TEXT NOT NULL,        -- JSON Schedule
    expiry_date        TEXT NOT NULL,
    ptep_days          INTEGER NOT NULL CHECK (ptep_days >= 0 AND ptep_days <= 3650),
    sens_notes         TEXT,
    created_by         TEXT NOT NULL,        -- canonical (case-folded NFKC) subjects: separation of duties
    edited_by          TEXT NOT NULL DEFAULT '[]',
    submitted_by       TEXT,
    approved_by        TEXT,
    approved_on        TEXT,
    approval_ref       TEXT NOT NULL DEFAULT '',
    activated_on       TEXT,
    termination_date   TEXT,
    for_cause          INTEGER NOT NULL DEFAULT 0,
    terminal_on        TEXT,                 -- date it became cancelled, forfeited or expired
    terminal_reason    TEXT NOT NULL DEFAULT '',
    exercised          INTEGER NOT NULL DEFAULT 0 CHECK (exercised >= 0),
    vested_notified    INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    CHECK (type != 'RSU' OR strike_minor = 0)
);
CREATE INDEX grants_stakeholder ON grants (stakeholder_id);
CREATE INDEX grants_plan ON grants (plan_id, status);
CREATE INDEX grants_entity_status ON grants (entity_id, status);
CREATE UNIQUE INDEX grants_one_supersede ON grants (supersedes_id) WHERE supersedes_id IS NOT NULL AND status != 'cancelled';

-- Recorded facts that feed the vesting calculator. Append-only.
CREATE TABLE grant_milestones (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    grant_id    TEXT NOT NULL REFERENCES grants (id),
    tranche_idx INTEGER NOT NULL,
    achieved_on TEXT NOT NULL,
    recorded_by TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (grant_id, tranche_idx)
);
CREATE TABLE grant_accel_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    grant_id    TEXT NOT NULL REFERENCES grants (id),
    trigger     TEXT NOT NULL,
    occurred_on TEXT NOT NULL,
    recorded_by TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (grant_id, trigger)
);

-- exercise requests: requested -> approved -> payment_recorded -> issued (or rejected / cancelled).
CREATE TABLE exercises (
    id                 TEXT PRIMARY KEY,
    entity_id          TEXT NOT NULL REFERENCES entities (id),
    grant_id           TEXT NOT NULL REFERENCES grants (id),
    stakeholder_id     TEXT NOT NULL REFERENCES stakeholders (id),
    quantity           INTEGER NOT NULL CHECK (quantity > 0),
    currency           TEXT NOT NULL,
    due_minor          INTEGER NOT NULL,     -- quantity x strike, computed once at request time
    status             TEXT NOT NULL CHECK (status IN ('requested', 'approved', 'payment_recorded', 'issued', 'rejected', 'cancelled')),
    requested_by       TEXT NOT NULL,        -- canonical subject
    approved_by        TEXT,
    paid_minor         INTEGER,
    paid_on            TEXT,
    payment_by         TEXT,
    sens_payment_ref   TEXT,
    issued_by          TEXT,
    issued_on          TEXT,
    ledger_id          TEXT,
    approval_ref       TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);
CREATE INDEX exercises_grant ON exercises (grant_id, status);
CREATE INDEX exercises_entity ON exercises (entity_id, status);

-- The equity ledger: append-only (triggers in 002). Positive quantity adds to the holder's
-- balance, negative removes. Holders appear as stakeholder ids only, never names.
CREATE TABLE ledger (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    id              TEXT NOT NULL UNIQUE,
    entity_id       TEXT NOT NULL REFERENCES entities (id),
    class_id        TEXT NOT NULL REFERENCES share_classes (id),
    stakeholder_id  TEXT NOT NULL REFERENCES stakeholders (id),
    kind            TEXT NOT NULL CHECK (kind IN ('issuance', 'exercise', 'transfer_out', 'transfer_in', 'cancellation', 'repurchase')),
    effective_date  TEXT NOT NULL,
    quantity        INTEGER NOT NULL CHECK (quantity != 0),
    price_minor     INTEGER,                 -- per share paid, integer minor units
    currency        TEXT NOT NULL DEFAULT '',
    group_id        TEXT NOT NULL,           -- both legs of a transfer share one group
    ref_grant_id    TEXT,
    ref_exercise_id TEXT,
    recorded_by     TEXT NOT NULL,
    recorded_at     INTEGER NOT NULL
);
CREATE INDEX ledger_entity_date ON ledger (entity_id, effective_date);
CREATE INDEX ledger_holder ON ledger (stakeholder_id, class_id);
CREATE INDEX ledger_grant ON ledger (ref_grant_id) WHERE ref_grant_id IS NOT NULL;

-- Convertible instruments (SAFE, convertible note) recorded as data; conversion is only ever
-- modelled (what-if), never executed. Terms are sealed: they identify an investor's money.
CREATE TABLE convertibles (
    id              TEXT PRIMARY KEY,
    entity_id       TEXT NOT NULL REFERENCES entities (id),
    stakeholder_id  TEXT NOT NULL REFERENCES stakeholders (id),
    kind            TEXT NOT NULL CHECK (kind IN ('safe_pre_money', 'safe_post_money', 'convertible_note')),
    issued_on       TEXT NOT NULL,
    sens_terms      TEXT NOT NULL,           -- JSON: principal, currency, cap, discount, interest
    status          TEXT NOT NULL DEFAULT 'outstanding' CHECK (status IN ('outstanding', 'converted', 'cancelled')),
    created_by      TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);

-- 409A and other valuation records, kept as data. Append-only apart from the sealed columns.
CREATE TABLE valuations (
    id             TEXT PRIMARY KEY,
    entity_id      TEXT NOT NULL REFERENCES entities (id),
    class_id       TEXT REFERENCES share_classes (id),
    kind           TEXT NOT NULL CHECK (kind IN ('409a', 'independent', 'board', 'other')),
    effective_date TEXT NOT NULL,
    expires_on     TEXT NOT NULL DEFAULT '',
    sens_fmv       TEXT,                     -- JSON: price_minor + currency per share
    sens_notes     TEXT,
    created_by     TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);

-- Idempotency: a repeated create with the same key and body returns the first result; the same
-- key with a different body is a conflict. Only the id of the created record is kept, so the
-- table holds no personal data and a replay re-reads the record under the caller's current rights.
CREATE TABLE idempotency (
    key         TEXT NOT NULL,
    actor       TEXT NOT NULL,
    route       TEXT NOT NULL,
    body_hash   TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (actor, route, key)
);

CREATE TABLE audit_log (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    at       INTEGER NOT NULL,
    actor    TEXT NOT NULL,
    action   TEXT NOT NULL,
    resource TEXT NOT NULL,
    fields   TEXT NOT NULL DEFAULT '',
    outcome  TEXT NOT NULL,
    prev     TEXT NOT NULL,
    hash     TEXT NOT NULL
);

-- Erasure tombstones (pulled into the E08 ledger by can-core); subject is a stakeholder id.
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT NOT NULL,
    class      TEXT NOT NULL,
    date       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (subject, class)
);

CREATE TABLE outbox (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL,
    payload      TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT 0,
    delivered_at INTEGER,
    last_error   TEXT NOT NULL DEFAULT ''
);

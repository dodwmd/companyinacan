-- people service schema. Timestamps are unix seconds (UTC); dates are YYYY-MM-DD text.
-- Columns named sens_* and *_sealed hold XChaCha20-Poly1305 ciphertext (v1.<key-id>.<b64>),
-- never plaintext. Everything here belongs to a data class (modules/people/module.yaml).

CREATE TABLE departments (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE teams (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    department_id TEXT REFERENCES departments (id),
    lead_id       TEXT,
    created_at    INTEGER NOT NULL
);

CREATE TABLE positions (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL UNIQUE,
    department_id TEXT REFERENCES departments (id),
    created_at    INTEGER NOT NULL
);

-- class directory-basic (columns up to end_date) and people-sensitive (sens_*).
CREATE TABLE people (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT    NOT NULL UNIQUE,
    kind              TEXT    NOT NULL CHECK (kind IN ('employee', 'contractor')),
    status            TEXT    NOT NULL CHECK (status IN ('candidate', 'onboarding', 'active', 'leave', 'offboarding', 'former')),
    sso_subject       TEXT UNIQUE,
    given_name        TEXT    NOT NULL DEFAULT '',
    family_name       TEXT    NOT NULL DEFAULT '',
    preferred_name    TEXT    NOT NULL DEFAULT '',
    work_email        TEXT    NOT NULL DEFAULT '',
    work_phone        TEXT    NOT NULL DEFAULT '',
    location          TEXT    NOT NULL DEFAULT '',
    department_id     TEXT REFERENCES departments (id),
    team_id           TEXT REFERENCES teams (id),
    position_id       TEXT REFERENCES positions (id),
    manager_id        TEXT REFERENCES people (id),
    start_date        TEXT    NOT NULL DEFAULT '',
    end_date          TEXT    NOT NULL DEFAULT '',
    sens_dob          TEXT,
    sens_national_id  TEXT,
    sens_address      TEXT,
    sens_phone        TEXT,
    sens_salary       TEXT,
    legal_hold        INTEGER NOT NULL DEFAULT 0,
    legal_hold_reason TEXT    NOT NULL DEFAULT '',
    ended_at          INTEGER,           -- when the person became former (retention clock)
    erased_at         INTEGER,           -- set by an erasure request: the row is a tombstone
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX people_manager ON people (manager_id);
CREATE INDEX people_status ON people (status);

-- Effective-dated status changes. applied=0 rows are future-dated and wait for ApplyDue.
CREATE TABLE status_history (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id      TEXT    NOT NULL REFERENCES people (id),
    from_status    TEXT    NOT NULL,
    to_status      TEXT    NOT NULL,
    effective_date TEXT    NOT NULL,
    applied        INTEGER NOT NULL DEFAULT 0,
    recorded_at    INTEGER NOT NULL,
    recorded_by    TEXT    NOT NULL,
    note           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX status_history_person ON status_history (person_id, seq);
-- at most one pending change per person
CREATE UNIQUE INDEX status_history_pending ON status_history (person_id) WHERE applied = 0;

-- class people-checklists
CREATE TABLE checklist_templates (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK (kind IN ('onboarding', 'offboarding')),
    auto_status TEXT NOT NULL DEFAULT '',   -- instantiate automatically when a person enters this status
    items       TEXT NOT NULL,              -- JSON array of template items
    created_at  INTEGER NOT NULL
);

CREATE TABLE checklists (
    id          TEXT PRIMARY KEY,
    person_id   TEXT NOT NULL REFERENCES people (id),
    template_id TEXT NOT NULL,
    kind        TEXT NOT NULL,
    anchor_date TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    created_by  TEXT NOT NULL
);
CREATE INDEX checklists_person ON checklists (person_id);

CREATE TABLE checklist_tasks (
    id             TEXT PRIMARY KEY,
    checklist_id   TEXT NOT NULL REFERENCES checklists (id) ON DELETE CASCADE,
    person_id      TEXT NOT NULL,
    ord            INTEGER NOT NULL,
    title          TEXT NOT NULL,
    assignee_role  TEXT NOT NULL,           -- self | manager | hr | admin
    assignee_id    TEXT,                    -- resolved person, when there is one
    due_date       TEXT NOT NULL,
    action         TEXT NOT NULL DEFAULT '',-- machine-readable hook, e.g. identity.provision
    done_at        INTEGER,
    done_by        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX tasks_assignee ON checklist_tasks (assignee_id, done_at);
CREATE INDEX tasks_person ON checklist_tasks (person_id);

-- class performance-oneonone. Notes are sealed (the manager-private notes are
-- application-level encrypted, SPEC 5 planned hardening).
CREATE TABLE oneonone_series (
    id         TEXT PRIMARY KEY,
    manager_id TEXT NOT NULL REFERENCES people (id),
    report_id  TEXT NOT NULL REFERENCES people (id),
    cadence    TEXT NOT NULL CHECK (cadence IN ('weekly', 'biweekly', 'monthly')),
    created_at INTEGER NOT NULL,
    UNIQUE (manager_id, report_id)
);
CREATE INDEX series_manager ON oneonone_series (manager_id);
CREATE INDEX series_report ON oneonone_series (report_id);

CREATE TABLE oneonone_meetings (
    id                  TEXT PRIMARY KEY,
    series_id           TEXT NOT NULL REFERENCES oneonone_series (id) ON DELETE CASCADE,
    scheduled_at        INTEGER NOT NULL,
    status              TEXT NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled', 'held', 'cancelled')),
    shared_notes_sealed TEXT,
    private_notes_sealed TEXT,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
CREATE INDEX meetings_series ON oneonone_meetings (series_id, scheduled_at);

CREATE TABLE agenda_items (
    id         TEXT PRIMARY KEY,
    meeting_id TEXT NOT NULL REFERENCES oneonone_meetings (id) ON DELETE CASCADE,
    author_id  TEXT NOT NULL,
    text       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE action_items (
    id            TEXT PRIMARY KEY,
    meeting_id    TEXT NOT NULL REFERENCES oneonone_meetings (id) ON DELETE CASCADE,
    text          TEXT NOT NULL,
    assignee_id   TEXT NOT NULL,
    due_date      TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done')),
    work_issue_id TEXT NOT NULL DEFAULT '',  -- link to a `work` issue, set when one exists
    created_by    TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

-- class performance-goals
CREATE TABLE goals (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL CHECK (scope IN ('person', 'team', 'company')),
    owner_id    TEXT REFERENCES people (id),
    team_id     TEXT REFERENCES teams (id),
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    period      TEXT NOT NULL DEFAULT '',
    visibility  TEXT NOT NULL CHECK (visibility IN ('private', 'manager', 'team', 'company')),
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('draft', 'active', 'achieved', 'abandoned')),
    parent_id   TEXT REFERENCES goals (id),
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX goals_owner ON goals (owner_id);
CREATE INDEX goals_parent ON goals (parent_id);

CREATE TABLE key_results (
    id            TEXT PRIMARY KEY,
    goal_id       TEXT NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    unit          TEXT NOT NULL DEFAULT '',
    start_value   REAL NOT NULL DEFAULT 0,
    target_value  REAL NOT NULL,
    current_value REAL NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

CREATE TABLE goal_updates (
    id         TEXT PRIMARY KEY,
    goal_id    TEXT NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    kr_id      TEXT,
    author_id  TEXT NOT NULL,
    value      REAL,
    comment    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Erasure tombstones: opaque subject id, data class, date. No PII. Pulled by can-core
-- (GET /v1/erasures) into the E08 erasure ledger (core/backup/ledger.go Entry).
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    date       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (subject, class)
);

-- Events waiting for delivery (at-least-once). Payloads carry ids, never names or contact data.
CREATE TABLE outbox (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL,
    payload      TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT 0,
    delivered_at INTEGER,
    last_error   TEXT    NOT NULL DEFAULT ''
);

-- Append-only, hash-chained audit trail of sensitive reads and all writes.
CREATE TABLE audit_log (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    at       INTEGER NOT NULL,
    actor    TEXT    NOT NULL,
    action   TEXT    NOT NULL,
    resource TEXT    NOT NULL,
    fields   TEXT    NOT NULL DEFAULT '',
    outcome  TEXT    NOT NULL,
    prev     TEXT    NOT NULL,
    hash     TEXT    NOT NULL
);

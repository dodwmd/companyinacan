-- can-jobs schema. Timestamps are unix seconds (UTC). Columns ending in _sealed hold
-- XChaCha20-Poly1305 ciphertext (v1.<key-id>.<base64url>), never plaintext.

CREATE TABLE stages (
    key      TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    kind     TEXT NOT NULL CHECK (kind IN ('initial','active','offer','hired')),
    position INTEGER NOT NULL
);

CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,
    slug                TEXT NOT NULL UNIQUE,
    title               TEXT NOT NULL,
    department          TEXT NOT NULL DEFAULT '',
    city                TEXT NOT NULL DEFAULT '',
    region              TEXT NOT NULL DEFAULT '',
    country             TEXT NOT NULL DEFAULT '',
    postal_code         TEXT NOT NULL DEFAULT '',
    remote              TEXT NOT NULL CHECK (remote IN ('onsite','hybrid','remote')),
    applicant_countries TEXT NOT NULL DEFAULT '[]',
    employment_type     TEXT NOT NULL,
    salary_min          REAL,
    salary_max          REAL,
    salary_currency     TEXT NOT NULL DEFAULT '',
    salary_period       TEXT NOT NULL DEFAULT '',
    publish_salary      INTEGER NOT NULL DEFAULT 0,
    sections            TEXT NOT NULL,
    hiring_manager      TEXT NOT NULL DEFAULT '',
    headcount           INTEGER NOT NULL DEFAULT 1,
    status              TEXT NOT NULL CHECK (status IN ('draft','open','paused','closed')),
    approval            TEXT NOT NULL DEFAULT 'none' CHECK (approval IN ('none','pending','approved','rejected')),
    approval_requested_by TEXT NOT NULL DEFAULT '',
    approval_ref        TEXT NOT NULL DEFAULT '',
    valid_through       INTEGER,
    opened_at           INTEGER,
    closed_at           INTEGER,
    close_reason        TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    version             INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX jobs_status ON jobs (status, updated_at);
CREATE INDEX jobs_manager ON jobs (hiring_manager);

-- One row per job and board: what the connector said last.
CREATE TABLE syndication (
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    board        TEXT NOT NULL,
    external_ref TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    last_error   TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (job_id, board)
);

CREATE TABLE candidates (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    email_sealed     TEXT,
    email_hash       TEXT NOT NULL UNIQUE,        -- HMAC-SHA256 of the lower-cased address; 'erased:<id>' once erased
    phone_sealed     TEXT,
    address_sealed   TEXT,
    salary_exp_sealed TEXT,
    notes_sealed     TEXT,
    tags             TEXT NOT NULL DEFAULT '[]',
    source           TEXT NOT NULL DEFAULT '',
    consent_at       INTEGER NOT NULL,
    consent_version  TEXT NOT NULL,
    talent_pool      INTEGER NOT NULL DEFAULT 0,
    talent_consent_at INTEGER,
    retain_until     INTEGER,                      -- NULL while any application is still active
    legal_hold       INTEGER NOT NULL DEFAULT 0,
    erased_at        INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX candidates_retain ON candidates (retain_until);
CREATE INDEX candidates_pool ON candidates (talent_pool) WHERE talent_pool = 1;

CREATE TABLE applications (
    id                 TEXT PRIMARY KEY,
    candidate_id       TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
    job_id             TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    stage              TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('active','rejected','withdrawn','hired')),
    reject_reason      TEXT NOT NULL DEFAULT '',
    reject_note_sealed TEXT,
    cover_letter_sealed TEXT,
    source             TEXT NOT NULL DEFAULT '',
    resume_key         TEXT NOT NULL DEFAULT '',
    resume_name        TEXT NOT NULL DEFAULT '',
    resume_type        TEXT NOT NULL DEFAULT '',
    resume_size        INTEGER NOT NULL DEFAULT 0,
    resume_sha256      TEXT NOT NULL DEFAULT '',
    resume_scan        TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    decided_at         INTEGER,
    UNIQUE (candidate_id, job_id)
);
CREATE INDEX applications_job ON applications (job_id, status, stage);
CREATE INDEX applications_candidate ON applications (candidate_id);

CREATE TABLE stage_history (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id TEXT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    from_stage     TEXT NOT NULL DEFAULT '',
    to_stage       TEXT NOT NULL,
    status         TEXT NOT NULL,
    actor          TEXT NOT NULL,
    at             INTEGER NOT NULL
);
CREATE INDEX stage_history_app ON stage_history (application_id, seq);

CREATE TABLE interviewers (
    application_id TEXT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    subject        TEXT NOT NULL,
    assigned_by    TEXT NOT NULL,
    at             INTEGER NOT NULL,
    PRIMARY KEY (application_id, subject)
);
CREATE INDEX interviewers_subject ON interviewers (subject);

CREATE TABLE scorecards (
    id              TEXT PRIMARY KEY,
    application_id  TEXT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    author          TEXT NOT NULL,
    rating          INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 4),
    recommendation  TEXT NOT NULL,
    comments_sealed TEXT,
    status          TEXT NOT NULL CHECK (status IN ('draft','submitted')),
    submitted_at    INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (application_id, author)
);

-- Voluntary diversity responses. Deliberately NOT linked to a candidate or an application:
-- there is no foreign key, no id and no timestamp finer than a month, so no query can tie a
-- response to a person and no interviewer-facing path touches this table. Only aggregates
-- (with small cells suppressed) are ever read.
CREATE TABLE eeo_responses (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id     TEXT NOT NULL,
    month      TEXT NOT NULL,
    gender     TEXT NOT NULL DEFAULT '',
    ethnicity  TEXT NOT NULL DEFAULT '',
    veteran    TEXT NOT NULL DEFAULT '',
    disability TEXT NOT NULL DEFAULT ''
);
CREATE INDEX eeo_job ON eeo_responses (job_id);

CREATE TABLE api_tokens (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    subject     TEXT NOT NULL,
    name        TEXT NOT NULL,
    secret_hash BLOB NOT NULL,
    role        TEXT NOT NULL,
    jobs        TEXT NOT NULL DEFAULT '[]',
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER,
    revoked_at  INTEGER,
    last_used_at INTEGER
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

-- Erasure tombstones in the shape core/backup's ledger takes: opaque subject, data class, date.
CREATE TABLE erasures (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT NOT NULL,
    class      TEXT NOT NULL,
    date       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (subject, class)
);

CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL,
    payload      TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    next_attempt INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    delivered_at INTEGER,
    dead_at      INTEGER,
    last_error   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_due ON outbox (next_attempt) WHERE delivered_at IS NULL AND dead_at IS NULL;

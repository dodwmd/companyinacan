-- board service schema (modules `board` and `entity` share one database). Timestamps are unix seconds (UTC);
-- dates are YYYY-MM-DD text; counts are integers. Columns named sens_* hold XChaCha20-Poly1305 ciphertext
-- (v1.<key-id>.<b64>) bound to their row and column, never plaintext. Everything here belongs to a data
-- class (modules/board/module.yaml). People are opaque directory person ids: no names are stored here.

CREATE TABLE bodies (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    kind                    TEXT NOT NULL CHECK (kind IN ('board', 'committee')),
    parent_id               TEXT REFERENCES bodies (id),
    entity_ref              TEXT NOT NULL DEFAULT '',           -- opaque legal entity id (entity module)
    quorum_num              INTEGER NOT NULL CHECK (quorum_num >= 0),
    quorum_den              INTEGER NOT NULL CHECK (quorum_den >= 1 AND quorum_num <= quorum_den),
    quorum_min              INTEGER NOT NULL DEFAULT 0 CHECK (quorum_min >= 0),
    quorum_excludes_recused INTEGER NOT NULL DEFAULT 1,
    pass_num                INTEGER NOT NULL CHECK (pass_num >= 0),
    pass_den                INTEGER NOT NULL CHECK (pass_den >= 1 AND pass_num <= pass_den),
    pass_inclusive          INTEGER NOT NULL DEFAULT 0,
    notice_days             INTEGER NOT NULL DEFAULT 0 CHECK (notice_days BETWEEN 0 AND 366),
    notice_basis            TEXT NOT NULL DEFAULT '',           -- free label of where the rule comes from; no rule is implemented
    created_by              TEXT NOT NULL,
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL
);

-- Terms: members are never deleted, a term is ended. class board-private (person ids only).
CREATE TABLE body_members (
    id         TEXT PRIMARY KEY,
    body_id    TEXT NOT NULL REFERENCES bodies (id),
    person_id  TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('chair', 'member', 'secretary', 'observer')),
    start_date TEXT NOT NULL,
    end_date   TEXT,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    ended_by   TEXT
);
CREATE UNIQUE INDEX body_members_open ON body_members (body_id, person_id) WHERE end_date IS NULL;
CREATE UNIQUE INDEX body_members_one_chair ON body_members (body_id) WHERE end_date IS NULL AND role = 'chair';
CREATE INDEX body_members_person ON body_members (person_id);

CREATE TABLE meetings (
    id                  TEXT PRIMARY KEY,
    body_id             TEXT NOT NULL REFERENCES bodies (id),
    title               TEXT NOT NULL,
    starts_on           TEXT NOT NULL,
    starts_time         TEXT NOT NULL DEFAULT '',
    mode                TEXT NOT NULL CHECK (mode IN ('in-person', 'remote', 'hybrid')),
    status              TEXT NOT NULL CHECK (status IN ('scheduled', 'held', 'cancelled')),
    notice_given_on     TEXT NOT NULL,
    notice_days_required INTEGER NOT NULL,
    short_notice_waiver_ref TEXT NOT NULL DEFAULT '',            -- opaque ref of the recorded waiver when notice was short
    pack_issued_on      TEXT,
    created_by          TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
CREATE INDEX meetings_body ON meetings (body_id, starts_on);

CREATE TABLE meeting_invitees (
    meeting_id TEXT NOT NULL REFERENCES meetings (id),
    person_id  TEXT NOT NULL,
    capacity   TEXT NOT NULL CHECK (capacity IN ('attendee', 'officer')),
    added_by   TEXT NOT NULL,
    PRIMARY KEY (meeting_id, person_id)
) WITHOUT ROWID;

CREATE TABLE agenda_items (
    id            TEXT PRIMARY KEY,
    meeting_id    TEXT NOT NULL REFERENCES meetings (id),
    position      INTEGER NOT NULL CHECK (position >= 1),
    title         TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('information', 'discussion', 'resolution', 'closed')),
    resolution_id TEXT,
    created_by    TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (meeting_id, position)
);

CREATE TABLE attendance (
    meeting_id  TEXT NOT NULL REFERENCES meetings (id),
    person_id   TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('present', 'remote', 'apology', 'absent')),
    recorded_by TEXT NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (meeting_id, person_id)
) WITHOUT ROWID;

-- Board pack: document references only (no bytes). class board-packs.
CREATE TABLE pack_items (
    id            TEXT PRIMARY KEY,
    meeting_id    TEXT NOT NULL REFERENCES meetings (id),
    doc_ref       TEXT NOT NULL,
    sens_title    TEXT,
    sens_note     TEXT,
    media_type    TEXT NOT NULL DEFAULT '',
    size_bytes    INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    sha256        TEXT NOT NULL DEFAULT '',
    position      INTEGER NOT NULL,
    supplementary INTEGER NOT NULL DEFAULT 0,                    -- added after the pack was issued
    added_by      TEXT NOT NULL,
    added_at      INTEGER NOT NULL,
    UNIQUE (meeting_id, doc_ref)
);

-- class board-resolutions. Status moves forward only; a closed row is frozen by trigger.
CREATE TABLE resolutions (
    id               TEXT PRIMARY KEY,
    body_id          TEXT NOT NULL REFERENCES bodies (id),
    meeting_id       TEXT REFERENCES meetings (id),
    kind             TEXT NOT NULL CHECK (kind IN ('meeting', 'consent')),
    secret_ballot    INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL CHECK (status IN ('draft', 'proposed', 'voting', 'passed', 'failed', 'withdrawn')),
    current_version  INTEGER NOT NULL DEFAULT 1,
    proposer_ref     TEXT NOT NULL,                              -- canonical subject of the proposer (separation of duties)
    proposer_person  TEXT NOT NULL DEFAULT '',
    pass_num         INTEGER NOT NULL,
    pass_den         INTEGER NOT NULL,
    pass_inclusive   INTEGER NOT NULL,
    consent_deadline TEXT NOT NULL DEFAULT '',
    external_type    TEXT NOT NULL DEFAULT '',                   -- e.g. equity.grant: what asked for this resolution
    external_ref     TEXT NOT NULL DEFAULT '',
    supersedes_id    TEXT,
    eligible_count   INTEGER NOT NULL DEFAULT 0,
    quorum_required  INTEGER NOT NULL DEFAULT 0,
    quorum_present   INTEGER NOT NULL DEFAULT 0,
    result_for       INTEGER NOT NULL DEFAULT 0,
    result_against   INTEGER NOT NULL DEFAULT 0,
    result_abstain   INTEGER NOT NULL DEFAULT 0,
    close_reason     TEXT NOT NULL DEFAULT '' CHECK (close_reason IN ('', 'tally', 'all_signed', 'declined', 'deadline', 'withdrawn', 'proposer_sole_approver', 'no_quorum')),
    opened_by        TEXT NOT NULL DEFAULT '',
    opened_at        INTEGER,
    closed_by        TEXT NOT NULL DEFAULT '',
    closed_on        TEXT,
    created_by       TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX resolutions_body ON resolutions (body_id, created_at);
CREATE UNIQUE INDEX resolutions_external ON resolutions (external_type, external_ref) WHERE external_type != '' AND status != 'withdrawn';

CREATE TABLE resolution_subjects (
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    person_id     TEXT NOT NULL,
    PRIMARY KEY (resolution_id, person_id)
) WITHOUT ROWID;

-- Amendments create versions; old versions are kept. Title and text are sealed (board confidential).
CREATE TABLE resolution_versions (
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    version       INTEGER NOT NULL,
    sens_title    TEXT NOT NULL,
    sens_text     TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,                                -- sha256(id|version|title|text): integrity check on every open
    created_by    TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (resolution_id, version)
) WITHOUT ROWID;

-- Declared interests. class board-private (the nature is sealed).
CREATE TABLE conflicts (
    id            TEXT PRIMARY KEY,
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    person_id     TEXT NOT NULL,
    treatment     TEXT NOT NULL CHECK (treatment IN ('recuse', 'abstain', 'disclosed')),
    sens_nature   TEXT,
    declared_by   TEXT NOT NULL,
    declared_at   INTEGER NOT NULL,
    UNIQUE (resolution_id, person_id)
);

-- The people who may vote, fixed when voting opens (members in term, minus subjects and recused).
CREATE TABLE resolution_voters (
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    person_id     TEXT NOT NULL,
    PRIMARY KEY (resolution_id, person_id)
) WITHOUT ROWID;

-- Open votes and written-consent signatures. Append-only, only while the resolution is voting.
CREATE TABLE votes (
    id            TEXT PRIMARY KEY,
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    voter_person  TEXT NOT NULL,
    voter_ref     TEXT NOT NULL,
    choice        TEXT NOT NULL CHECK (choice IN ('for', 'against', 'abstain')),
    method        TEXT NOT NULL CHECK (method IN ('roll_call', 'consent')),
    signature_ref TEXT NOT NULL DEFAULT '',                      -- opaque ref of the e-signature record (consent)
    cast_at       INTEGER NOT NULL,
    UNIQUE (resolution_id, voter_person)
);

-- Secret ballot: who voted (receipt) and what was voted (ballot) are in separate tables with no link and
-- no timestamps or insertion order on the ballots (WITHOUT ROWID, random key), so the choice cannot be
-- tied to a person. The choice is sealed; the tally is computed when voting closes.
CREATE TABLE ballots (
    id            TEXT PRIMARY KEY,
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    sens_choice   TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE vote_receipts (
    resolution_id TEXT NOT NULL REFERENCES resolutions (id),
    voter_key     TEXT NOT NULL,
    PRIMARY KEY (resolution_id, voter_key)
) WITHOUT ROWID;

-- Minutes: statutory records. class board-minutes. An approved version is immutable; a correction is a new version.
CREATE TABLE minutes (
    id            TEXT PRIMARY KEY,
    meeting_id    TEXT NOT NULL REFERENCES meetings (id),
    version       INTEGER NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('draft', 'approved', 'superseded')),
    sens_content  TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,                                -- sha256(id|content): integrity check, frozen once approved
    author_ref    TEXT NOT NULL,                                 -- canonical subject of the drafter
    author_person TEXT NOT NULL DEFAULT '',
    edited_by     TEXT NOT NULL DEFAULT '',                      -- canonical subjects of everyone who edited the draft, comma separated
    approved_by   TEXT NOT NULL DEFAULT '',
    approved_on   TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (meeting_id, version)
);

-- Action items link to `work` issues by opaque id. The text is sealed.
CREATE TABLE action_items (
    id              TEXT PRIMARY KEY,
    meeting_id      TEXT NOT NULL REFERENCES meetings (id),
    resolution_id   TEXT,
    sens_text       TEXT NOT NULL,
    assignee_person TEXT NOT NULL DEFAULT '',
    work_issue_ref  TEXT NOT NULL DEFAULT '',
    due_on          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('open', 'done', 'cancelled')),
    created_by      TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- Legal hold on a person: blocks erasure and retention purges of their private data.
CREATE TABLE person_holds (
    person_id TEXT PRIMARY KEY,
    reason    TEXT NOT NULL DEFAULT '',
    placed_by TEXT NOT NULL,
    placed_at INTEGER NOT NULL
);

CREATE TABLE idempotency (
    key         TEXT NOT NULL,
    actor       TEXT NOT NULL,
    route       TEXT NOT NULL,
    body_hash   TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (actor, route, key)
);

-- class board-audit: append-only, hash-chained; ids and field names only.
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

-- Erasure tombstones (pulled into the E08 ledger by can-core); subject is a directory person id.
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

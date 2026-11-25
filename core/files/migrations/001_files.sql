-- can-files schema. Timestamps are unix seconds (UTC). Columns ending in _sealed hold
-- XChaCha20-Poly1305 ciphertext (v1.<key-id>.<base64url>), never plaintext. File bytes are never in
-- this database: blob_key names an object in the backend (a random key, never a user-supplied path).

CREATE TABLE spaces (
    id          TEXT PRIMARY KEY,
    name_sealed TEXT NOT NULL,
    owner       TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX spaces_owner ON spaces (owner);

-- principal is "user|<subject>" or "group|<opaque directory group id>".
CREATE TABLE space_members (
    space_id  TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    principal TEXT NOT NULL,
    role      TEXT NOT NULL CHECK (role IN ('viewer','editor','manager')),
    added_by  TEXT NOT NULL,
    added_at  INTEGER NOT NULL,
    PRIMARY KEY (space_id, principal)
);
CREATE INDEX space_members_principal ON space_members (principal, space_id);

CREATE TABLE folders (
    id          TEXT PRIMARY KEY,
    space_id    TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    parent_id   TEXT REFERENCES folders(id),
    depth       INTEGER NOT NULL,
    name_sealed TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX folders_space ON folders (space_id, id);
CREATE INDEX folders_parent ON folders (parent_id);

CREATE TABLE files (
    id                 TEXT PRIMARY KEY,
    space_id           TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    folder_id          TEXT REFERENCES folders(id),
    name_sealed        TEXT,
    description_sealed TEXT,
    ext                TEXT NOT NULL,
    -- opaque ids that let other modules and erasure find a file: never names or contact details
    subject            TEXT NOT NULL DEFAULT '',
    ref                TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL CHECK (status IN ('active','deleted','purging','purged')),
    -- set when a purge is claimed, so an interrupted one can be finished
    purge_reason       TEXT NOT NULL DEFAULT '',
    purge_actor        TEXT NOT NULL DEFAULT '',
    purge_action       TEXT NOT NULL DEFAULT '',
    current_version    INTEGER NOT NULL,
    rev                INTEGER NOT NULL DEFAULT 1,
    legal_hold         INTEGER NOT NULL DEFAULT 0,
    retain_until       INTEGER,
    created_by         TEXT NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    deleted_at         INTEGER,
    erased_at          INTEGER
);
CREATE INDEX files_space ON files (space_id, status, id);
CREATE INDEX files_folder ON files (folder_id);
CREATE INDEX files_ref ON files (ref) WHERE ref != '';
CREATE INDEX files_subject ON files (subject) WHERE subject != '';
CREATE INDEX files_deleted ON files (deleted_at) WHERE status = 'deleted';
CREATE INDEX files_purging ON files (id) WHERE status = 'purging';

-- Versions are immutable: a new upload is a new row; nothing rewrites an old one except a purge,
-- which blanks the blob reference and checksum.
CREATE TABLE versions (
    file_id      TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    n            INTEGER NOT NULL,
    blob_key     TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    content_type TEXT NOT NULL,
    scan         TEXT NOT NULL CHECK (scan IN ('clean','skipped','')),
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    purged_at    INTEGER,
    PRIMARY KEY (file_id, n)
);

-- A share link is its row plus an HMAC the token carries; revoking is setting revoked_at.
CREATE TABLE shares (
    id           TEXT PRIMARY KEY,
    file_id      TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    label_sealed TEXT,
    expires_at   INTEGER NOT NULL,
    one_time     INTEGER NOT NULL DEFAULT 0,
    uses         INTEGER NOT NULL DEFAULT 0,
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    revoked_at   INTEGER
);
CREATE INDEX shares_file ON shares (file_id);

-- A blob is registered here before it is written and released in the same transaction that
-- records its version: a crash in between leaves a row the sweep turns into a deleted blob.
CREATE TABLE pending_blobs (
    blob_key   TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
);

-- Tombstones of erased and purged content, in the format of core/backup's erasure ledger
-- (opaque subject, data class, date): can-core pulls them so a restore from an older backup
-- cannot bring the content back. Rows carry no personal data.
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

-- Hash-chained audit log: actor, action, resource id and field names, never names or content.
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

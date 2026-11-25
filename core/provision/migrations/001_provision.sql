-- can-provision schema (specs/028-identity-provision). Timestamps are unix seconds (UTC).
-- Nothing here holds personal data beyond opaque ids: names and emails are read from the people
-- directory at the moment they are sent to a downstream system and are never stored.

CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('joiner','mover','leaver')),
    party_id       TEXT NOT NULL,
    state          TEXT NOT NULL CHECK (state IN ('pending','running','scheduled','awaiting_approval','succeeded','failed','compensating','compensated','cancelled')),
    trigger        TEXT NOT NULL,
    idem_key       TEXT NOT NULL UNIQUE,
    urgent         INTEGER NOT NULL DEFAULT 0,
    dry_run        INTEGER NOT NULL DEFAULT 0,
    privileged     INTEGER NOT NULL DEFAULT 0,
    effective_date TEXT NOT NULL DEFAULT '',
    initiator      TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
-- One live run per person and kind: a redelivered or repeated trigger finds it instead of starting a second.
CREATE UNIQUE INDEX runs_live ON runs(party_id, kind) WHERE state IN ('pending','running','scheduled','awaiting_approval','compensating');
CREATE INDEX runs_party ON runs(party_id);
CREATE INDEX runs_state ON runs(state, updated_at);

CREATE TABLE steps (
    run_id       TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    ord          INTEGER NOT NULL,
    op           TEXT NOT NULL,
    connector    TEXT NOT NULL,
    arg          TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL CHECK (state IN ('pending','running','awaiting_approval','done','dead','skipped','rejected','comp_pending','compensated','comp_failed')),
    sensitive    INTEGER NOT NULL DEFAULT 0,
    optional     INTEGER NOT NULL DEFAULT 0,
    comp_op      TEXT NOT NULL DEFAULT '',
    attempts     INTEGER NOT NULL DEFAULT 0,
    not_before   INTEGER NOT NULL DEFAULT 0,
    next_at      INTEGER NOT NULL DEFAULT 0,
    approval_id  TEXT NOT NULL DEFAULT '',
    approved     INTEGER NOT NULL DEFAULT 0,
    external_id  TEXT NOT NULL DEFAULT '',
    error_code   TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, ord)
);

-- The downstream account of a person per connector, so a later disable names the right account.
CREATE TABLE accounts (
    party_id    TEXT NOT NULL,
    connector   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('active','disabled','deleted')),
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (party_id, connector)
);

-- The groups a person was added to at a connector (by name), so a mover or leaver removes exactly those.
CREATE TABLE applied_groups (
    party_id  TEXT NOT NULL,
    connector TEXT NOT NULL,
    grp       TEXT NOT NULL,
    PRIMARY KEY (party_id, connector, grp)
);

-- People whose termination was flagged urgent: access ends at once, not on the last day.
CREATE TABLE urgent (
    party_id   TEXT PRIMARY KEY,
    flagged_by TEXT NOT NULL,
    flagged_at INTEGER NOT NULL
);

-- Delivered event ids, so a redelivery after a restart is recognised.
CREATE TABLE seen_events (
    event_id TEXT PRIMARY KEY,
    seen_at  INTEGER NOT NULL
);

CREATE TABLE scim_tokens (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    hash       TEXT NOT NULL UNIQUE,
    scopes     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE groups (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    version      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE group_members (
    group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    party_id TEXT NOT NULL,
    PRIMARY KEY (group_id, party_id)
);
CREATE INDEX group_members_party ON group_members(party_id);

-- Events to publish through can-core (ids only), delivered at least once.
CREATE TABLE outbox (
    id           TEXT PRIMARY KEY,
    type         TEXT NOT NULL,
    data         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_at      INTEGER NOT NULL DEFAULT 0,
    delivered_at INTEGER NOT NULL DEFAULT 0
);

-- Hash-chained audit trail of every provisioning action: actor, action, target ids and counts only.
CREATE TABLE audit (
    seq       INTEGER PRIMARY KEY,
    time      INTEGER NOT NULL,
    actor     TEXT NOT NULL,
    action    TEXT NOT NULL,
    target    TEXT NOT NULL,
    detail    TEXT NOT NULL,
    prev_hash TEXT NOT NULL,
    hash      TEXT NOT NULL
);

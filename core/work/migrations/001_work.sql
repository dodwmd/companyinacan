-- Work tracker schema. Timestamps are unix seconds (UTC). An issue key is
-- "<projects.key>-<issues.number>"; the key is never stored on the issue row.
CREATE TABLE projects (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    key         TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    workflow    TEXT    NOT NULL,            -- JSON, see workflow.go
    sla         TEXT    NOT NULL,            -- JSON: per-priority response/resolution hints (hours)
    next_number INTEGER NOT NULL DEFAULT 1,
    archived    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE sprints (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    goal       TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'active', 'closed')),
    starts_on  TEXT,
    ends_on    TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX sprints_project ON sprints (project_id);
-- At most one active sprint per project.
CREATE UNIQUE INDEX sprints_one_active ON sprints (project_id) WHERE state = 'active';

CREATE TABLE issues (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    type        TEXT    NOT NULL CHECK (type IN ('epic', 'story', 'task', 'bug', 'subtask')),
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    priority    TEXT    NOT NULL CHECK (priority IN ('P0', 'P1', 'P2', 'P3')),
    status      TEXT    NOT NULL,
    reporter    TEXT    NOT NULL,
    assignee    TEXT,
    due_date    TEXT,                        -- YYYY-MM-DD
    estimate    REAL,                        -- story points or hours, the project decides
    parent_id   INTEGER REFERENCES issues (id) ON DELETE SET NULL,
    sprint_id   INTEGER REFERENCES sprints (id) ON DELETE SET NULL,
    rank        TEXT    NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    resolved_at INTEGER,
    UNIQUE (project_id, number)
);
CREATE INDEX issues_project_rank ON issues (project_id, rank);
CREATE INDEX issues_assignee     ON issues (assignee) WHERE assignee IS NOT NULL;
CREATE INDEX issues_parent       ON issues (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX issues_sprint       ON issues (sprint_id) WHERE sprint_id IS NOT NULL;
CREATE INDEX issues_resolved     ON issues (resolved_at) WHERE resolved_at IS NOT NULL;

CREATE TABLE issue_labels (
    issue_id INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    label    TEXT    NOT NULL,
    PRIMARY KEY (issue_id, label)
);
CREATE INDEX issue_labels_label ON issue_labels (label);

CREATE TABLE issue_links (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    type       TEXT    NOT NULL CHECK (type IN ('blocks', 'relates', 'duplicates')),
    source_id  INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    target_id  INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    created_by TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    CHECK (source_id <> target_id),
    UNIQUE (type, source_id, target_id)
);
CREATE INDEX issue_links_target ON issue_links (target_id);

CREATE TABLE comments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id   INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    author     TEXT    NOT NULL,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX comments_issue ON comments (issue_id, id);

CREATE TABLE activity (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    actor    TEXT    NOT NULL,
    kind     TEXT    NOT NULL,
    data     TEXT    NOT NULL DEFAULT '{}',
    at       INTEGER NOT NULL
);
CREATE INDEX activity_issue ON activity (issue_id, id);

CREATE TABLE watchers (
    issue_id INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    subject  TEXT    NOT NULL,
    PRIMARY KEY (issue_id, subject)
);

-- Metadata only: the tracker stores no file bytes yet.
CREATE TABLE attachments (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id     INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    filename     TEXT    NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL,
    sha256       TEXT    NOT NULL DEFAULT '',
    url          TEXT    NOT NULL DEFAULT '',
    added_by     TEXT    NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX attachments_issue ON attachments (issue_id);

-- Retries of the same create must not duplicate. Scoped per subject so one caller cannot
-- probe or collide with another's keys.
CREATE TABLE idempotency (
    subject      TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    request_hash TEXT    NOT NULL,
    response     TEXT    NOT NULL,
    status       INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (subject, key)
);

-- API tokens: only the SHA-256 of the secret is stored.
CREATE TABLE api_tokens (
    id           TEXT    PRIMARY KEY,
    kind         TEXT    NOT NULL CHECK (kind IN ('personal', 'service')),
    subject      TEXT    NOT NULL,
    name         TEXT    NOT NULL DEFAULT '',
    secret_hash  BLOB    NOT NULL,
    scopes       TEXT    NOT NULL,           -- JSON array of work.read/work.write/work.admin
    projects     TEXT    NOT NULL DEFAULT '[]', -- JSON array of project keys; empty = all
    created_by   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    revoked_at   INTEGER,
    last_used_at INTEGER
);

-- Admin actions (tokens, projects, workflows). Never holds secrets.
CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    actor  TEXT    NOT NULL,
    action TEXT    NOT NULL,
    target TEXT    NOT NULL DEFAULT '',
    detail TEXT    NOT NULL DEFAULT '{}',
    at     INTEGER NOT NULL
);

-- Transactional outbox for webhook delivery (written in the same transaction as the change).
CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id     TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL,
    payload      TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL,
    delivered_at INTEGER,
    last_error   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX outbox_due ON outbox (next_attempt) WHERE delivered_at IS NULL;
